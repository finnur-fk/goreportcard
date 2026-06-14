package vault

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadWebhookConfig(t *testing.T) {
	t.Setenv("ALPACORE_WEBHOOK_URL", "https://engine.example/api/webhook/global/catch")
	t.Setenv("ALPACORE_WEBHOOK_SESSION_ID", "paypal-ledger")
	t.Setenv("ALPACORE_WEBHOOK_TIMEOUT_SECONDS", "15")

	cfg := LoadWebhookConfig()
	if cfg.URL != "https://engine.example/api/webhook/global/catch" {
		t.Fatalf("unexpected url: %q", cfg.URL)
	}
	if cfg.SessionID != "paypal-ledger" {
		t.Fatalf("unexpected session: %q", cfg.SessionID)
	}
	if cfg.Timeout.Seconds() != 15 {
		t.Fatalf("unexpected timeout: %s", cfg.Timeout)
	}
	if !cfg.Enabled {
		t.Fatal("expected enabled")
	}
}

func TestLoadWebhookConfigDisabled(t *testing.T) {
	t.Setenv("ALPACORE_WEBHOOK_URL", "https://engine.example/catch")
	t.Setenv("ALPACORE_WEBHOOK_ENABLED", "false")

	cfg := LoadWebhookConfig()
	if cfg.Enabled {
		t.Fatal("expected disabled")
	}
}

func TestBuildWebhookPayload(t *testing.T) {
	payload := buildWebhookPayload(Transaction{
		Date:          "2024-01-15",
		Type:          PaymentTransaction,
		Amount:        "100.50",
		Description:   "Product sale payment",
		TransactionID: "TXN001",
	})

	if payload["id"] != "paypal-TXN001" {
		t.Fatalf("unexpected id: %v", payload["id"])
	}
	if payload["state"] != "completed" {
		t.Fatalf("unexpected state: %v", payload["state"])
	}
	if payload["amount"] != 100.50 {
		t.Fatalf("unexpected amount: %v", payload["amount"])
	}
	if payload["currency"] != "EUR" {
		t.Fatalf("unexpected currency: %v", payload["currency"])
	}
}

func TestDispatchToAlpaCore(t *testing.T) {
	var received int
	var session string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received++
		session = r.Header.Get("X-Session-Id")

		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if body["id"] == nil || body["state"] == nil || body["amount"] == nil {
			t.Errorf("missing required fields: %#v", body)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"STORED"}`))
	}))
	defer server.Close()

	t.Setenv("ALPACORE_WEBHOOK_URL", server.URL)
	t.Setenv("ALPACORE_WEBHOOK_SESSION_ID", "paypal-ledger")

	tmpDir := t.TempDir()
	vaultDir := filepath.Join(tmpDir, "vault")
	ledgerDir := filepath.Join(tmpDir, "ledger")
	if err := os.MkdirAll(vaultDir, 0755); err != nil {
		t.Fatalf("mkdir vault: %v", err)
	}

	sample, err := os.ReadFile(filepath.Join("sample_transactions.csv"))
	if err != nil {
		t.Fatalf("read sample csv: %v", err)
	}
	if err := os.WriteFile(filepath.Join(vaultDir, "sample_transactions.csv"), sample, 0644); err != nil {
		t.Fatalf("write sample csv: %v", err)
	}

	processor, err := NewTransactionProcessor(vaultDir, ledgerDir)
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}

	result, err := processor.ProcessWithResult()
	if err != nil {
		t.Fatalf("process with result: %v", err)
	}

	if result.TransactionCount != 7 {
		t.Fatalf("expected 7 transactions, got %d", result.TransactionCount)
	}
	if result.Dispatch.Skipped {
		t.Fatal("dispatch should not be skipped")
	}
	if result.Dispatch.Succeeded != 7 {
		t.Fatalf("expected 7 succeeded, got %d (errors=%v)", result.Dispatch.Succeeded, result.Dispatch.Errors)
	}
	if received != 7 {
		t.Fatalf("server received %d requests, want 7", received)
	}
	if session != "paypal-ledger" {
		t.Fatalf("unexpected session header: %q", session)
	}

	ledgerPath := filepath.Join(ledgerDir, "FK_MASTER_LEDGER.md")
	if _, err := os.Stat(ledgerPath); err != nil {
		t.Fatalf("ledger not generated: %v", err)
	}
}

func TestDispatchSkippedWithoutURL(t *testing.T) {
	t.Setenv("ALPACORE_WEBHOOK_URL", "")
	t.Setenv("WEBHOOK_GLOBAL_CATCH_URL", "")

	tmpDir := t.TempDir()
	vaultDir := filepath.Join(tmpDir, "vault")
	ledgerDir := filepath.Join(tmpDir, "ledger")
	os.MkdirAll(vaultDir, 0755)

	processor, err := NewTransactionProcessor(vaultDir, ledgerDir)
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}

	result := processor.DispatchToAlpaCore([]Transaction{
		{TransactionID: "TXN001", Amount: "1.00", Type: PaymentTransaction},
	})

	if !result.Skipped {
		t.Fatal("expected skipped dispatch")
	}
}

func TestWebhookConfigFallbackVars(t *testing.T) {
	t.Setenv("ALPACORE_WEBHOOK_URL", "")
	t.Setenv("WEBHOOK_GLOBAL_CATCH_URL", "https://engine.example/catch")
	t.Setenv("ALPACORE_WEBHOOK_SESSION_ID", "")
	t.Setenv("X_SESSION_ID", "session-fallback")

	cfg := LoadWebhookConfig()
	if !strings.HasSuffix(cfg.URL, "/catch") {
		t.Fatalf("unexpected url: %q", cfg.URL)
	}
	if cfg.SessionID != "session-fallback" {
		t.Fatalf("unexpected session: %q", cfg.SessionID)
	}
}
