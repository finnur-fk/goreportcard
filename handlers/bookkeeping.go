package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/dgraph-io/badger/v2"
	"github.com/gojp/goreportcard/vault"
)

// BookkeepingHandler serves the bookkeeping viewer page showing transaction data
func (gh *GRCHandler) BookkeepingHandler(w http.ResponseWriter, r *http.Request, db *badger.DB) {
	// Security: Only allow GET requests
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	t, err := gh.loadTemplate("/templates/bookkeeping.html")
	if err != nil {
		http.Error(w, "Failed to load template", http.StatusInternalServerError)
		return
	}

	// Read transactions from vault
	vaultDir := getEnvOrDefault("VAULT_DIR", "vault")
	ledgerDir := getEnvOrDefault("LEDGER_DIR", "ledger")

	processor, err := vault.NewTransactionProcessor(vaultDir, ledgerDir)
	if err != nil {
		http.Error(w, "Failed to initialize transaction processor", http.StatusInternalServerError)
		return
	}

	transactions, err := processor.ReadCSVFiles()
	if err != nil {
		http.Error(w, "Failed to read transaction files", http.StatusInternalServerError)
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

	// Use current year dynamically
	currentYear := fmt.Sprintf("%d", time.Now().Year())

	data := map[string]interface{}{
		"Transactions":         transactionData,
		"Summary":              summary,
		"Year":                 currentYear,
		"google_analytics_key": googleAnalyticsKey,
	}

	if err := t.ExecuteTemplate(w, "base", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// BookkeepingAPIHandler provides JSON API for transaction data
func (gh *GRCHandler) BookkeepingAPIHandler(w http.ResponseWriter, r *http.Request, db *badger.DB) {
	// Security: Only allow GET requests
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Set content type first
	w.Header().Set("Content-Type", "application/json")

	vaultDir := getEnvOrDefault("VAULT_DIR", "vault")
	ledgerDir := getEnvOrDefault("LEDGER_DIR", "ledger")

	processor, err := vault.NewTransactionProcessor(vaultDir, ledgerDir)
	if err != nil {
		log.Printf("Error initializing processor: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to initialize transaction processor"})
		return
	}

	transactions, err := processor.ReadCSVFiles()
	if err != nil {
		log.Printf("Error reading CSV files: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to read transaction files"})
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

	json.NewEncoder(w).Encode(response)
}

// ProcessTransactionsHandler triggers manual processing of CSV files
func (gh *GRCHandler) ProcessTransactionsHandler(w http.ResponseWriter, r *http.Request, db *badger.DB) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Set content type first
	w.Header().Set("Content-Type", "application/json")

	vaultDir := getEnvOrDefault("VAULT_DIR", "vault")
	ledgerDir := getEnvOrDefault("LEDGER_DIR", "ledger")

	if err := vault.Run(vaultDir, ledgerDir); err != nil {
		log.Printf("Error processing transactions: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "error",
			"message": "Failed to process transactions",
		})
		return
	}

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
		n, err := fmt.Sscanf(txn.Amount, "%f", &amount)
		if err != nil || n != 1 {
			log.Printf("Warning: Failed to parse amount '%s' for transaction %s, treating as 0.0", txn.Amount, txn.TransactionID)
			amount = 0.0
		}

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

// getEnvOrDefault returns environment variable value or default if not set
// Security: Sanitizes both environment values and defaults
func getEnvOrDefault(name, defaultValue string) string {
	value := os.Getenv(name)
	if value == "" {
		// Use default value, sanitized
		absPath, err := filepath.Abs(filepath.Clean(defaultValue))
		if err != nil {
			return defaultValue
		}
		return absPath
	}
	// Sanitize environment variable value
	absPath, err := filepath.Abs(filepath.Clean(value))
	if err != nil {
		return value
	}
	return absPath
}
