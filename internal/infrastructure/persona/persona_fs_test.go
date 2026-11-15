package persona_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"digitalclone/internal/domain"
	"digitalclone/internal/infrastructure/persona"
)

func TestFilesystemPersonaLoader(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	want := "# LinkedIn Engineering Persona\nWrite crisp engineering stories."
	if err := os.WriteFile(filepath.Join(tempDir, "linkedin.md"), []byte(want), 0o600); err != nil {
		t.Fatalf("write persona fixture: %v", err)
	}

	loader := persona.NewFilesystemPersonaLoader(tempDir)
	got, err := loader.LoadPersona(context.Background(), domain.PlatformLinkedIn)
	if err != nil {
		t.Fatalf("LoadPersona failed: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
