package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/dgraph-io/badger/v2"
	"github.com/gojp/goreportcard/vault"
)

// BookkeepingHandler serves the bookkeeping viewer page showing transaction data
func (gh GRCHandler) BookkeepingHandler(w http.ResponseWriter, r *http.Request, db *badger.DB) {
	// Security: Only allow GET requests
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	t, err := gh.loadTemplate("/templates/bookkeeping.html")
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load template: %v", err), http.StatusInternalServerError)
		return
	}

	// Read transactions from vault
	vaultDir := getEnvOrDefault("VAULT_DIR", "vault")
	ledgerDir := getEnvOrDefault("LEDGER_DIR", "ledger")

	processor, err := vault.NewTransactionProcessor(vaultDir, ledgerDir)
	if err != nil {
		renderError(w, "Failed to initialize transaction processor", err)
		return
	}

	transactions, err := processor.ReadCSVFiles()
	if err != nil {
		renderError(w, "Failed to read transaction files", err)
		return
	}

	// Categorize transactions
	categorized := processor.CategorizeTransactions(transactions)

	// Calculate summary statistics
	summary := calculateSummary(transactions)

	// Create a more template-friendly structure
	transactionData := map[string][]vault.Transaction{
		"Payments":  categorized[vault.PaymentTransaction],
		"Transfers": categorized[vault.TransferTransaction],
		"Fees":      categorized[vault.FeeTransaction],
	}

	data := map[string]interface{}{
		"Transactions":         transactionData,
		"Summary":              summary,
		"Year":                 "2026",
		"google_analytics_key": googleAnalyticsKey,
	}

	if err := t.ExecuteTemplate(w, "base", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// BookkeepingAPIHandler provides JSON API for transaction data
func (gh GRCHandler) BookkeepingAPIHandler(w http.ResponseWriter, r *http.Request, db *badger.DB) {
	// Security: Only allow GET requests
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	vaultDir := getEnvOrDefault("VAULT_DIR", "vault")
	ledgerDir := getEnvOrDefault("LEDGER_DIR", "ledger")

	processor, err := vault.NewTransactionProcessor(vaultDir, ledgerDir)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	transactions, err := processor.ReadCSVFiles()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	categorized := processor.CategorizeTransactions(transactions)
	summary := calculateSummary(transactions)

	// Create a more API-friendly structure
	transactionData := map[string][]vault.Transaction{
		"Payments":  categorized[vault.PaymentTransaction],
		"Transfers": categorized[vault.TransferTransaction],
		"Fees":      categorized[vault.FeeTransaction],
	}

	response := struct {
		Transactions map[string][]vault.Transaction `json:"transactions"`
		Summary      SummaryStats                   `json:"summary"`
		Count        int                            `json:"count"`
	}{
		Transactions: transactionData,
		Summary:      summary,
		Count:        len(transactions),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// ProcessTransactionsHandler triggers manual processing of CSV files
func (gh GRCHandler) ProcessTransactionsHandler(w http.ResponseWriter, r *http.Request, db *badger.DB) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	vaultDir := getEnvOrDefault("VAULT_DIR", "vault")
	ledgerDir := getEnvOrDefault("LEDGER_DIR", "ledger")

	if err := vault.Run(vaultDir, ledgerDir); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "error",
			"message": err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "success",
		"message": "Transactions processed successfully",
	})
}

// SummaryStats holds summary statistics for financial data
type SummaryStats struct {
	TotalTransactions int     `json:"total_transactions"`
	TotalPayments     int     `json:"total_payments"`
	TotalTransfers    int     `json:"total_transfers"`
	TotalFees         int     `json:"total_fees"`
	PaymentsSum       float64 `json:"payments_sum"`
	TransfersSum      float64 `json:"transfers_sum"`
	FeesSum           float64 `json:"fees_sum"`
	NetLiquidity      float64 `json:"net_liquidity"`
}

// calculateSummary computes summary statistics from transactions
func calculateSummary(transactions []vault.Transaction) SummaryStats {
	stats := SummaryStats{
		TotalTransactions: len(transactions),
	}

	for _, txn := range transactions {
		var amount float64
		fmt.Sscanf(txn.Amount, "%f", &amount)

		switch txn.Type {
		case vault.PaymentTransaction:
			stats.TotalPayments++
			stats.PaymentsSum += amount
		case vault.TransferTransaction:
			stats.TotalTransfers++
			stats.TransfersSum += amount
		case vault.FeeTransaction:
			stats.TotalFees++
			stats.FeesSum += amount
		}
	}

	// Calculate net liquidity (payments + transfers + fees)
	stats.NetLiquidity = stats.PaymentsSum + stats.TransfersSum + stats.FeesSum

	return stats
}

// renderError renders an error page
func renderError(w http.ResponseWriter, message string, err error) {
	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprintf(w, "<html><body><h1>Error</h1><p>%s: %v</p></body></html>", message, err)
}

// getEnvOrDefault returns environment variable value or default if not set
func getEnvOrDefault(name, defaultValue string) string {
	value := os.Getenv(name)
	if value == "" {
		// Try to use absolute path from repository root
		if absPath, err := filepath.Abs(defaultValue); err == nil {
			return absPath
		}
		return defaultValue
	}
	return value
}
