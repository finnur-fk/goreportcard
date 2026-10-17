package vault

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestScrapeApex(t *testing.T) {
	var received bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = true
		if r.Method != http.MethodPost || r.URL.Path != apexScrapePath {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("X-Sovereign-God-Key"); got != "test-owner-key" {
			t.Errorf("unexpected owner key header: %q", got)
		}
		var request struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if request.URL != "https://example.org" {
			t.Errorf("unexpected target URL: %q", request.URL)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ApexScrapeResult{
			OK:         true,
			URL:        request.URL,
			FinalURL:   request.URL,
			Title:      "Example",
			Text:       "Example text",
			Emails:     []string{"hello@example.org"},
			EmailCount: 1,
			Source:     "engine",
		})
	}))
	defer server.Close()

	t.Setenv("ALPACORE_APEX_BASE_URL", server.URL)
	t.Setenv("SOVEREIGN_GOD_KEY", "test-owner-key")

	result, err := ScrapeApex(context.Background(), "https://example.org")
	if err != nil {
		t.Fatalf("ScrapeApex returned error: %v", err)
	}
	if !received {
		t.Fatal("Apex endpoint did not receive a request")
	}
	if !result.OK || result.Title != "Example" || result.EmailCount != 1 {
		t.Fatalf("unexpected scrape result: %+v", result)
	}
}

func TestScrapeApexRequiresConfiguration(t *testing.T) {
	t.Setenv("ALPACORE_APEX_BASE_URL", "")
	t.Setenv("SOVEREIGN_GOD_KEY", "")

	_, err := ScrapeApex(context.Background(), "https://example.org")
	if err == nil || !strings.Contains(err.Error(), "ALPACORE_APEX_BASE_URL") {
		t.Fatalf("expected missing base URL error, got %v", err)
	}

	t.Setenv("ALPACORE_APEX_BASE_URL", "https://alpacore.net")
	_, err = ScrapeApex(context.Background(), "https://example.org")
	if err == nil || !strings.Contains(err.Error(), "SOVEREIGN_GOD_KEY") {
		t.Fatalf("expected missing owner key error, got %v", err)
	}
}

func TestScrapeApexRejectsInsecureRemoteBaseURL(t *testing.T) {
	t.Setenv("ALPACORE_APEX_BASE_URL", "http://alpacore.net")
	t.Setenv("SOVEREIGN_GOD_KEY", "test-owner-key")

	_, err := ScrapeApex(context.Background(), "https://example.org")
	if err == nil || !strings.Contains(err.Error(), "HTTPS URL") {
		t.Fatalf("expected insecure remote base URL error, got %v", err)
	}
}

func TestScrapeApexReportsHTTPAndDecodeErrors(t *testing.T) {
	for _, test := range []struct {
		name       string
		statusCode int
		body       string
		wantError  string
	}{
		{name: "unauthorized", statusCode: http.StatusUnauthorized, body: `{"message":"unauthorized"}`, wantError: "HTTP 401"},
		{name: "redirect", statusCode: http.StatusTemporaryRedirect, body: "", wantError: "HTTP 307"},
		{name: "malformed response", statusCode: http.StatusOK, body: `{`, wantError: "decode Apex scrape response"},
		{name: "scrape rejected", statusCode: http.StatusOK, body: `{"ok":false,"error":"url is invalid"}`, wantError: "Apex scrape failed: url is invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.statusCode)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			t.Setenv("ALPACORE_APEX_BASE_URL", server.URL)
			t.Setenv("SOVEREIGN_GOD_KEY", "test-owner-key")
			_, err := ScrapeApex(context.Background(), "https://example.org")
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("expected error containing %q, got %v", test.wantError, err)
			}
		})
	}
}
