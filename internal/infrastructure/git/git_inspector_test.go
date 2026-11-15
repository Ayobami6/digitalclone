package git_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"digitalclone/internal/infrastructure/git"
)

func TestInspectLocalCommitAndFetchGitHubCommit(t *testing.T) {
	t.Parallel()

	t.Run("inspect local git commit", func(t *testing.T) {
		t.Parallel()
		repoDir := t.TempDir()
		runGit(t, repoDir, "init", "-b", "main")
		runGit(t, repoDir, "config", "user.name", "Ayobami")
		runGit(t, repoDir, "config", "user.email", "ayo@example.com")

		filePath := filepath.Join(repoDir, "hello.go")
		if err := os.WriteFile(filePath, []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
			t.Fatalf("write sample file: %v", err)
		}
		runGit(t, repoDir, "add", "hello.go")
		runGit(t, repoDir, "commit", "-m", "feat: bootstrap digitalclone")

		inspector := git.NewCommitInspector(git.InspectorConfig{})
		event, err := inspector.InspectLocalCommit(context.Background(), repoDir, "HEAD")
		if err != nil {
			t.Fatalf("InspectLocalCommit failed: %v", err)
		}
		if event.Author != "Ayobami" || event.Message != "feat: bootstrap digitalclone" {
			t.Fatalf("unexpected commit event: %+v", event)
		}
		if len(event.ChangedFiles) != 1 || event.ChangedFiles[0] != "hello.go" {
			t.Fatalf("unexpected changed files: %+v", event.ChangedFiles)
		}
		if !strings.Contains(event.DiffPatch, "+func main() {}") {
			t.Fatalf("expected diff patch to include added code, got: %s", event.DiffPatch)
		}
	})

	t.Run("fetch GitHub commit metadata and diff", func(t *testing.T) {
		t.Parallel()
		httpClient := newTestHTTPClient(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Accept") == "application/vnd.github.v3.diff" {
				_, _ = w.Write([]byte("diff --git a/main.go b/main.go\n+println(\"ok\")"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sha":"abc987","commit":{"message":"feat: webhook","author":{"name":"Ayo","date":"2026-11-14T10:00:00Z"}},"files":[{"filename":"main.go"}]}`))
		})

		inspector := git.NewCommitInspector(git.InspectorConfig{
			GitHubToken:   "ghp_test",
			GitHubAPIBase: "https://api.github.test",
			HTTPClient:    httpClient,
		})
		event, err := inspector.FetchGitHubCommit(context.Background(), "Ayobami6/digitalclone", "abc987")
		if err != nil {
			t.Fatalf("FetchGitHubCommit failed: %v", err)
		}
		if event.CommitSHA != "abc987" || !strings.Contains(event.DiffPatch, `println("ok")`) {
			t.Fatalf("unexpected GitHub commit event: %+v", event)
		}
	})
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed (%v): %s", args, err, string(out))
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func newTestHTTPClient(handler http.HandlerFunc) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Body != nil {
				defer func() { _ = req.Body.Close() }()
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			resp := recorder.Result()
			resp.Request = req
			return resp, nil
		}),
	}
}
