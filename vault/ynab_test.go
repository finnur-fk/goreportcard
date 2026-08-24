package vault

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testYNABToken is a fake Personal Access Token used only by the test suite.
const testYNABToken = "test-token-not-a-real-secret"

// clearYNABEnv removes every YNAB variable so tests never inherit real credentials.
func clearYNABEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"YNAB_API_KEY",
		"YNAB_BUDGET_ID",
		"YNAB_BASE_URL",
		"YNAB_TIMEOUT_SECONDS",
		"YNAB_ENABLED",
		"YNAB_ACCOUNT_MAP",
	} {
		t.Setenv(key, "")
	}
}

// newTestYNABClient builds a client pointed at a test server with negligible backoff.
func newTestYNABClient(t *testing.T, baseURL string, cfg YNABConfig) *YNABClient {
	t.Helper()

	cfg.BaseURL = baseURL
	if cfg.APIKey == "" {
		cfg.APIKey = testYNABToken
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}

	client, err := NewYNABClient(cfg, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	client.backoff = time.Millisecond

	return client
}

func TestLoadYNABConfigDefaults(t *testing.T) {
	clearYNABEnv(t)
	t.Setenv("YNAB_API_KEY", testYNABToken)

	cfg := LoadYNABConfig()
	if cfg.BaseURL != defaultYNABBaseURL {
		t.Fatalf("unexpected base url: %q", cfg.BaseURL)
	}
	if cfg.Timeout != defaultYNABTimeout {
		t.Fatalf("unexpected timeout: %s", cfg.Timeout)
	}
	if !cfg.Enabled {
		t.Fatal("expected enabled when the token is present")
	}
	if cfg.BudgetID != "" {
		t.Fatalf("unexpected budget id: %q", cfg.BudgetID)
	}
}

func TestLoadYNABConfigOverrides(t *testing.T) {
	clearYNABEnv(t)
	t.Setenv("YNAB_API_KEY", testYNABToken)
	t.Setenv("YNAB_BUDGET_ID", "budget-1")
	t.Setenv("YNAB_BASE_URL", "https://api.ynab.example/v1/")
	t.Setenv("YNAB_TIMEOUT_SECONDS", "12")
	t.Setenv("YNAB_ACCOUNT_MAP", "paypal-processor=PayPal Business, stripe = Stripe ")

	cfg := LoadYNABConfig()
	if cfg.BaseURL != "https://api.ynab.example/v1" {
		t.Fatalf("trailing slash not trimmed: %q", cfg.BaseURL)
	}
	if cfg.Timeout != 12*time.Second {
		t.Fatalf("unexpected timeout: %s", cfg.Timeout)
	}
	if cfg.AccountMap["paypal-processor"] != "PayPal Business" {
		t.Fatalf("unexpected account map: %#v", cfg.AccountMap)
	}
	if cfg.AccountMap["stripe"] != "Stripe" {
		t.Fatalf("unexpected account map: %#v", cfg.AccountMap)
	}
}

func TestLoadYNABConfigDisabled(t *testing.T) {
	clearYNABEnv(t)
	t.Setenv("YNAB_API_KEY", testYNABToken)
	t.Setenv("YNAB_ENABLED", "false")

	if LoadYNABConfig().Enabled {
		t.Fatal("expected disabled")
	}
}

func TestYNABConfigValidateMissingKey(t *testing.T) {
	err := YNABConfig{BaseURL: defaultYNABBaseURL}.Validate()
	if err == nil {
		t.Fatal("expected an error when the token is missing")
	}
	if !strings.Contains(err.Error(), "YNAB_API_KEY") {
		t.Fatalf("error should name the variable: %v", err)
	}
}

func TestYNABConfigValidateRejectsPlainHTTP(t *testing.T) {
	cfg := YNABConfig{APIKey: testYNABToken, BaseURL: "http://api.ynab.example/v1"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected plain http to be rejected")
	}
	if strings.Contains(err.Error(), testYNABToken) {
		t.Fatal("error must never contain the token")
	}
}

func TestYNABConfigValidateAllowsLoopbackHTTP(t *testing.T) {
	for _, base := range []string{"http://127.0.0.1:8080/v1", "http://localhost:9000", "https://api.ynab.com/v1"} {
		cfg := YNABConfig{APIKey: testYNABToken, BaseURL: base}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("base %q should be accepted: %v", base, err)
		}
	}
}

func TestYNABUserHealthCheck(t *testing.T) {
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("X-Rate-Limit", "3/200")
		_, _ = w.Write([]byte(`{"data":{"user":{"id":"user-123"}}}`))
	}))
	defer server.Close()

	client := newTestYNABClient(t, server.URL, YNABConfig{})
	user, err := client.User(context.Background())
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if user.ID != "user-123" {
		t.Fatalf("unexpected user: %#v", user)
	}
	if auth != "Bearer "+testYNABToken {
		t.Fatalf("unexpected authorization header: %q", auth)
	}
	if client.RateLimit() != "3/200" {
		t.Fatalf("rate limit not captured: %q", client.RateLimit())
	}
}

func TestYNABErrorClassification(t *testing.T) {
	cases := []struct {
		status    int
		kind      YNABErrorKind
		retryable bool
		attempts  int
	}{
		{http.StatusUnauthorized, YNABErrorAuth, false, 1},
		{http.StatusForbidden, YNABErrorForbidden, false, 1},
		{http.StatusNotFound, YNABErrorNotFound, false, 1},
		{http.StatusTooManyRequests, YNABErrorRateLimit, true, ynabMaxRetries + 1},
		{http.StatusInternalServerError, YNABErrorTemporary, true, ynabMaxRetries + 1},
		{http.StatusBadRequest, YNABErrorRequest, false, 1},
	}

	for _, tc := range cases {
		var calls int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"error":{"id":"x","name":"err","detail":"boom"}}`))
		}))

		client := newTestYNABClient(t, server.URL, YNABConfig{})
		_, err := client.User(context.Background())
		server.Close()

		var apiErr *YNABError
		if !errors.As(err, &apiErr) {
			t.Fatalf("status %d: expected a YNABError, got %v", tc.status, err)
		}
		if apiErr.Kind != tc.kind {
			t.Fatalf("status %d: expected kind %q, got %q", tc.status, tc.kind, apiErr.Kind)
		}
		if apiErr.Retryable() != tc.retryable {
			t.Fatalf("status %d: unexpected retryable %v", tc.status, apiErr.Retryable())
		}
		if calls != tc.attempts {
			t.Fatalf("status %d: expected %d attempt(s), got %d", tc.status, tc.attempts, calls)
		}
	}
}

func TestYNABErrorRedactsToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"id":"x","name":"err","detail":"bad token ` + testYNABToken + `"}}`))
	}))
	defer server.Close()

	client := newTestYNABClient(t, server.URL, YNABConfig{})
	_, err := client.User(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), testYNABToken) {
		t.Fatalf("token leaked into the error: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("expected a redaction marker: %v", err)
	}
}

func TestYNABDeltaSyncSendsServerKnowledge(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		_, _ = w.Write([]byte(`{"data":{"accounts":[],"server_knowledge":42}}`))
	}))
	defer server.Close()

	client := newTestYNABClient(t, server.URL, YNABConfig{})
	ctx := context.Background()

	if _, err := client.Accounts(ctx, "budget-1"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if got := client.ServerKnowledge("/budgets/budget-1/accounts"); got != 42 {
		t.Fatalf("server knowledge not stored: %d", got)
	}
	if _, err := client.Accounts(ctx, "budget-1"); err != nil {
		t.Fatalf("second call: %v", err)
	}

	if len(queries) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(queries))
	}
	if queries[0] != "" {
		t.Fatalf("first call should not send a cursor: %q", queries[0])
	}
	if queries[1] != "last_knowledge_of_server=42" {
		t.Fatalf("second call should send the cursor: %q", queries[1])
	}
}

func TestYNABAccountsFilterClosedAndDeleted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"accounts":[
			{"id":"a1","name":"Open","closed":false,"deleted":false},
			{"id":"a2","name":"Closed","closed":true,"deleted":false},
			{"id":"a3","name":"Deleted","closed":false,"deleted":true}
		]}}`))
	}))
	defer server.Close()

	client := newTestYNABClient(t, server.URL, YNABConfig{})
	accounts, err := client.Accounts(context.Background(), "budget-1")
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	if len(accounts) != 1 || accounts[0].ID != "a1" {
		t.Fatalf("unexpected accounts: %#v", accounts)
	}
}

func TestYNABBudgetPathIsEscaped(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"data":{"accounts":[]}}`))
	}))
	defer server.Close()

	client := newTestYNABClient(t, server.URL, YNABConfig{})
	if _, err := client.Accounts(context.Background(), "weird/../id"); err != nil {
		t.Fatalf("accounts: %v", err)
	}
	if !strings.Contains(path, "weird%2F..%2Fid") {
		t.Fatalf("budget id was not escaped: %q", path)
	}
}
