// `suchi demo` — seed DATA_DIR with a small representative dataset so
// a first-time visitor can click around instead of staring at the
// empty state. Idempotent per run: if the target rows already exist,
// they are skipped.
//
// What lands:
//   - 1 admin user (email demo@example.com) if none exists yet — but
//     ONLY when the DB has zero users. If bootstrap already ran, the
//     demo skips user creation to avoid clobbering credentials.
//   - 4 correspondents (Landlord, HDFC Bank, Amazon, BESCOM)
//   - 4 document_types (Invoice, Receipt, Bank statement, Utility bill)
//   - 4 tags (rent, utilities, purchase, banking)
//   - 3 sample documents (title + content only; no real files)
//   - 1 automation ("route utilities" — auto-tags BESCOM docs)
//   - 1 rule (title-contains "invoice" → set_document_type Invoice)
//
// It writes no blobs — the docs are purely metadata for browse
// testing. If you need real file previews, upload a few PDFs after.
//
// Not idempotent on the user side: re-running never touches an
// existing user. Everything else uses INSERT OR IGNORE / UPSERT so
// re-running converges on the same state.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/suchi-dms/suchi/core/config"
	"github.com/suchi-dms/suchi/core/db"
	migrations "github.com/suchi-dms/suchi/core/db/migrations"
	"github.com/suchi-dms/suchi/core/jd"
	"github.com/suchi-dms/suchi/core/logx"
)

func runDemo(args []string) int {
	fs := flag.NewFlagSet("suchi demo", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "DATA_DIR to seed; defaults to $DATA_DIR or /data")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 1
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}

	log := logx.Setup(os.Stdout, cfg.LogLevel)
	slog.SetDefault(log)

	ctx := context.Background()

	d, err := db.Open(ctx, cfg.DataDir+"/suchi.db")
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		return 1
	}
	defer func() { _ = d.Close() }()

	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "load migrations: %v\n", err)
		return 1
	}
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		return 1
	}
	if err := jd.EnsureTree(ctx, d, log, jd.ModeJD); err != nil {
		fmt.Fprintf(os.Stderr, "jd tree: %v\n", err)
		return 1
	}

	now := time.Now().Unix()

	// Seed a demo admin user only if the DB has zero users.
	var userCount int
	_ = d.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&userCount)
	var demoUser int64
	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		if userCount == 0 {
			res, err := tx.ExecContext(ctx, `
				INSERT INTO users(email, display_name, role, created_at, updated_at)
				VALUES ('demo@example.com', 'Demo Admin', 'admin', ?, ?)
			`, now, now)
			if err != nil {
				return err
			}
			demoUser, _ = res.LastInsertId()
			fmt.Println("seeded demo user: demo@example.com (no password — run bootstrap or the API to add credentials)")
		} else {
			_ = tx.QueryRowContext(ctx, `SELECT id FROM users ORDER BY id LIMIT 1`).Scan(&demoUser)
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "seed user: %v\n", err)
		return 1
	}

	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		return seedTaxonomy(ctx, tx, now)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "seed taxonomy: %v\n", err)
		return 1
	}

	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		return seedDocs(ctx, tx, now, demoUser)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "seed docs: %v\n", err)
		return 1
	}

	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		return seedAutomationAndRule(ctx, tx, now)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "seed automation: %v\n", err)
		return 1
	}

	fmt.Println("demo seed complete — start `suchi serve` and browse the doc list.")
	return 0
}

func seedTaxonomy(ctx context.Context, tx *sql.Tx, now int64) error {
	corrs := []string{"Landlord", "HDFC Bank", "Amazon", "BESCOM"}
	for _, c := range corrs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO correspondents(name, slug, created_at, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(name) DO NOTHING
		`, c, slugify(c), now, now); err != nil {
			return err
		}
	}
	types := []string{"Invoice", "Receipt", "Bank statement", "Utility bill"}
	for _, dt := range types {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO document_types(name, slug, created_at, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(name) DO NOTHING
		`, dt, slugify(dt), now, now); err != nil {
			return err
		}
	}
	tags := []string{"rent", "utilities", "purchase", "banking"}
	for _, t := range tags {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tags(name, slug, created_at, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(name) DO NOTHING
		`, t, t, now, now); err != nil {
			return err
		}
	}
	return nil
}

func seedDocs(ctx context.Context, tx *sql.Tx, now int64, owner int64) error {
	// jd category — take Inbox; classifier can rehome later.
	var inbox int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM jd_categories WHERE code = 10`).Scan(&inbox); err != nil {
		// Inbox may live under a different code depending on the tree
		// preset — fall through to first category we find.
		_ = tx.QueryRowContext(ctx,
			`SELECT id FROM jd_categories ORDER BY id LIMIT 1`).Scan(&inbox)
	}

	docs := []struct {
		title, content, corr string
	}{
		{
			title:   "March 2026 electricity bill",
			content: "BESCOM bill for the month of March. Amount due: Rs. 4523. Due date: 15 April 2026.",
			corr:    "BESCOM",
		},
		{
			title:   "Rent invoice — March 2026",
			content: "Monthly rent invoice from the landlord. Amount: Rs. 45000. Due: 5th of each month.",
			corr:    "Landlord",
		},
		{
			title:   "HDFC statement — Q1 2026",
			content: "Quarterly bank statement covering January through March 2026.",
			corr:    "HDFC Bank",
		},
	}
	for i, d := range docs {
		var corrID sql.NullInt64
		var id int64
		_ = tx.QueryRowContext(ctx,
			`SELECT id FROM correspondents WHERE name = ?`, d.corr).Scan(&id)
		if id != 0 {
			corrID = sql.NullInt64{Int64: id, Valid: true}
		}

		// Sentinel blob key so re-runs are idempotent via the unique
		// (owner_id, original_blob) partial index.
		blobKey := fmt.Sprintf("demo:%d:%s", owner, slugify(d.title))
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO documents(owner_id, original_blob, original_size,
			                      title, content, correspondent_id,
			                      jd_category_id, created_at, updated_at)
			VALUES (?, ?, 0, ?, ?, ?, ?, ?, ?)
			ON CONFLICT DO NOTHING
		`, owner, blobKey, d.title, d.content, corrID, inbox,
			now-int64((i+1)*3600), now-int64((i+1)*3600)); err != nil {
			return err
		}
	}
	return nil
}

func seedAutomationAndRule(ctx context.Context, tx *sql.Tx, now int64) error {
	// One rule — deterministic classifier line.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rules(name, description, if_kind, if_value, then_kind, then_value,
		                  priority, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 100, 1, ?, ?)
		ON CONFLICT(name) DO NOTHING
	`, "demo: invoices are Invoice type",
		"Sample rule seeded by `suchi demo`.",
		"title_contains", "invoice",
		"set_document_type", "Invoice",
		now, now); err != nil {
		return err
	}

	// One automation — "when doc arrives with correspondent BESCOM,
	// tag it utilities". We look up the ids we just seeded.
	var corrID, tagID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM correspondents WHERE name = 'BESCOM'`).Scan(&corrID); err != nil {
		return nil // demo taxonomy missing — skip quietly
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM tags WHERE name = 'utilities'`).Scan(&tagID); err != nil {
		return nil
	}

	var wfID int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM workflows WHERE name = 'demo: route utilities'`).Scan(&wfID)
	if err == nil {
		return nil // already seeded
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO workflows(name, order_index, enabled, created_at, updated_at)
		VALUES ('demo: route utilities', 10, 1, ?, ?)
	`, now, now)
	if err != nil {
		return err
	}
	wfID, _ = res.LastInsertId()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workflow_triggers(workflow_id, type, filter_corr_id, created_at)
		VALUES (?, 'document_added', ?, ?)
	`, wfID, corrID, now); err != nil {
		return err
	}

	params, _ := json.Marshal(map[string]any{"tag_ids": []int64{tagID}})
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workflow_actions(workflow_id, order_index, kind, params_json, created_at)
		VALUES (?, 0, 'assign_tags', ?, ?)
	`, wfID, string(params), now); err != nil {
		return err
	}
	return nil
}

// slugify — lowercase + replace non-alphanumeric with '-'. Duplicates
// the classifier's helper because pulling that in would add a
// dependency for four lines of code.
func slugify(s string) string {
	out := make([]byte, 0, len(s))
	prevDash := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32)
			prevDash = false
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
			prevDash = false
		default:
			if !prevDash && len(out) > 0 {
				out = append(out, '-')
				prevDash = true
			}
		}
	}
	if len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}
	return string(out)
}
