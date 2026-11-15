// Package git implements domain.CommitInspector for local git repositories
// and GitHub REST API commit diff retrieval.
package git

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"digitalclone/internal/domain"
	"digitalclone/pkg/resilience"
)

const defaultGitHubAPIBase = "https://api.github.com"

// InspectorConfig configures GitHub API authentication and timeouts.
type InspectorConfig struct {
	GitHubToken    string
	GitHubAPIBase  string
	RequestTimeout time.Duration
	HTTPClient     *http.Client
}

// CommitInspector implements domain.CommitInspector.
type CommitInspector struct {
	githubToken    string
	githubAPIBase  string
	requestTimeout time.Duration
	httpClient     *http.Client
	breaker        *resilience.CircuitBreaker
}

// NewCommitInspector constructs a CommitInspector adapter.
func NewCommitInspector(config InspectorConfig) *CommitInspector {
	apiBase := strings.TrimRight(strings.TrimSpace(config.GitHubAPIBase), "/")
	if apiBase == "" {
		apiBase = defaultGitHubAPIBase
	}
	timeout := config.RequestTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}
	return &CommitInspector{
		githubToken:    strings.TrimSpace(config.GitHubToken),
		githubAPIBase:  apiBase,
		requestTimeout: timeout,
		httpClient:     httpClient,
		breaker: resilience.NewCircuitBreaker(resilience.CircuitBreakerConfig{
			Name:         "github-rest-api",
			MaxFailures:  5,
			ResetTimeout: 30 * time.Second,
		}),
	}
}

// InspectLocalCommit reads commit metadata, changed files, and patch diff from a local git repository.
func (inspector *CommitInspector) InspectLocalCommit(ctx context.Context, repoPath, commitRef string) (*domain.CommitEvent, error) {
	if strings.TrimSpace(repoPath) == "" {
		repoPath = "."
	}
	if strings.TrimSpace(commitRef) == "" {
		commitRef = "HEAD"
	}

	absPath, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve repo path: %w", err)
	}

	// Unit-separator delimited format makes commit message newlines deterministic to parse.
	metaOutput, err := runGitCommand(ctx, absPath, "log", "-1", "--format=%H%x1f%an%x1f%cI%x1f%B", commitRef)
	if err != nil {
		return nil, fmt.Errorf("inspect git commit metadata: %w", err)
	}
	parts := strings.SplitN(metaOutput, "\x1f", 4)
	if len(parts) < 4 {
		return nil, fmt.Errorf("unexpected git log output format")
	}

	commitSHA := strings.TrimSpace(parts[0])
	author := strings.TrimSpace(parts[1])
	timestamp, _ := time.Parse(time.RFC3339, strings.TrimSpace(parts[2]))
	message := strings.TrimSpace(parts[3])

	branchName, err := runGitCommand(ctx, absPath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		branchName = "main"
	}

	filesOutput, err := runGitCommand(ctx, absPath, "diff-tree", "--no-commit-id", "--name-only", "-r", "--root", commitSHA)
	if err != nil {
		return nil, fmt.Errorf("list changed files: %w", err)
	}
	var changedFiles []string
	for _, line := range strings.Split(filesOutput, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			changedFiles = append(changedFiles, trimmed)
		}
	}

	diffPatch, err := runGitCommand(ctx, absPath, "show", "--no-color", "--format=", "--patch", commitSHA)
	if err != nil {
		return nil, fmt.Errorf("read git patch: %w", err)
	}

	event := &domain.CommitEvent{
		RepoName:     filepath.Base(absPath),
		Branch:       strings.TrimSpace(branchName),
		CommitSHA:    commitSHA,
		Author:       author,
		Message:      message,
		Timestamp:    timestamp,
		ChangedFiles: changedFiles,
		DiffPatch:    diffPatch,
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	return event, nil
}

// FetchGitHubCommit retrieves commit metadata and unified diff patch via GitHub's REST API.
func (inspector *CommitInspector) FetchGitHubCommit(ctx context.Context, repoFullName, commitSHA string) (*domain.CommitEvent, error) {
	if strings.TrimSpace(repoFullName) == "" || strings.TrimSpace(commitSHA) == "" {
		return nil, fmt.Errorf("%w: repoFullName and commitSHA are required", domain.ErrValidationFailed)
	}

	var event *domain.CommitEvent
	err := inspector.breaker.Execute(ctx, func(callCtx context.Context) error {
		reqCtx, cancel := context.WithTimeout(callCtx, inspector.requestTimeout)
		defer cancel()

		commitURL := fmt.Sprintf("%s/repos/%s/commits/%s", inspector.githubAPIBase, strings.Trim(repoFullName, "/"), commitSHA)

		var commitResp struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
				Author  struct {
					Name string    `json:"name"`
					Date time.Time `json:"date"`
				} `json:"author"`
			} `json:"commit"`
			Files []struct {
				Filename string `json:"filename"`
			} `json:"files"`
		}
		metaBytes, err := inspector.doGitHubGet(reqCtx, commitURL, "application/vnd.github+json")
		if err != nil {
			return fmt.Errorf("fetch github commit metadata: %w", err)
		}
		if err := json.Unmarshal(metaBytes, &commitResp); err != nil {
			return fmt.Errorf("decode github commit metadata: %w", err)
		}

		diffBytes, err := inspector.doGitHubGet(reqCtx, commitURL, "application/vnd.github.v3.diff")
		if err != nil {
			return fmt.Errorf("fetch github commit diff: %w", err)
		}

		changedFiles := make([]string, 0, len(commitResp.Files))
		for _, file := range commitResp.Files {
			changedFiles = append(changedFiles, file.Filename)
		}

		candidate := &domain.CommitEvent{
			RepoName:     repoFullName,
			CommitSHA:    commitResp.SHA,
			Author:       commitResp.Commit.Author.Name,
			Message:      commitResp.Commit.Message,
			Timestamp:    commitResp.Commit.Author.Date,
			ChangedFiles: changedFiles,
			DiffPatch:    string(diffBytes),
		}
		if validateErr := candidate.Validate(); validateErr != nil {
			return validateErr
		}
		event = candidate
		return nil
	})
	return event, err
}

func (inspector *CommitInspector) doGitHubGet(ctx context.Context, targetURL, acceptHeader string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", acceptHeader)
	req.Header.Set("User-Agent", "DigitalClone/1.0")
	if inspector.githubToken != "" {
		req.Header.Set("Authorization", "Bearer "+inspector.githubToken)
	}

	resp, err := inspector.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if readErr != nil {
		return nil, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github API HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}
	return bodyBytes, nil
}

func runGitCommand(ctx context.Context, repoPath string, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", repoPath}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s failed (%w): %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
