package problem_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"digitalclone/pkg/problem"
)

func TestWriteRFC7807(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	problem.Write(
		recorder,
		http.StatusBadRequest,
		"https://digitalclone.local/problems/validation-error",
		"Validation Failed",
		"commit_sha is required",
		"/api/v1/triggers/commit",
	)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 status, got %d", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/problem+json" {
		t.Fatalf("expected application/problem+json, got %s", contentType)
	}

	var decoded problem.Details
	if err := json.NewDecoder(recorder.Body).Decode(&decoded); err != nil {
		t.Fatalf("failed to decode response JSON: %v", err)
	}
	if decoded.Title != "Validation Failed" || decoded.Detail != "commit_sha is required" || decoded.Status != 400 {
		t.Fatalf("unexpected problem payload: %+v", decoded)
	}
}
