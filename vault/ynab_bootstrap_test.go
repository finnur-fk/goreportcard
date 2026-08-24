package vault

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	singleBudgetJSON = `[{"id":"budget-eur","name":"FK Rekstur","currency_format":{"iso_code":"EUR"}}]`
	twoBudgetsJSON   = `[{"id":"budget-eur","name":"FK Rekstur","currency_format":{"iso_code":"EUR"}},` +
		`{"id":"budget-isk","name":"Heimili","currency_format":{"iso_code":"ISK"}}]`
	payPalAccountsJSON = `[{"id":"acct-paypal","name":"PayPal Business","closed":false,"deleted":false},` +
		`{"id":"acct-bank","name":"Landsbankinn","closed":false,"deleted":false}]`
)

// newFakeYNABServer serves the three endpoints the bootstrap relies on.
func newFakeYNABServer(t *testing.T, budgetsJSON, accountsJSON, settingsCurrency string) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Rate-Limit", "5/200")
		_, _ = w.Write([]byte(`{"data":{"user":{"id":"user-123"}}}`))
	})
	mux.HandleFunc("/budgets", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"data":{"budgets":%s}}`, budgetsJSON)
	})
	mux.HandleFunc("/budgets/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/accounts"):
			_, _ = fmt.Fprintf(w, `{"data":{"accounts":%s,"server_knowledge":77}}`, accountsJSON)
		case strings.HasSuffix(r.URL.Path, "/settings"):
			_, _ = fmt.Fprintf(w, `{"data":{"settings":{"currency_format":{"iso_code":%q}}}}`, settingsCurrency)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func bootstrapWithConfig(t *testing.T, baseURL string, cfg YNABConfig) (YNABBootstrapResult, error) {
	t.Helper()
	return newTestYNABClient(t, baseURL, cfg).Bootstrap(context.Background())
}

func TestBootstrapYNABMissingAPIKey(t *testing.T) {
	clearYNABEnv(t)

	_, err := BootstrapYNAB(context.Background(), log.New(io.Discard, "", 0))
	if err == nil {
		t.Fatal("expected the bootstrap to fail fast")
	}
	if !strings.Contains(err.Error(), "YNAB_API_KEY") {
		t.Fatalf("error should name the missing variable: %v", err)
	}
}

func TestBootstrapYNABDisabled(t *testing.T) {
	clearYNABEnv(t)
	t.Setenv("YNAB_API_KEY", testYNABToken)
	t.Setenv("YNAB_ENABLED", "off")

	result, err := BootstrapYNAB(context.Background(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Skipped {
		t.Fatal("expected a skipped bootstrap")
	}
	if !strings.Contains(result.Report(), "skipped") {
		t.Fatalf("unexpected report: %s", result.Report())
	}
}

func TestBootstrapYNABFromEnvironment(t *testing.T) {
	server := newFakeYNABServer(t, singleBudgetJSON, payPalAccountsJSON, "EUR")

	clearYNABEnv(t)
	t.Setenv("YNAB_API_KEY", testYNABToken)
	t.Setenv("YNAB_BASE_URL", server.URL)
	t.Setenv("YNAB_TIMEOUT_SECONDS", "5")

	result, err := BootstrapYNAB(context.Background(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if result.BudgetID != "budget-eur" {
		t.Fatalf("expected the only budget to be selected, got %q", result.BudgetID)
	}
	if result.UserID != "user-123" {
		t.Fatalf("unexpected user: %q", result.UserID)
	}
	if result.Currency != "EUR" || result.CurrencyMismatch {
		t.Fatalf("unexpected currency state: %q mismatch=%v", result.Currency, result.CurrencyMismatch)
	}
	if result.AccountMapping[defaultAccountID] != "acct-paypal" {
		t.Fatalf("paypal account not mapped: %#v", result.AccountMapping)
	}
	if result.ServerKnowledge != 77 {
		t.Fatalf("server knowledge not recorded: %d", result.ServerKnowledge)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", result.Warnings)
	}

	report := result.Report()
	if strings.Contains(report, testYNABToken) {
		t.Fatal("the report must never contain the access token")
	}
	if !strings.Contains(report, "5/200") {
		t.Fatalf("report should surface the rate limit: %s", report)
	}
}

func TestBootstrapYNABBudgetSelectionRequired(t *testing.T) {
	server := newFakeYNABServer(t, twoBudgetsJSON, payPalAccountsJSON, "EUR")

	result, err := bootstrapWithConfig(t, server.URL, YNABConfig{})
	if !errors.Is(err, ErrYNABBudgetSelection) {
		t.Fatalf("expected a budget selection error, got %v", err)
	}
	menu := result.BudgetMenu()
	if !strings.Contains(menu, "budget-eur") || !strings.Contains(menu, "budget-isk") {
		t.Fatalf("menu should list every budget: %s", menu)
	}
}

func TestBootstrapYNABUnknownBudgetID(t *testing.T) {
	server := newFakeYNABServer(t, twoBudgetsJSON, payPalAccountsJSON, "EUR")

	_, err := bootstrapWithConfig(t, server.URL, YNABConfig{BudgetID: "budget-missing"})
	if err == nil {
		t.Fatal("expected an error for an unknown budget id")
	}
	if !strings.Contains(err.Error(), "budget-eur") {
		t.Fatalf("error should list the valid ids: %v", err)
	}
}

func TestBootstrapYNABLastUsedAlias(t *testing.T) {
	server := newFakeYNABServer(t, twoBudgetsJSON, payPalAccountsJSON, "EUR")

	result, err := bootstrapWithConfig(t, server.URL, YNABConfig{BudgetID: ynabLastUsedBudget})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if result.BudgetID != ynabLastUsedBudget {
		t.Fatalf("expected the alias to be preserved, got %q", result.BudgetID)
	}
	if result.Currency != "EUR" {
		t.Fatalf("currency should come from the settings endpoint, got %q", result.Currency)
	}
}

func TestBootstrapYNABCurrencyMismatch(t *testing.T) {
	server := newFakeYNABServer(t, twoBudgetsJSON, payPalAccountsJSON, "ISK")

	result, err := bootstrapWithConfig(t, server.URL, YNABConfig{BudgetID: "budget-isk"})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !result.CurrencyMismatch {
		t.Fatal("expected a currency mismatch")
	}
	if len(result.Warnings) == 0 || !strings.Contains(result.Warnings[0], defaultCurrency) {
		t.Fatalf("expected a warning naming the ledger currency: %v", result.Warnings)
	}
}

func TestBootstrapYNABAccountMapping(t *testing.T) {
	server := newFakeYNABServer(t, singleBudgetJSON, payPalAccountsJSON, "EUR")

	result, err := bootstrapWithConfig(t, server.URL, YNABConfig{
		AccountMap: map[string]string{
			defaultAccountID: "Landsbankinn",
			"stripe":         "Missing Account",
		},
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if result.AccountMapping[defaultAccountID] != "acct-bank" {
		t.Fatalf("explicit mapping should win: %#v", result.AccountMapping)
	}
	if _, mapped := result.AccountMapping["stripe"]; mapped {
		t.Fatal("an unresolvable entry must not be mapped")
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "stripe") {
		t.Fatalf("expected a warning about the unresolved entry: %v", result.Warnings)
	}
}

func TestBootstrapYNABWarnsWithoutPayPalAccount(t *testing.T) {
	accounts := `[{"id":"acct-bank","name":"Landsbankinn","closed":false,"deleted":false}]`
	server := newFakeYNABServer(t, singleBudgetJSON, accounts, "EUR")

	result, err := bootstrapWithConfig(t, server.URL, YNABConfig{})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], defaultAccountID) {
		t.Fatalf("expected a warning about the missing account: %v", result.Warnings)
	}
}

func TestBootstrapYNABHealthCheckFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"id":"401","name":"unauthorized","detail":"Unauthorized"}}`))
	}))
	defer server.Close()

	_, err := bootstrapWithConfig(t, server.URL, YNABConfig{})
	var apiErr *YNABError
	if !errors.As(err, &apiErr) || apiErr.Kind != YNABErrorAuth {
		t.Fatalf("expected an auth error, got %v", err)
	}
}

func TestYNABMilliunits(t *testing.T) {
	cases := []struct {
		amount string
		want   int64
	}{
		{"100.50", 100500},
		{" -50.00 ", -50000},
		{"0", 0},
		{"2.99", 2990},
		{"-2.999", -2999},
		{"1234567.89", 1234567890},
	}

	for _, tc := range cases {
		got, err := YNABMilliunits(tc.amount)
		if err != nil {
			t.Fatalf("amount %q: %v", tc.amount, err)
		}
		if got != tc.want {
			t.Fatalf("amount %q: expected %d, got %d", tc.amount, tc.want, got)
		}
	}

	for _, invalid := range []string{"", "   ", "abc", "1,50"} {
		if _, err := YNABMilliunits(invalid); err == nil {
			t.Fatalf("amount %q should be rejected", invalid)
		}
	}
}

func TestYNABImportID(t *testing.T) {
	if got := YNABImportID(Transaction{TransactionID: "TXN001"}); got != "paypal-TXN001" {
		t.Fatalf("unexpected import id: %q", got)
	}
	if got := YNABImportID(Transaction{TransactionID: "  "}); got != "" {
		t.Fatalf("an unstable transaction must not get an import id: %q", got)
	}

	long := YNABImportID(Transaction{TransactionID: strings.Repeat("A", 80)})
	if len(long) != ynabMaxImportIDLength {
		t.Fatalf("import id must fit the YNAB limit, got %d chars", len(long))
	}
	if long != YNABImportID(Transaction{TransactionID: strings.Repeat("A", 80)}) {
		t.Fatal("import id must be deterministic")
	}
	if long == YNABImportID(Transaction{TransactionID: strings.Repeat("A", 79) + "B"}) {
		t.Fatal("distinct transactions must not collide")
	}
}

func TestBuildYNABTransaction(t *testing.T) {
	payload, err := BuildYNABTransaction(Transaction{
		Date:          "2024-01-15",
		Type:          PaymentTransaction,
		Amount:        "100.50",
		Description:   "Product sale payment",
		TransactionID: "TXN001",
	}, "acct-paypal")
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if payload["amount"] != int64(100500) {
		t.Fatalf("amount must be milliunits: %#v", payload["amount"])
	}
	if payload["import_id"] != "paypal-TXN001" {
		t.Fatalf("unexpected import id: %#v", payload["import_id"])
	}
	if payload["account_id"] != "acct-paypal" {
		t.Fatalf("unexpected account: %#v", payload["account_id"])
	}
	if payload["date"] != "2024-01-15" {
		t.Fatalf("unexpected date: %#v", payload["date"])
	}

	if _, err := BuildYNABTransaction(Transaction{Amount: "not-a-number"}, "acct"); err == nil {
		t.Fatal("expected an error for an unparsable amount")
	}
}

func TestBuildWebhookPayloadIDUnchanged(t *testing.T) {
	payload := buildWebhookPayload(Transaction{TransactionID: "TXN001"})
	if payload["id"] != "paypal-TXN001" {
		t.Fatalf("unexpected id: %v", payload["id"])
	}

	generated, ok := buildWebhookPayload(Transaction{})["id"].(string)
	if !ok || !strings.HasPrefix(generated, "paypal-") {
		t.Fatalf("unexpected generated id: %v", generated)
	}

	if payload["id"] != buildWebhookPayload(Transaction{TransactionID: "paypal-TXN001"})["id"] {
		t.Fatal("an already prefixed id must not be prefixed twice")
	}
}
