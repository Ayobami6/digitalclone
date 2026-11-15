package genai_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"digitalclone/internal/infrastructure/genai"
)

func TestGenAIAdapterDryRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	adapter, err := genai.NewAdapter(ctx, genai.Config{DryRun: true})
	if err != nil {
		t.Fatalf("NewAdapter failed: %v", err)
	}

	text, err := adapter.GenerateText(ctx, "System persona", "Platform: linkedin\nCommit Message: feat: add ADK agent")
	if err != nil {
		t.Fatalf("GenerateText failed: %v", err)
	}
	if !strings.Contains(text, "LINKEDIN") || !strings.Contains(text, "feat: add ADK agent") {
		t.Fatalf("unexpected dry-run text output: %s", text)
	}

	imgBytes, imgMime, err := adapter.GenerateImage(ctx, "diagram", "16:9")
	if err != nil {
		t.Fatalf("GenerateImage failed: %v", err)
	}
	if imgMime != "image/png" || len(imgBytes) < 8 || !bytes.HasPrefix(imgBytes, []byte{0x89, 'P', 'N', 'G'}) {
		t.Fatalf("expected valid PNG header, got mime=%s len=%d", imgMime, len(imgBytes))
	}

	vidBytes, vidMime, err := adapter.GenerateVideo(ctx, "demo clip", "9:16")
	if err != nil {
		t.Fatalf("GenerateVideo failed: %v", err)
	}
	if vidMime != "video/mp4" || !bytes.Contains(vidBytes, []byte("ftyp")) {
		t.Fatalf("expected valid MP4 ftyp header, got mime=%s len=%d", vidMime, len(vidBytes))
	}
}
