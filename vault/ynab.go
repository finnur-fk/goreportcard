package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultYNABBaseURL = "https://api.ynab.com/v1"
	defaultYNABTimeout = 30 * time.Second
	// ynabLastUsedBudget is the alias YNAB accepts in place of a budget UUID.
	ynabLastUsedBudget = "last-used"
	// ynabMaxRetries bounds the retries performed for rate limited or temporary failures.
	ynabMaxRetries = 3
	// ynabRetryBackoff is the base delay used for exponential backoff between retries.
	ynabRetryBackoff = 2 * time.Second
	// ynabMaxBodyBytes caps how much of a YNAB response body is read into memory.
	ynabMaxBodyBytes = 1 << 20
)

// YNABConfig holds the Personal Access Token settings used to reach the YNAB API.
// The token itself is never logged or rendered; only its presence is reported.
type YNABConfig struct {
	// APIKey is the YNAB Personal Access Token (YNAB_API_KEY). Treat as a secret.
	APIKey string
	// BudgetID is the optional budget UUID or the "last-used" alias (YNAB_BUDGET_ID).
	BudgetID string
	// BaseURL is the YNAB REST root, defaulting to https://api.ynab.com/v1.
	BaseURL string
	// AccountMap maps AlpaCore account identifiers to YNAB account UUIDs or names.
	AccountMap map[string]string
	// Timeout bounds each individual HTTP request.
	Timeout time.Duration
	// Enabled reports whether the YNAB bridge should run at all.
	Enabled bool
}

// LoadYNABConfig reads the YNAB Personal Access Token variables from the environment.
// It mirrors the precedence and toggle conventions used by LoadWebhookConfig.
func LoadYNABConfig() YNABConfig {
	apiKey := firstNonEmpty(os.Getenv("YNAB_API_KEY"))
	budgetID := firstNonEmpty(os.Getenv("YNAB_BUDGET_ID"))
	baseURL := firstNonEmpty(os.Getenv("YNAB_BASE_URL"), defaultYNABBaseURL)

	timeout := defaultYNABTimeout
	if raw := strings.TrimSpace(os.Getenv("YNAB_TIMEOUT_SECONDS")); raw != "" {
		if sec, err := strconv.Atoi(raw); err == nil && sec > 0 {
			timeout = time.Duration(sec) * time.Second
		}
	}

	enabled := apiKey != ""
	switch strings.ToLower(strings.TrimSpace(os.Getenv("YNAB_ENABLED"))) {
	case "false", "0", "no", "off":
		enabled = false
	}

	return YNABConfig{
		APIKey:     apiKey,
		BudgetID:   budgetID,
		BaseURL:    strings.TrimRight(baseURL, "/"),
		AccountMap: parseYNABAccountMap(os.Getenv("YNAB_ACCOUNT_MAP")),
		Timeout:    timeout,
		Enabled:    enabled,
	}
}

// Validate fails fast when the token is missing or the base URL cannot carry a secret.
// The token value is never included in the returned error.
func (c YNABConfig) Validate() error {
	if c.APIKey == "" {
		return fmt.Errorf("YNAB_API_KEY is not set: export the Personal Access Token before bootstrapping")
	}
	if err := validateYNABBaseURL(c.BaseURL); err != nil {
		return fmt.Errorf("YNAB_BASE_URL invalid: %w", err)
	}
	return nil
}

// validateYNABBaseURL rejects any base URL that would leak the bearer token in clear text.
// Plain HTTP is only tolerated for loopback hosts so tests can use httptest servers.
func validateYNABBaseURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	if parsed.Host == "" {
		return fmt.Errorf("missing host in %q", raw)
	}

	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(parsed.Host) {
			return nil
		}
		return fmt.Errorf("refusing to send the access token over plain http to %q", parsed.Host)
	default:
		return fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}
}

func isLoopbackHost(host string) bool {
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(hostname, "[]"))
	return ip != nil && ip.IsLoopback()
}

// parseYNABAccountMap parses "alpacore-id=ynab-account,other=Account Name" pairs.
func parseYNABAccountMap(raw string) map[string]string {
	mapping := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		key, value, found := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !found || key == "" || value == "" {
			continue
		}
		mapping[key] = value
	}
	return mapping
}

// YNABErrorKind classifies YNAB API failures so callers can react to each case.
type YNABErrorKind string

const (
	// YNABErrorAuth means the token is wrong, revoked or expired (HTTP 401).
	YNABErrorAuth YNABErrorKind = "auth"
	// YNABErrorForbidden means the token is valid but lacks access (HTTP 403).
	YNABErrorForbidden YNABErrorKind = "forbidden"
	// YNABErrorNotFound means the budget or account does not exist (HTTP 404).
	YNABErrorNotFound YNABErrorKind = "not_found"
	// YNABErrorRateLimit means the 200 requests/hour quota is exhausted (HTTP 429).
	YNABErrorRateLimit YNABErrorKind = "rate_limit"
	// YNABErrorTemporary covers 5xx responses and transport failures.
	YNABErrorTemporary YNABErrorKind = "temporary"
	// YNABErrorRequest covers remaining 4xx responses.
	YNABErrorRequest YNABErrorKind = "request"
)

// YNABError is a classified YNAB API failure carrying the HTTP status and a redacted detail.
type YNABError struct {
	Kind       YNABErrorKind
	StatusCode int
	Endpoint   string
	Detail     string
}

// Error implements the error interface without ever exposing the access token.
func (e *YNABError) Error() string {
	msg := fmt.Sprintf("ynab %s: %s", e.Endpoint, ynabKindMessage(e.Kind))
	if e.StatusCode > 0 {
		msg = fmt.Sprintf("%s (status %d)", msg, e.StatusCode)
	}
	if e.Detail != "" {
		msg = fmt.Sprintf("%s: %s", msg, e.Detail)
	}
	return msg
}

// Retryable reports whether the request may succeed if attempted again later.
func (e *YNABError) Retryable() bool {
	return e.Kind == YNABErrorRateLimit || e.Kind == YNABErrorTemporary
}

func ynabKindMessage(kind YNABErrorKind) string {
	switch kind {
	case YNABErrorAuth:
		return "access token rejected, rotate YNAB_API_KEY"
	case YNABErrorForbidden:
		return "access token lacks permission for this resource"
	case YNABErrorNotFound:
		return "resource not found"
	case YNABErrorRateLimit:
		return "rate limit reached (200 requests/hour per token)"
	case YNABErrorTemporary:
		return "temporary upstream failure"
	default:
		return "request rejected"
	}
}

func ynabErrorKind(status int) YNABErrorKind {
	switch {
	case status == http.StatusUnauthorized:
		return YNABErrorAuth
	case status == http.StatusForbidden:
		return YNABErrorForbidden
	case status == http.StatusNotFound:
		return YNABErrorNotFound
	case status == http.StatusTooManyRequests:
		return YNABErrorRateLimit
	case status >= 500:
		return YNABErrorTemporary
	default:
		return YNABErrorRequest
	}
}

// YNABClient performs authenticated, rate-limit aware calls against the YNAB REST API.
type YNABClient struct {
	cfg       YNABConfig
	http      *http.Client
	logger    *log.Logger
	knowledge map[string]int64
	rateLimit string
	backoff   time.Duration
}

// NewYNABClient builds a client for the supplied configuration.
// It returns an error when the configuration cannot safely carry the access token.
func NewYNABClient(cfg YNABConfig, logger *log.Logger) (*YNABClient, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = log.New(os.Stdout, "[YNAB] ", log.LstdFlags)
	}

	return &YNABClient{
		cfg:       cfg,
		http:      &http.Client{Timeout: cfg.Timeout},
		logger:    logger,
		knowledge: map[string]int64{},
		backoff:   ynabRetryBackoff,
	}, nil
}

// RateLimit returns the most recent X-Rate-Limit header value ("used/limit"), if any.
func (c *YNABClient) RateLimit() string {
	return c.rateLimit
}

// ServerKnowledge returns the delta-sync cursor stored for an endpoint path.
func (c *YNABClient) ServerKnowledge(path string) int64 {
	return c.knowledge[path]
}

// SetServerKnowledge seeds the delta-sync cursor for an endpoint path.
func (c *YNABClient) SetServerKnowledge(path string, knowledge int64) {
	c.knowledge[path] = knowledge
}

// ynabEnvelope is the common YNAB response wrapper.
type ynabEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Detail string `json:"detail"`
	} `json:"error"`
}

// get performs an authenticated GET, decodes the "data" envelope into out and
// stores any returned server_knowledge cursor for the next delta-sync call.
func (c *YNABClient) get(ctx context.Context, path string, out interface{}) error {
	var lastErr error

	for attempt := 0; attempt <= ynabMaxRetries; attempt++ {
		if attempt > 0 {
			if err := ynabWait(ctx, c.backoff<<uint(attempt-1)); err != nil {
				return err
			}
			c.logger.Printf("Retrying %s (attempt %d/%d)", path, attempt+1, ynabMaxRetries+1)
		}

		data, err := c.do(ctx, path)
		if err == nil {
			return c.decode(path, data, out)
		}

		lastErr = err
		var apiErr *YNABError
		if !errors.As(err, &apiErr) || !apiErr.Retryable() {
			return err
		}
	}

	return lastErr
}

// do issues a single request and returns the raw "data" payload.
func (c *YNABClient) do(ctx context.Context, path string) (json.RawMessage, error) {
	endpoint := c.cfg.BaseURL + path
	if knowledge := c.knowledge[path]; knowledge > 0 {
		endpoint = ynabAppendKnowledge(endpoint, knowledge)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create request for %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &YNABError{
			Kind:     YNABErrorTemporary,
			Endpoint: path,
			Detail:   c.redact(err.Error()),
		}
	}
	defer resp.Body.Close()

	if limit := resp.Header.Get("X-Rate-Limit"); limit != "" {
		c.rateLimit = limit
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, ynabMaxBodyBytes))
	if err != nil {
		return nil, &YNABError{
			Kind:       YNABErrorTemporary,
			StatusCode: resp.StatusCode,
			Endpoint:   path,
			Detail:     c.redact(err.Error()),
		}
	}

	var envelope ynabEnvelope
	decodeErr := json.Unmarshal(body, &envelope)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := strings.TrimSpace(string(body))
		if decodeErr == nil && envelope.Error != nil {
			detail = strings.TrimSpace(envelope.Error.Detail)
		}
		return nil, &YNABError{
			Kind:       ynabErrorKind(resp.StatusCode),
			StatusCode: resp.StatusCode,
			Endpoint:   path,
			Detail:     c.redact(truncate(detail, 200)),
		}
	}

	if decodeErr != nil {
		return nil, fmt.Errorf("decode response for %s: %w", path, decodeErr)
	}

	return envelope.Data, nil
}

// decode unmarshals the data payload and records the delta-sync cursor.
func (c *YNABClient) decode(path string, data json.RawMessage, out interface{}) error {
	if len(data) == 0 {
		return fmt.Errorf("empty response for %s", path)
	}

	var cursor struct {
		ServerKnowledge int64 `json:"server_knowledge"`
	}
	if err := json.Unmarshal(data, &cursor); err == nil && cursor.ServerKnowledge > 0 {
		c.knowledge[path] = cursor.ServerKnowledge
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response for %s: %w", path, err)
	}
	return nil
}

// redact removes the access token from any string before it reaches a log or error.
func (c *YNABClient) redact(s string) string {
	if c.cfg.APIKey == "" {
		return s
	}
	return strings.ReplaceAll(s, c.cfg.APIKey, "[REDACTED]")
}

func ynabAppendKnowledge(endpoint string, knowledge int64) string {
	separator := "?"
	if strings.Contains(endpoint, "?") {
		separator = "&"
	}
	return fmt.Sprintf("%s%slast_knowledge_of_server=%d", endpoint, separator, knowledge)
}

func ynabWait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// YNABUser is the identity returned by GET /user.
type YNABUser struct {
	ID string `json:"id"`
}

// YNABBudget is a budget summary returned by GET /budgets.
type YNABBudget struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	LastModifiedOn string `json:"last_modified_on"`
	CurrencyFormat *struct {
		ISOCode string `json:"iso_code"`
	} `json:"currency_format"`
}

// Currency returns the budget ISO currency code, or an empty string when unknown.
func (b YNABBudget) Currency() string {
	if b.CurrencyFormat == nil {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(b.CurrencyFormat.ISOCode))
}

// YNABAccount is an account returned by GET /budgets/{id}/accounts.
type YNABAccount struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Closed  bool   `json:"closed"`
	Deleted bool   `json:"deleted"`
	Balance int64  `json:"balance"`
}

// User calls GET /user and confirms that the Personal Access Token is live.
func (c *YNABClient) User(ctx context.Context) (YNABUser, error) {
	var payload struct {
		User YNABUser `json:"user"`
	}
	if err := c.get(ctx, "/user", &payload); err != nil {
		return YNABUser{}, err
	}
	return payload.User, nil
}

// Budgets calls GET /budgets and returns every budget the token can reach.
func (c *YNABClient) Budgets(ctx context.Context) ([]YNABBudget, error) {
	var payload struct {
		Budgets []YNABBudget `json:"budgets"`
	}
	if err := c.get(ctx, "/budgets", &payload); err != nil {
		return nil, err
	}
	return payload.Budgets, nil
}

// BudgetCurrency calls GET /budgets/{id}/settings to resolve the ISO currency code.
// It is the only way to learn the currency behind the "last-used" alias.
func (c *YNABClient) BudgetCurrency(ctx context.Context, budgetID string) (string, error) {
	var payload struct {
		Settings struct {
			CurrencyFormat *struct {
				ISOCode string `json:"iso_code"`
			} `json:"currency_format"`
		} `json:"settings"`
	}
	path := fmt.Sprintf("/budgets/%s/settings", url.PathEscape(budgetID))
	if err := c.get(ctx, path, &payload); err != nil {
		return "", err
	}
	if payload.Settings.CurrencyFormat == nil {
		return "", nil
	}
	return strings.ToUpper(strings.TrimSpace(payload.Settings.CurrencyFormat.ISOCode)), nil
}

// ynabAccountsPath builds the accounts endpoint path used for requests and delta-sync keys.
func ynabAccountsPath(budgetID string) string {
	return fmt.Sprintf("/budgets/%s/accounts", url.PathEscape(budgetID))
}

// Accounts calls GET /budgets/{id}/accounts and drops closed or deleted accounts.
func (c *YNABClient) Accounts(ctx context.Context, budgetID string) ([]YNABAccount, error) {
	var payload struct {
		Accounts []YNABAccount `json:"accounts"`
	}
	if err := c.get(ctx, ynabAccountsPath(budgetID), &payload); err != nil {
		return nil, err
	}

	open := make([]YNABAccount, 0, len(payload.Accounts))
	for _, account := range payload.Accounts {
		if account.Closed || account.Deleted {
			continue
		}
		open = append(open, account)
	}
	return open, nil
}
