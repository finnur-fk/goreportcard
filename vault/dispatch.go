package vault

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultWebhookSession = "paypal-ledger"
	defaultWebhookTimeout = 30 * time.Second
	defaultAccountID      = "paypal-processor"
	defaultCurrency       = "EUR"
)

// WebhookConfig holds AlpaCore global/catch dispatch settings from environment.
type WebhookConfig struct {
	URL       string
	SessionID string
	Timeout   time.Duration
	Enabled   bool
}

// DispatchResult summarizes webhook POST outcomes for processed PayPal transactions.
type DispatchResult struct {
	Attempted int
	Succeeded int
	Failed    int
	Skipped   bool
	Errors    []string
}

// ProcessResult combines ledger generation with optional webhook dispatch stats.
type ProcessResult struct {
	TransactionCount int
	Dispatch         DispatchResult
}

// LoadWebhookConfig reads Railway/AlpaCore bridge variables.
func LoadWebhookConfig() WebhookConfig {
	url := firstNonEmpty(
		os.Getenv("ALPACORE_WEBHOOK_URL"),
		os.Getenv("WEBHOOK_GLOBAL_CATCH_URL"),
	)
	session := firstNonEmpty(
		os.Getenv("ALPACORE_WEBHOOK_SESSION_ID"),
		os.Getenv("X_SESSION_ID"),
		defaultWebhookSession,
	)

	timeout := defaultWebhookTimeout
	if raw := strings.TrimSpace(os.Getenv("ALPACORE_WEBHOOK_TIMEOUT_SECONDS")); raw != "" {
		if sec, err := strconv.Atoi(raw); err == nil && sec > 0 {
			timeout = time.Duration(sec) * time.Second
		}
	}

	enabled := url != ""
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ALPACORE_WEBHOOK_ENABLED"))) {
	case "false", "0", "no", "off":
		enabled = false
	}

	return WebhookConfig{
		URL:       strings.TrimSpace(url),
		SessionID: strings.TrimSpace(session),
		Timeout:   timeout,
		Enabled:   enabled,
	}
}

// DispatchToAlpaCore POSTs each parsed transaction to AlpaCore /api/webhook/global/catch.
func (tp *TransactionProcessor) DispatchToAlpaCore(transactions []Transaction) DispatchResult {
	cfg := LoadWebhookConfig()
	if !cfg.Enabled || cfg.URL == "" {
		tp.logger.Println("Webhook dispatch skipped: ALPACORE_WEBHOOK_URL / WEBHOOK_GLOBAL_CATCH_URL not set")
		return DispatchResult{Skipped: true}
	}

	if len(transactions) == 0 {
		return DispatchResult{Skipped: true}
	}

	client := &http.Client{Timeout: cfg.Timeout}
	result := DispatchResult{Attempted: len(transactions)}

	tp.logger.Printf(
		"Dispatching %d transaction(s) to AlpaCore webhook (session=%s)",
		len(transactions),
		cfg.SessionID,
	)

	for _, txn := range transactions {
		if err := postTransaction(client, cfg, txn); err != nil {
			result.Failed++
			msg := fmt.Sprintf("%s: %v", txn.TransactionID, err)
			result.Errors = append(result.Errors, msg)
			tp.logger.Printf("Webhook dispatch failed for %s: %v", txn.TransactionID, err)
			continue
		}
		result.Succeeded++
	}

	tp.logger.Printf(
		"Webhook dispatch complete: %d/%d succeeded",
		result.Succeeded,
		result.Attempted,
	)

	return result
}

func postTransaction(client *http.Client, cfg WebhookConfig, txn Transaction) error {
	body, err := json.Marshal(buildWebhookPayload(txn))
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-Id", cfg.SessionID)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	return nil
}

func buildWebhookPayload(txn Transaction) map[string]interface{} {
	amount, _ := strconv.ParseFloat(strings.TrimSpace(txn.Amount), 64)

	id := strings.TrimSpace(txn.TransactionID)
	if id == "" {
		id = fmt.Sprintf("paypal-%d", time.Now().UnixNano())
	} else if !strings.HasPrefix(strings.ToLower(id), "paypal-") {
		id = "paypal-" + id
	}

	note := fmt.Sprintf("PayPal %s: %s", txn.Type, strings.TrimSpace(txn.Description))

	return map[string]interface{}{
		"id":          id,
		"state":       "completed",
		"amount":      amount,
		"currency":    defaultCurrency,
		"account_id":  defaultAccountID,
		"note":        note,
		"source":      "goreportcard",
		"type":        string(txn.Type),
		"date":        txn.Date,
		"description": txn.Description,
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// RunWithResult processes CSV files, generates the ledger, and dispatches to AlpaCore.
func RunWithResult(vaultDir, ledgerDir string) (ProcessResult, error) {
	processor, err := NewTransactionProcessor(vaultDir, ledgerDir)
	if err != nil {
		return ProcessResult{}, fmt.Errorf("failed to initialize processor: %w", err)
	}

	return processor.ProcessWithResult()
}

// ProcessWithResult runs the full workflow including webhook dispatch.
func (tp *TransactionProcessor) ProcessWithResult() (ProcessResult, error) {
	result := ProcessResult{}

	tp.logger.Println("Starting transaction processing...")

	transactions, err := tp.ReadCSVFiles()
	if err != nil {
		return result, fmt.Errorf("failed to read CSV files: %w", err)
	}

	result.TransactionCount = len(transactions)
	if len(transactions) == 0 {
		tp.logger.Println("No transactions found to process")
		result.Dispatch = DispatchResult{Skipped: true}
		return result, nil
	}

	tp.logger.Printf("Total transactions read: %d", len(transactions))

	if err := tp.GenerateLedger(transactions, "FK_MASTER_LEDGER.md"); err != nil {
		return result, fmt.Errorf("failed to generate ledger: %w", err)
	}

	result.Dispatch = tp.DispatchToAlpaCore(transactions)
	tp.logger.Println("Transaction processing completed successfully")
	return result, nil
}

// Process runs the workflow and dispatches to AlpaCore when webhook env vars are set.
func (tp *TransactionProcessor) Process() error {
	_, err := tp.ProcessWithResult()
	return err
}

// Run is a convenience function that creates a processor and runs the complete workflow.
func Run(vaultDir, ledgerDir string) error {
	_, err := RunWithResult(vaultDir, ledgerDir)
	return err
}
