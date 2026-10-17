package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gojp/goreportcard/vault"
)

func TestRunApexScrape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Sovereign-God-Key"); got != "test-owner-key" {
			t.Errorf("unexpected owner key header: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(vault.ApexScrapeResult{
			OK:       true,
			URL:      "https://example.org",
			FinalURL: "https://example.org",
			Title:    "Example",
		})
	}))
	defer server.Close()

	t.Setenv("ALPACORE_APEX_BASE_URL", server.URL)
	t.Setenv("SOVEREIGN_GOD_KEY", "test-owner-key")

	var output bytes.Buffer
	if err := runApexScrape(context.Background(), "https://example.org", &output); err != nil {
		t.Fatalf("runApexScrape returned error: %v", err)
	}

	var result vault.ApexScrapeResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode CLI result: %v", err)
	}
	if !result.OK || result.Title != "Example" {
		t.Fatalf("unexpected CLI result: %+v", result)
	}
}
