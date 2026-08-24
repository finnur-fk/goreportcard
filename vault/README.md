# Vault Transaction Processor

A robust Go package for processing PayPal CSV transaction files and generating ledger reports.

## Features

- **CSV Parsing**: Reads PayPal transaction CSV files from the vault directory
- **Transaction Categorization**: Automatically categorizes transactions into:
  - **Payments**: Incoming payments from customers
  - **Transfers**: Money transfers to/from accounts
  - **Fees**: PayPal processing and service fees
- **Error Handling**: Robust error handling for file and data issues with detailed logging
- **Ledger Generation**: Generates formatted markdown ledger reports with transaction tables
- **High Code Quality**: Follows Go best practices with comprehensive documentation

## Installation

```bash
go get github.com/gojp/goreportcard/vault
```

## Usage

### As a Library

```go
package main

import (
    "log"
    "github.com/gojp/goreportcard/vault"
)

func main() {
    // Process transactions from vault/ and generate ledger in ledger/
    if err := vault.Run("vault", "ledger"); err != nil {
        log.Fatalf("Error: %v", err)
    }
}
```

### As a Command-Line Tool

```bash
# Use default directories (vault/ and ledger/)
go run vault/cmd/main.go

# Specify custom directories
go run vault/cmd/main.go -vault=./my-transactions -ledger=./my-reports

# Show help
go run vault/cmd/main.go -help
```

## CSV Format

The processor expects CSV files with the following header:

```
Date,Type,Amount,Description,Transaction ID
```

Example:

```csv
Date,Type,Amount,Description,Transaction ID
2024-01-15,Payment,100.50,Product sale payment,TXN001
2024-01-16,Transfer,-50.00,Bank transfer,TXN002
2024-01-17,Fee,-2.99,PayPal processing fee,TXN003
```

## Output

The processor generates a markdown ledger file (`FK_MASTER_LEDGER.md`) with:

- Transaction summary statistics
- Categorized transaction tables
- Icelandic column headers: Dagsetning, Tegund, Upphæð, Lýsing, PayPal Transaction ID

When Railway bridge variables are set, each transaction is also POSTed to AlpaCore
`POST /api/webhook/global/catch` (session `paypal-ledger` by default).

| Variable | Purpose |
|----------|---------|
| `ALPACORE_WEBHOOK_URL` or `WEBHOOK_GLOBAL_CATCH_URL` | Target webhook URL |
| `ALPACORE_WEBHOOK_SESSION_ID` or `X_SESSION_ID` | `X-Session-Id` header |
| `ALPACORE_WEBHOOK_ENABLED` | Set `false` to disable dispatch |
| `ALPACORE_WEBHOOK_TIMEOUT_SECONDS` | HTTP timeout (default 30) |
| `DATABASE_URL` | Postgres ref (shared with Engine) |

## YNAB bootstrap

The YNAB bridge authenticates with a **Personal Access Token** (not OAuth). Run the
bootstrap once after setting the variables to verify the token, resolve the budget and
map the AlpaCore account ids onto YNAB accounts:

```bash
go run vault/cmd/main.go -ynab-bootstrap
```

| Variable | Purpose |
|----------|---------|
| `YNAB_API_KEY` | Personal Access Token (required, never logged) |
| `YNAB_BUDGET_ID` | Budget UUID or the `last-used` alias; resolved automatically when only one budget exists |
| `YNAB_BASE_URL` | REST root (default `https://api.ynab.com/v1`) |
| `YNAB_TIMEOUT_SECONDS` | HTTP timeout (default 30) |
| `YNAB_ENABLED` | Set `false` to disable the bridge without removing the token |
| `YNAB_ACCOUNT_MAP` | `alpacore-id=YNAB account name or UUID` pairs, comma separated |

The bootstrap:

- calls `GET /user` and reports `401` (token revoked), `429` (the 200 requests/hour
  quota) and `5xx` as distinct, retryable-aware errors;
- calls `GET /budgets`, refuses to guess when several budgets exist and prints the
  candidate list instead — the chosen id belongs in your secret store, never in the repo;
- compares `currency_format.iso_code` with the `EUR` ledger currency hardcoded in
  `vault/dispatch.go` and warns on a mismatch;
- calls `GET /budgets/{id}/accounts`, drops closed and deleted accounts and warns when
  no account matches the AlpaCore `paypal-processor` id;
- stores the `server_knowledge` cursor and replays it as `last_knowledge_of_server` so
  follow-up calls are delta syncs rather than full downloads.

`YNABMilliunits` and `YNABImportID` convert ledger rows into the integer milliunits and
the deterministic `import_id` YNAB uses to reject duplicate imports.

> Store `YNAB_API_KEY` in the same secret store as the other Railway variables. The token
> is never written to logs, reports or files, and plain HTTP base URLs are rejected.

Example output:

```markdown
# FK Master Ledger

**Generated:** 2026-01-16 01:35:50
**Total Transactions:** 7

## Payments

**Count:** 3

| Dagsetning | Tegund | Upphæð | Lýsing | PayPal Transaction ID |
|------------|--------|---------|--------|-----------------------|
| 2024-01-15 | Payments | 100.50 | Product sale payment | TXN001 |
```

## Testing

```bash
# Run tests
go test ./vault/...

# Run tests with coverage
go test ./vault/... -cover

# Run tests verbosely
go test -v ./vault/...
```

## Code Quality

This package follows Go best practices and passes all standard quality checks:

- ✓ `go fmt` - Code formatting
- ✓ `go vet` - Static analysis
- ✓ `staticcheck` - Advanced static analysis
- ✓ `golint` - Code style checking
- ✓ `gocyclo` - Cyclomatic complexity (all functions < 15)
- ✓ `misspell` - Spelling checks
- ✓ Test coverage: 82.2%

## Package Structure

```
vault/
├── check_transactions.go      # Main transaction processor implementation
├── check_transactions_test.go # Comprehensive test suite
├── dispatch.go                # AlpaCore webhook dispatch
├── ynab.go                    # YNAB REST client (PAT auth, typed errors, delta sync)
├── ynab_bootstrap.go          # YNAB connection bootstrap and payload helpers
├── cmd/
│   └── main.go               # Command-line interface
├── sample_transactions.csv   # Example CSV file
└── README.md                 # This file
```

## API Documentation

### Types

- `TransactionType`: Enum for transaction categories (Payments, Transfers, Fees)
- `Transaction`: Represents a single transaction record
- `TransactionProcessor`: Main processor for handling transactions
- `YNABConfig`: Personal Access Token settings read from the environment
- `YNABClient`: Authenticated, rate-limit aware YNAB REST client
- `YNABError`: Classified API failure (`auth`, `forbidden`, `not_found`, `rate_limit`, `temporary`, `request`)
- `YNABBootstrapResult`: Resolved budget, accounts, mapping and warnings

### Functions

- `NewTransactionProcessor(vaultDir, ledgerDir string)`: Create a new processor
- `Run(vaultDir, ledgerDir string)`: Convenience function to run the full workflow
- `LoadYNABConfig()`: Read the `YNAB_*` environment variables
- `BootstrapYNAB(ctx, logger)`: Validate the token and resolve budget/accounts
- `YNABMilliunits(amount string)`: Convert a decimal amount into YNAB milliunits
- `YNABImportID(txn Transaction)`: Build the deterministic `import_id` for deduplication
- `BuildYNABTransaction(txn Transaction, accountID string)`: Build a YNAB transaction payload

### Methods

- `ReadCSVFiles()`: Read all CSV files from vault directory
- `CategorizeTransactions(transactions)`: Group transactions by type
- `GenerateLedger(transactions, outputFilename)`: Generate markdown ledger
- `Process()`: Run the complete processing workflow

## Error Handling

The processor includes comprehensive error handling:

- Validates vault directory exists before processing
- Creates ledger directory if it doesn't exist
- Logs warnings for malformed CSV rows (continues processing)
- Returns errors for critical issues (file access, write failures)
- Provides detailed error messages with context

## Logging

The processor logs all operations to stdout with timestamps:

```
[TransactionProcessor] 2026/01/16 01:35:50 Starting transaction processing...
[TransactionProcessor] 2026/01/16 01:35:50 Found 1 CSV file(s) to process
[TransactionProcessor] 2026/01/16 01:35:50 Successfully processed sample_transactions.csv: 7 transactions
```

## License

This package is part of the goreportcard project and follows the same Apache 2.0 license.
