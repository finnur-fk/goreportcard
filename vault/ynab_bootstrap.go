package vault

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// ynabMaxImportIDLength is the hard limit YNAB enforces on import_id values.
const ynabMaxImportIDLength = 36

// ErrYNABBudgetSelection is returned when several budgets exist and YNAB_BUDGET_ID is unset.
// The caller is expected to print the candidate list and let a human pick one.
var ErrYNABBudgetSelection = errors.New("YNAB_BUDGET_ID is not set and the token can reach more than one budget")

// YNABBootstrapResult is the outcome of validating and wiring up the YNAB connection.
type YNABBootstrapResult struct {
	// Skipped is true when the bridge is disabled or no token is configured.
	Skipped bool
	// UserID is the identity behind the Personal Access Token.
	UserID string
	// BudgetID is the resolved budget identifier (UUID or the "last-used" alias).
	BudgetID string
	// BudgetName is the human readable budget name when known.
	BudgetName string
	// Currency is the budget ISO currency code reported by YNAB.
	Currency string
	// CurrencyMismatch is true when the budget currency differs from the ledger currency.
	CurrencyMismatch bool
	// AvailableBudgets lists every budget the token can reach.
	AvailableBudgets []YNABBudget
	// Accounts lists the open accounts of the resolved budget.
	Accounts []YNABAccount
	// AccountMapping maps AlpaCore account identifiers to YNAB account UUIDs.
	AccountMapping map[string]string
	// Warnings collects non-fatal issues that need human attention.
	Warnings []string
	// RateLimit is the last X-Rate-Limit header value ("used/limit").
	RateLimit string
	// ServerKnowledge is the delta-sync cursor for the accounts endpoint.
	ServerKnowledge int64
}

// BootstrapYNAB loads the environment configuration and wires up the YNAB connection.
// A missing YNAB_API_KEY is a hard error; YNAB_ENABLED=false skips the bootstrap instead.
func BootstrapYNAB(ctx context.Context, logger *log.Logger) (YNABBootstrapResult, error) {
	cfg := LoadYNABConfig()

	if err := cfg.Validate(); err != nil {
		return YNABBootstrapResult{}, err
	}
	if !cfg.Enabled {
		return YNABBootstrapResult{Skipped: true}, nil
	}

	client, err := NewYNABClient(cfg, logger)
	if err != nil {
		return YNABBootstrapResult{}, err
	}

	return client.Bootstrap(ctx)
}

// Bootstrap validates the token, resolves the budget and maps AlpaCore accounts to YNAB.
func (c *YNABClient) Bootstrap(ctx context.Context) (YNABBootstrapResult, error) {
	result := YNABBootstrapResult{AccountMapping: map[string]string{}}

	user, err := c.User(ctx)
	if err != nil {
		return result, fmt.Errorf("health check failed: %w", err)
	}
	result.UserID = user.ID
	c.logger.Printf("Access token accepted for user %s", user.ID)

	budgets, err := c.Budgets(ctx)
	if err != nil {
		return result, fmt.Errorf("budget discovery failed: %w", err)
	}
	result.AvailableBudgets = budgets

	if err := c.resolveBudget(ctx, &result); err != nil {
		result.RateLimit = c.RateLimit()
		return result, err
	}

	if err := c.resolveAccounts(ctx, &result); err != nil {
		result.RateLimit = c.RateLimit()
		return result, err
	}

	result.RateLimit = c.RateLimit()
	return result, nil
}

// resolveBudget selects the budget to use and records its currency.
func (c *YNABClient) resolveBudget(ctx context.Context, result *YNABBootstrapResult) error {
	budgetID := c.cfg.BudgetID

	switch {
	case budgetID == ynabLastUsedBudget:
		result.BudgetID = ynabLastUsedBudget
		result.BudgetName = "(resolved by YNAB as last-used)"
	case budgetID != "":
		budget, found := findYNABBudget(result.AvailableBudgets, budgetID)
		if !found {
			return fmt.Errorf(
				"YNAB_BUDGET_ID %q is not available to this token; valid ids: %s",
				budgetID,
				describeYNABBudgets(result.AvailableBudgets),
			)
		}
		result.BudgetID = budget.ID
		result.BudgetName = budget.Name
		result.Currency = budget.Currency()
	case len(result.AvailableBudgets) == 1:
		budget := result.AvailableBudgets[0]
		result.BudgetID = budget.ID
		result.BudgetName = budget.Name
		result.Currency = budget.Currency()
		c.logger.Printf("Using the only available budget: %s", budget.Name)
	case len(result.AvailableBudgets) == 0:
		return errors.New("the access token cannot reach any budget")
	default:
		return fmt.Errorf("%w; choose one of: %s", ErrYNABBudgetSelection, describeYNABBudgets(result.AvailableBudgets))
	}

	if result.Currency == "" {
		currency, err := c.BudgetCurrency(ctx, result.BudgetID)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("could not read the budget currency: %v", err))
		} else {
			result.Currency = currency
		}
	}

	if result.Currency != "" && result.Currency != defaultCurrency {
		result.CurrencyMismatch = true
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"budget currency %s differs from the ledger currency %s hardcoded in vault/dispatch.go; amounts would be booked in the wrong currency",
			result.Currency,
			defaultCurrency,
		))
	}

	return nil
}

// resolveAccounts fetches the open accounts and builds the AlpaCore account mapping.
func (c *YNABClient) resolveAccounts(ctx context.Context, result *YNABBootstrapResult) error {
	accounts, err := c.Accounts(ctx, result.BudgetID)
	if err != nil {
		return fmt.Errorf("account discovery failed: %w", err)
	}

	result.Accounts = accounts
	result.ServerKnowledge = c.ServerKnowledge(ynabAccountsPath(result.BudgetID))

	for alpaID, wanted := range c.cfg.AccountMap {
		account, found := findYNABAccount(accounts, wanted)
		if !found {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"YNAB_ACCOUNT_MAP entry %q=%q matches no open YNAB account", alpaID, wanted,
			))
			continue
		}
		result.AccountMapping[alpaID] = account.ID
	}

	if _, mapped := result.AccountMapping[defaultAccountID]; !mapped {
		c.mapDefaultAccount(accounts, result)
	}

	return nil
}

// mapDefaultAccount tries to guess the YNAB account behind the AlpaCore paypal-processor id.
func (c *YNABClient) mapDefaultAccount(accounts []YNABAccount, result *YNABBootstrapResult) {
	var candidates []YNABAccount
	for _, account := range accounts {
		if strings.Contains(strings.ToLower(account.Name), "paypal") {
			candidates = append(candidates, account)
		}
	}

	switch len(candidates) {
	case 1:
		result.AccountMapping[defaultAccountID] = candidates[0].ID
		c.logger.Printf("Mapped AlpaCore %q to YNAB account %q", defaultAccountID, candidates[0].Name)
	case 0:
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"no open YNAB account matches AlpaCore account_id %q; set YNAB_ACCOUNT_MAP=%s=<account name or id>",
			defaultAccountID, defaultAccountID,
		))
	default:
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"several YNAB accounts look like PayPal accounts; set YNAB_ACCOUNT_MAP=%s=<account name or id> to remove the ambiguity",
			defaultAccountID,
		))
	}
}

// findYNABBudget matches a budget by UUID or by name, case-insensitively.
func findYNABBudget(budgets []YNABBudget, wanted string) (YNABBudget, bool) {
	for _, budget := range budgets {
		if budget.ID == wanted || strings.EqualFold(budget.Name, wanted) {
			return budget, true
		}
	}
	return YNABBudget{}, false
}

// findYNABAccount matches an account by UUID or by name, case-insensitively.
func findYNABAccount(accounts []YNABAccount, wanted string) (YNABAccount, bool) {
	for _, account := range accounts {
		if account.ID == wanted || strings.EqualFold(account.Name, wanted) {
			return account, true
		}
	}
	return YNABAccount{}, false
}

func describeYNABBudgets(budgets []YNABBudget) string {
	if len(budgets) == 0 {
		return "(none)"
	}
	entries := make([]string, 0, len(budgets))
	for _, budget := range budgets {
		entries = append(entries, fmt.Sprintf("%s (%s)", budget.ID, budget.Name))
	}
	return strings.Join(entries, ", ")
}

// Report renders a human readable summary of the bootstrap. It never contains the token.
func (r YNABBootstrapResult) Report() string {
	var b strings.Builder

	b.WriteString("YNAB bootstrap\n")
	if r.Skipped {
		b.WriteString("  status:   skipped (YNAB_ENABLED=false)\n")
		return b.String()
	}

	fmt.Fprintf(&b, "  user:     %s\n", r.UserID)
	fmt.Fprintf(&b, "  budget:   %s (%s)\n", r.BudgetID, r.BudgetName)
	fmt.Fprintf(&b, "  currency: %s (ledger uses %s)\n", orPlaceholder(r.Currency), defaultCurrency)
	fmt.Fprintf(&b, "  accounts: %d open\n", len(r.Accounts))

	if len(r.AccountMapping) > 0 {
		b.WriteString("  mapping:\n")
		for _, alpaID := range sortedKeys(r.AccountMapping) {
			fmt.Fprintf(&b, "    %s -> %s\n", alpaID, r.AccountMapping[alpaID])
		}
	}

	if r.ServerKnowledge > 0 {
		fmt.Fprintf(&b, "  server_knowledge: %d (send as last_knowledge_of_server next time)\n", r.ServerKnowledge)
	}
	if r.RateLimit != "" {
		fmt.Fprintf(&b, "  rate limit: %s requests this hour\n", r.RateLimit)
	}

	for _, warning := range r.Warnings {
		fmt.Fprintf(&b, "  WARNING: %s\n", warning)
	}

	return b.String()
}

// BudgetMenu renders the budget candidates so a human can pick one for YNAB_BUDGET_ID.
// The value is deliberately not written to any file inside the repository.
func (r YNABBootstrapResult) BudgetMenu() string {
	var b strings.Builder
	b.WriteString("Available budgets (set one as YNAB_BUDGET_ID in your secret store):\n")
	for _, budget := range r.AvailableBudgets {
		fmt.Fprintf(&b, "  %s  %s\n", budget.ID, budget.Name)
	}
	return b.String()
}

func orPlaceholder(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// YNABMilliunits converts a decimal amount such as "100.50" into YNAB milliunits (100500).
// YNAB stores every amount as an integer scaled by 1000.
func YNABMilliunits(amount string) (int64, error) {
	trimmed := strings.TrimSpace(amount)
	if trimmed == "" {
		return 0, errors.New("empty amount")
	}

	value, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, fmt.Errorf("parse amount %q: %w", trimmed, err)
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("amount %q is not a finite number", trimmed)
	}

	return int64(math.Round(value * 1000)), nil
}

// YNABImportID builds the deterministic import_id that lets YNAB reject duplicates.
// It reuses the same identifier the AlpaCore webhook payload carries and returns an
// empty string when the transaction has no stable id to derive from.
func YNABImportID(txn Transaction) string {
	if strings.TrimSpace(txn.TransactionID) == "" {
		return ""
	}
	return shortenImportID(webhookTransactionID(txn))
}

// shortenImportID keeps the identifier within the 36 character limit YNAB enforces.
func shortenImportID(id string) string {
	if len(id) <= ynabMaxImportIDLength {
		return id
	}

	digest := fnv.New32a()
	_, _ = digest.Write([]byte(id))
	suffix := fmt.Sprintf("-%08x", digest.Sum32())

	return id[:ynabMaxImportIDLength-len(suffix)] + suffix
}

// BuildYNABTransaction converts a ledger transaction into the YNAB transaction payload.
// The account identifier must already be resolved through the bootstrap account mapping.
func BuildYNABTransaction(txn Transaction, accountID string) (map[string]interface{}, error) {
	milliunits, err := YNABMilliunits(txn.Amount)
	if err != nil {
		return nil, err
	}

	payload := map[string]interface{}{
		"account_id": accountID,
		"date":       strings.TrimSpace(txn.Date),
		"amount":     milliunits,
		"payee_name": truncate(fmt.Sprintf("PayPal %s", txn.Type), 50),
		"memo":       truncate(strings.TrimSpace(txn.Description), 200),
		"cleared":    "cleared",
		"approved":   false,
	}

	if importID := YNABImportID(txn); importID != "" {
		payload["import_id"] = importID
	}

	return payload, nil
}

// PrintYNABBootstrap runs the bootstrap and writes the report to stdout.
// It is the entry point used by the vault command line tool.
func PrintYNABBootstrap(ctx context.Context) error {
	logger := log.New(os.Stdout, "[YNAB] ", log.LstdFlags)

	result, err := BootstrapYNAB(ctx, logger)
	if errors.Is(err, ErrYNABBudgetSelection) {
		fmt.Print(result.BudgetMenu())
		return err
	}
	if err != nil {
		return err
	}

	fmt.Print(result.Report())
	return nil
}
