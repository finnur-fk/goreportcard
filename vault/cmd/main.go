// Package main provides a command-line utility for processing PayPal transactions.
// It reads CSV files from the vault directory and generates a ledger report.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/gojp/goreportcard/vault"
)

func main() {
	// Define command-line flags
	vaultDir := flag.String("vault", "vault", "Directory containing PayPal CSV transaction files")
	ledgerDir := flag.String("ledger", "ledger", "Directory for generated ledger reports")
	ynabBootstrap := flag.Bool("ynab-bootstrap", false, "Validate the YNAB access token and resolve budget/accounts")
	apexScrapeURL := flag.String("apex-scrape", "", "Scrape a target URL using AlpaCore Apex and print the JSON result")
	help := flag.Bool("help", false, "Show usage information")

	flag.Parse()

	apexScrapeRequested := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "apex-scrape" {
			apexScrapeRequested = true
		}
	})

	if *help {
		fmt.Println("PayPal Transaction Processor")
		fmt.Println("\nUsage:")
		flag.PrintDefaults()
		fmt.Println("\nExample:")
		fmt.Println("  go run vault/cmd/main.go -vault=./vault -ledger=./ledger")
		fmt.Println("  go run vault/cmd/main.go -ynab-bootstrap")
		fmt.Println("  go run ./vault/cmd -apex-scrape=https://example.org")
		os.Exit(0)
	}

	if apexScrapeRequested {
		if *ynabBootstrap {
			log.Fatal("Apex scrape and YNAB bootstrap cannot be run together")
		}
		if err := runApexScrape(context.Background(), *apexScrapeURL, os.Stdout); err != nil {
			log.Fatalf("Apex scrape failed: %v", err)
		}
		return
	}

	if *ynabBootstrap {
		if err := vault.PrintYNABBootstrap(context.Background()); err != nil {
			log.Fatalf("YNAB bootstrap failed: %v", err)
		}
		return
	}

	// Convert to absolute paths
	absVaultDir, err := filepath.Abs(*vaultDir)
	if err != nil {
		log.Fatalf("Invalid vault directory path: %v", err)
	}

	absLedgerDir, err := filepath.Abs(*ledgerDir)
	if err != nil {
		log.Fatalf("Invalid ledger directory path: %v", err)
	}

	fmt.Printf("Processing transactions...\n")
	fmt.Printf("Vault Directory:  %s\n", absVaultDir)
	fmt.Printf("Ledger Directory: %s\n\n", absLedgerDir)

	// Run the transaction processor
	if err := vault.Run(absVaultDir, absLedgerDir); err != nil {
		log.Fatalf("Error processing transactions: %v", err)
	}

	fmt.Println("\n✓ Transaction processing completed successfully!")
}

func runApexScrape(ctx context.Context, targetURL string, output io.Writer) error {
	result, err := vault.ScrapeApex(ctx, targetURL)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(output).Encode(result); err != nil {
		return fmt.Errorf("write Apex scrape result: %w", err)
	}
	return nil
}
