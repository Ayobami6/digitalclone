// Package problem implements RFC 7807 Problem Details JSON HTTP responses.
package problem

import (
	"encoding/json"
	"net/http"
)

// Details conforms to the RFC 7807 Problem Details specification for HTTP APIs.
type Details struct {
	Type       string         `json:"type"`
	Title      string         `json:"title"`
	Status     int            `json:"status"`
	Detail     string         `json:"detail,omitempty"`
	Instance   string         `json:"instance,omitempty"`
	Extensions map[string]any `json:"extensions,omitempty"`
}

// Write encodes an RFC 7807 problem payload with Content-Type: application/problem+json.
func Write(writer http.ResponseWriter, status int, problemType, title, detail, instance string) {
	if problemType == "" {
		problemType = "about:blank"
	}
	payload := Details{
		Type:     problemType,
		Title:    title,
		Status:   status,
		Detail:   detail,
		Instance: instance,
	}
	writer.Header().Set("Content-Type", "application/problem+json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}
