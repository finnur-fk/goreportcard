package handlers

import (
	"html"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

// LedgerHandler handles the ledger page
func (gh *GRCHandler) LedgerHandler(w http.ResponseWriter, r *http.Request) {
	// Read the ledger markdown file
	ledgerPath := filepath.Join("ledger", "FK_MASTER_LEDGER.md")
	content, err := os.ReadFile(ledgerPath)
	if err != nil {
		log.Println("ERROR: could not read ledger file: ", err)
		// If file doesn't exist, show a message
		content = []byte("# No Ledger Available\n\nNo ledger data has been generated yet.")
	}

	t, err := gh.loadTemplate("templates/ledger.html")
	if err != nil {
		log.Println("ERROR: could not get ledger template: ", err)
		http.Error(w, err.Error(), 500)
		return
	}

	if err := t.ExecuteTemplate(w, "base", map[string]interface{}{
		"google_analytics_key": googleAnalyticsKey,
		"LedgerContent":        template.HTML(markdownToHTML(string(content))),
	}); err != nil {
		log.Println("ERROR:", err)
	}
}

// markdownToHTML converts markdown to HTML with proper escaping
// This is a minimal implementation for the ledger display
func markdownToHTML(md string) string {
	// Escape HTML to prevent XSS attacks
	escaped := html.EscapeString(md)
	// Wrap in a div with proper styling
	return `<div class="ledger-content">` + escaped + `</div>`
}
