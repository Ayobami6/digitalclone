// Package persona loads platform-specific Markdown persona skills from disk.
package persona

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"digitalclone/internal/domain"
)

// FilesystemPersonaLoader implements domain.PersonaLoader by reading Markdown files in personasDir.
type FilesystemPersonaLoader struct {
	personasDir string
}

// NewFilesystemPersonaLoader constructs a loader anchored to personasDir.
func NewFilesystemPersonaLoader(personasDir string) *FilesystemPersonaLoader {
	if strings.TrimSpace(personasDir) == "" {
		personasDir = "personas"
	}
	return &FilesystemPersonaLoader{personasDir: filepath.Clean(personasDir)}
}

// LoadPersona reads <personasDir>/<platform>.md safely, validating against path traversal.
func (loader *FilesystemPersonaLoader) LoadPersona(ctx context.Context, platform domain.Platform) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	parsedPlatform, err := domain.ParsePlatform(string(platform))
	if err != nil {
		return "", err
	}

	fileName := fmt.Sprintf("%s.md", parsedPlatform)
	resolvedPath := filepath.Join(loader.personasDir, fileName)
	cleanDir := filepath.Clean(loader.personasDir)
	if !strings.HasPrefix(filepath.Clean(resolvedPath), cleanDir+string(filepath.Separator)) && filepath.Clean(resolvedPath) != filepath.Join(cleanDir, fileName) {
		return "", fmt.Errorf("%w: unsafe persona path %s", domain.ErrValidationFailed, resolvedPath)
	}

	content, err := os.ReadFile(resolvedPath)
	if err != nil {
		return "", fmt.Errorf("read persona file %s: %w", resolvedPath, err)
	}
	return strings.TrimSpace(string(content)), nil
}
