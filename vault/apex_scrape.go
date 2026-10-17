package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultApexScrapeTimeout = 30 * time.Second
	apexScrapePath           = "/api/apex/scrape"
)

// ApexScrapeResult is the read-only research result returned by AlpaCore Apex.
type ApexScrapeResult struct {
	OK         bool     `json:"ok"`
	URL        string   `json:"url"`
	FinalURL   string   `json:"finalUrl"`
	Title      string   `json:"title"`
	Text       string   `json:"text"`
	Emails     []string `json:"emails"`
	EmailCount int      `json:"emailCount"`
	Error      string   `json:"error"`
	Source     string   `json:"source"`
}

// ScrapeApex calls the owner-gated Apex read-only scrape endpoint.
// Configure ALPACORE_APEX_BASE_URL (for example, https://alpacore.net) and
// SOVEREIGN_GOD_KEY in the environment. It never logs credentials or page data.
func ScrapeApex(ctx context.Context, targetURL string) (ApexScrapeResult, error) {
	if ctx == nil {
		return ApexScrapeResult{}, fmt.Errorf("request context is required")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("ALPACORE_APEX_BASE_URL")), "/")
	if baseURL == "" {
		return ApexScrapeResult{}, fmt.Errorf("ALPACORE_APEX_BASE_URL is not configured")
	}
	parsedBaseURL, err := url.Parse(baseURL)
	if err != nil || parsedBaseURL.Hostname() == "" || parsedBaseURL.User != nil ||
		(parsedBaseURL.Scheme != "https" && !(parsedBaseURL.Scheme == "http" && isLoopbackHost(parsedBaseURL.Host))) ||
		parsedBaseURL.RawQuery != "" || parsedBaseURL.Fragment != "" {
		return ApexScrapeResult{}, fmt.Errorf("ALPACORE_APEX_BASE_URL must be an HTTPS URL (HTTP is allowed only for loopback tests)")
	}
	ownerKey := strings.TrimSpace(os.Getenv("SOVEREIGN_GOD_KEY"))
	if ownerKey == "" {
		return ApexScrapeResult{}, fmt.Errorf("SOVEREIGN_GOD_KEY is not configured")
	}
	if strings.TrimSpace(targetURL) == "" {
		return ApexScrapeResult{}, fmt.Errorf("target URL is required")
	}

	payload, err := json.Marshal(struct {
		URL string `json:"url"`
	}{URL: targetURL})
	if err != nil {
		return ApexScrapeResult{}, fmt.Errorf("marshal Apex scrape request: %w", err)
	}

	endpoint := baseURL + apexScrapePath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return ApexScrapeResult{}, fmt.Errorf("create Apex scrape request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Sovereign-God-Key", ownerKey)

	client := &http.Client{
		Timeout: defaultApexScrapeTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return ApexScrapeResult{}, fmt.Errorf("call Apex scrape endpoint: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ApexScrapeResult{}, fmt.Errorf("read Apex scrape response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return ApexScrapeResult{}, fmt.Errorf("Apex scrape returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result ApexScrapeResult
	if err := json.Unmarshal(body, &result); err != nil {
		return ApexScrapeResult{}, fmt.Errorf("decode Apex scrape response: %w", err)
	}
	if !result.OK {
		if result.Error == "" {
			return result, fmt.Errorf("Apex scrape failed without an error message")
		}
		return result, fmt.Errorf("Apex scrape failed: %s", result.Error)
	}
	return result, nil
}
