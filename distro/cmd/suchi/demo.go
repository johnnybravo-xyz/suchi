// `suchi demo` — seed DATA_DIR with a small representative dataset so
// a first-time visitor can click around instead of staring at the
// empty state. Idempotent per run: if the target rows already exist,
// they are skipped.
//
// What lands (user-independent, always):
//   - 4 correspondents (Landlord, HDFC Bank, Amazon, BESCOM)
//   - 4 document_types (Invoice, Receipt, Bank statement, Utility bill)
//   - 4 tags (rent, utilities, purchase, banking)
//   - 1 automation ("route utilities" — auto-tags BESCOM docs)
//   - 1 rule (title-contains "invoice" → set_document_type Invoice)
//
// What lands (only when an admin/user already exists):
//   - 3 sample documents (title + content only; no real files) owned
//     by the first available admin (or first user if no admin).
//
// Admin provisioning:
//   - Normal (SUCHI_DEMO_MODE unset): the seed does NOT create a user.
//     /setup is the single source of truth for admin credentials, so
//     there's no UNIQUE-email collision. On a fresh DATA_DIR: run
//     `suchi demo` → `suchi serve` → `/bootstrap` → rerun `suchi demo`
//     to seed docs owned by the new admin.
//   - Public demo (SUCHI_DEMO_MODE=1): the seed mints a passwordless
//     system admin (`admin@demo.local`, password_hash NULL) so first
//     boot is zero-touch — visitors land on the SPA via the anon
//     session tier, no `/bootstrap` handshake needed. The row exists
//     only to own docs + satisfy usersEmpty; NULL hash means no login
//     path (localauth treats !hash.Valid as invalid credentials).
//
// Everything uses INSERT OR IGNORE / UPSERT so re-running converges on
// the same state.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/logx"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/postingest"
	"github.com/johnnybravo-xyz/suchi/core/slug"
	"github.com/johnnybravo-xyz/suchi/distro/demo"
	localauth "github.com/johnnybravo-xyz/suchi/plugins/local-auth"
)

// Public demo credentials. Rendered as a hint on /login when
// SUCHI_DEMO_MODE=1 (see ui.Server.DemoHint) so visitors who bother to
// open the login page know how to get in. Not a secret by design —
// the anon-session tier is the primary landing path anyway.
const (
	DemoLoginEmail    = "user@demo.suchi.page"
	DemoLoginPassword = "demo"
)

func runDemo(args []string) int {
	fs := flag.NewFlagSet("suchi demo", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "DATA_DIR to seed; defaults to $DATA_DIR or /data")
	corpusFile := fs.String("corpus-file", "", "path to a suchi-demo corpus tarball (skips HTTP fetch)")
	corpusURL := fs.String("corpus-url", "", "HTTPS URL of a suchi-demo corpus tarball; defaults to the latest release")
	corpusDirFlag := fs.String("corpus-dir", "", "path to an already-extracted corpus directory (skips tarball fetch + extract)")
	fetchOnly := fs.Bool("fetch-only", false, "download + extract the corpus into the cache; do not seed the DB")
	resetFlag := fs.Bool("reset", false, "wipe demo-seeded rows before seeding (idempotent full reset)")
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

	// If the operator pointed at a corpus tarball (or asked us to fetch
	// one), resolve it before touching the DB. --corpus-dir skips the
	// tarball dance entirely (compose stacks that bind-mount the corpus
	// as a volume). --fetch-only exits after extraction.
	var corpusDir string
	switch {
	case *corpusDirFlag != "":
		corpusDir = *corpusDirFlag
		fmt.Printf("corpus dir: %s\n", corpusDir)
	case *corpusFile != "" || *corpusURL != "":
		corpusDir, err = demo.Fetch(ctx, demo.FetchOptions{
			LocalFile: *corpusFile,
			URL:       *corpusURL,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "corpus fetch: %v\n", err)
			return 1
		}
		fmt.Printf("corpus ready at %s\n", corpusDir)
	}
	if *fetchOnly {
		return 0
	}

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

	if *resetFlag {
		if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
			return resetDemoRows(ctx, tx)
		}); err != nil {
			fmt.Fprintf(os.Stderr, "demo reset: %v\n", err)
			return 1
		}
		fmt.Println("demo rows wiped; reseeding")
	}

	now := time.Now().Unix()

	// Demo mode + empty DB: mint the public demo admin. Credentials
	// are intentionally fixed + trivial (published on the login page)
	// so a visitor who wants to "log in" can, but the SPA's default
	// landing goes through the anon-session tier and never asks.
	//
	// Security posture:
	//   - Role stays admin so seeded docs stay visible to the login
	//     path and rescan seeding has an owner. Shared-state mutations
	//     from this session are still blocked by httpx.DemoReadOnly
	//     (the middleware guards regardless of role).
	//   - Fixed password is hashed with argon2id via localauth.HashPassword,
	//     matching every other credentialed row. Publishing the plaintext
	//     is a demo affordance, not a leak.
	//   - Guarded on cfg.DemoMode so a normal `suchi demo` invocation
	//     never plants a known-password admin on a real install.
	//   - INSERT ... WHERE NOT EXISTS keeps re-runs idempotent.
	if cfg.DemoMode {
		hash, err := localauth.HashPassword(DemoLoginPassword)
		if err != nil {
			fmt.Fprintf(os.Stderr, "demo admin hash: %v\n", err)
			return 1
		}
		if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO users(email, display_name, role, password_hash, created_at, updated_at)
				SELECT ?, 'Demo User', 'admin', ?, ?, ?
				WHERE NOT EXISTS (SELECT 1 FROM users)
			`, DemoLoginEmail, hash, now, now)
			return err
		}); err != nil {
			fmt.Fprintf(os.Stderr, "demo admin: %v\n", err)
			return 1
		}
	}

	// Look up an owner for the sample docs. Prefer an admin; fall back
	// to the first user. On non-demo installs before /setup, this comes
	// up empty and the doc seed is skipped.
	var demoUser int64
	_ = d.Read.QueryRowContext(ctx, `
		SELECT id FROM users
		WHERE disabled = 0
		ORDER BY role = 'admin' DESC, id ASC
		LIMIT 1
	`).Scan(&demoUser)

	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		return seedTaxonomy(ctx, tx, now)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "seed taxonomy: %v\n", err)
		return 1
	}

	// The 3 metadata-only sample rows are only useful as a smoke-test
	// stand-in when no real corpus is available — they carry sentinel
	// `demo:*` blob keys and can't preview. Skip them entirely when a
	// corpus is being ingested (the corpus's own fixtures are the
	// browse-testing dataset).
	switch {
	case corpusDir != "":
		// covered by the manifest seed below
	case demoUser > 0:
		if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
			return seedDocs(ctx, tx, now, demoUser)
		}); err != nil {
			fmt.Fprintf(os.Stderr, "seed docs: %v\n", err)
			return 1
		}
	default:
		fmt.Println("no users yet — skipping sample docs. Complete /setup, then re-run `suchi demo` to seed docs owned by the new admin.")
	}

	if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		return seedAutomationAndRule(ctx, tx, now)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "seed automation: %v\n", err)
		return 1
	}

	// Manifest-driven corpus seed. For every fixture in the manifest:
	//   - stream the file into the CAS (dedup is automatic on hash)
	//   - upsert its correspondent + document_type into taxonomy
	//   - insert the document row (idempotent via ON CONFLICT DO NOTHING
	//     on the (owner_id, original_blob) partial index)
	//   - link tags via document_tags
	//   - enqueue a post-ingest job so content extraction + FTS happen
	//     in the background once serve starts
	if corpusDir != "" {
		if demoUser <= 0 {
			fmt.Fprintln(os.Stderr, "corpus seed: no admin/owner found — cannot own docs. Complete /setup first or enable SUCHI_DEMO_MODE.")
			return 1
		}
		cas, err := blob.New(cfg.DataDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "corpus seed cas: %v\n", err)
			return 1
		}
		ingest := makeFixtureIngest(d, cas, demoUser, now, log)
		viewIngest := makeSavedViewIngest(d, demoUser, now)
		stats, err := demo.SeedFromManifest(ctx, demo.SeedOptions{
			CorpusDir:       corpusDir,
			Log:             log,
			FixtureIngest:   ingest,
			SavedViewIngest: viewIngest,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "manifest seed: %v\n", err)
			return 1
		}
		fmt.Printf("manifest seed: seeded=%d skipped=%d failed=%d would-seed=%d views=%d existing-views=%d views-failed=%d\n",
			stats.Seeded, stats.Skipped, stats.Failed, stats.WouldSeed,
			stats.ViewsSeeded, stats.ViewsExisting, stats.ViewsFailed)
	}

	fmt.Println("demo seed complete — start `suchi serve` and browse the doc list.")
	return 0
}

// resetDemoRows wipes rows previously seeded by `suchi demo` so a
// nightly reset returns the DB to the canonical seed state. Deletes:
//   - automations / rules whose name starts with "demo:"
//   - documents whose original_blob starts with "demo:"
//   - correspondents / tags / document_types are LEFT ALONE — they
//     may be referenced by user uploads, and seedTaxonomy is
//     idempotent via ON CONFLICT.
//
// The reset is intentionally narrow. Users' own uploads survive until
// the demo-mode ticker's scratch-user sweep evicts them.
func resetDemoRows(ctx context.Context, tx *sql.Tx) error {
	stmts := []string{
		`DELETE FROM automation_actions WHERE automation_id IN (SELECT id FROM automations WHERE name LIKE 'demo:%')`,
		`DELETE FROM automation_triggers WHERE automation_id IN (SELECT id FROM automations WHERE name LIKE 'demo:%')`,
		`DELETE FROM automations WHERE name LIKE 'demo:%'`,
		`DELETE FROM rules WHERE name LIKE 'demo:%'`,
		`DELETE FROM documents WHERE original_blob LIKE 'demo:%'`,
	}
	for _, s := range stmts {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}

func seedTaxonomy(ctx context.Context, tx *sql.Tx, now int64) error {
	corrs := []string{"Landlord", "HDFC Bank", "Amazon", "BESCOM"}
	for _, c := range corrs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO correspondents(name, slug, created_at, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(name) DO NOTHING
		`, c, slug.Make(c), now, now); err != nil {
			return err
		}
	}
	types := []string{"Invoice", "Receipt", "Bank statement", "Utility bill"}
	for _, dt := range types {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO document_types(name, slug, created_at, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(name) DO NOTHING
		`, dt, slug.Make(dt), now, now); err != nil {
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
		blobKey := fmt.Sprintf("demo:%d:%s", owner, slug.Make(d.title))
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

	var atmID int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM automations WHERE name = 'demo: route utilities'`).Scan(&atmID)
	if err == nil {
		return nil // already seeded
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO automations(name, order_index, enabled, created_at, updated_at)
		VALUES ('demo: route utilities', 10, 1, ?, ?)
	`, now, now)
	if err != nil {
		return err
	}
	atmID, _ = res.LastInsertId()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO automation_triggers(automation_id, type, filter_corr_id, created_at)
		VALUES (?, 'document_added', ?, ?)
	`, atmID, corrID, now); err != nil {
		return err
	}

	params, _ := json.Marshal(map[string]any{"tag_ids": []int64{tagID}})
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO automation_actions(automation_id, order_index, kind, params_json, created_at)
		VALUES (?, 0, 'assign_tags', ?, ?)
	`, atmID, string(params), now); err != nil {
		return err
	}
	return nil
}

// makeFixtureIngest builds the demo.SeedFromManifest callback. Each
// fixture becomes:
//   - one blob in the CAS (dedup automatic on hash)
//   - one row in `documents` (idempotent — the unique index on
//     (owner_id, original_blob) means a re-run is a no-op)
//   - one link row per tag in `document_tags`
//   - one post-ingest job so content extraction + FTS + thumb happen
//     in the background when serve starts
//
// Correspondents / document_types named by the manifest are upserted
// on demand; the fixture author doesn't have to pre-populate taxonomy.
// JD categories are looked up by code and fall back to the JD inbox
// (code=10) if the manifest names something outside the seeded tree.
func makeFixtureIngest(d *db.DB, cas *blob.CAS, ownerID int64, now int64, log *slog.Logger) func(context.Context, demo.ManifestFixture, string) error {
	return func(ctx context.Context, f demo.ManifestFixture, path string) error {
		// 1. Stream the file into the CAS.
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open fixture: %w", err)
		}
		defer file.Close()
		ref, err := cas.Put(file)
		if err != nil {
			return fmt.Errorf("cas put: %w", err)
		}

		// 2. Derive a browsable title from the fixture filename.
		title := titleFromFilename(f.Filename)

		// 3. Sniff MIME from extension. Falls back to octet-stream so
		//    the ingest pipeline can still route it (content-detect
		//    inside post-ingest is the source of truth anyway).
		mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(f.Filename)))
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}

		// 4. JD category by manifest code; fall back to inbox.
		var jdCatID int64
		if f.JDCategory > 0 {
			_ = d.Read.QueryRowContext(ctx,
				`SELECT id FROM jd_categories WHERE code = ?`, f.JDCategory).
				Scan(&jdCatID)
		}
		if jdCatID == 0 {
			if err := d.Read.QueryRowContext(ctx,
				`SELECT id FROM jd_categories WHERE code = 10`).Scan(&jdCatID); err != nil {
				_ = d.Read.QueryRowContext(ctx,
					`SELECT id FROM jd_categories ORDER BY id LIMIT 1`).Scan(&jdCatID)
			}
		}

		// 5. One write-tx: upsert taxonomy, insert doc, link tags,
		//    enqueue post-ingest. Keeps the doc row + its outbox job
		//    atomic so a crash between them can't leave orphan work.
		return d.WriteTx(ctx, func(tx *sql.Tx) error {
			corrID, err := upsertCorrespondent(ctx, tx, f.Correspondent, now)
			if err != nil {
				return err
			}
			dtID, err := upsertDocumentType(ctx, tx, f.DocumentType, now)
			if err != nil {
				return err
			}

			res, err := tx.ExecContext(ctx, `
				INSERT INTO documents(
					owner_id, original_blob, original_size, title, mime_type,
					jd_category_id, correspondent_id, document_type_id,
					sensitivity, languages,
					added_at, created_at, updated_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT DO NOTHING
			`, ownerID, ref.SHA256, ref.Size, title, mimeType,
				jdCatID, corrID, dtID,
				nullString(f.Sensitivity), f.Language,
				now, now, now)
			if err != nil {
				return err
			}
			docID, err := res.LastInsertId()
			if err != nil {
				return err
			}
			if docID == 0 {
				// Already seeded (ON CONFLICT hit). Skip tag + job wiring
				// so re-runs stay silent.
				return nil
			}

			for _, name := range f.Tags {
				tagID, err := upsertTag(ctx, tx, name, now)
				if err != nil {
					return err
				}
				if tagID == 0 {
					continue
				}
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO document_tags(document_id, tag_id)
					VALUES (?, ?) ON CONFLICT DO NOTHING
				`, docID, tagID); err != nil {
					return err
				}
			}

			payload, err := json.Marshal(map[string]any{
				"sha256":    ref.SHA256,
				"size":      ref.Size,
				"mime_type": mimeType,
				"filename":  f.Filename,
			})
			if err != nil {
				return err
			}
			return jobs.Enqueue(ctx, tx, postingest.Kind, docID, string(payload))
		})
	}
}

func upsertCorrespondent(ctx context.Context, tx *sql.Tx, name string, now int64) (sql.NullInt64, error) {
	if name == "" {
		return sql.NullInt64{}, nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO correspondents(name, slug, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO NOTHING
	`, name, slug.Make(name), now, now); err != nil {
		return sql.NullInt64{}, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM correspondents WHERE name = ?`, name).Scan(&id); err != nil {
		return sql.NullInt64{}, err
	}
	return sql.NullInt64{Int64: id, Valid: true}, nil
}

func upsertDocumentType(ctx context.Context, tx *sql.Tx, name string, now int64) (sql.NullInt64, error) {
	if name == "" {
		return sql.NullInt64{}, nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO document_types(name, slug, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO NOTHING
	`, name, slug.Make(name), now, now); err != nil {
		return sql.NullInt64{}, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM document_types WHERE name = ?`, name).Scan(&id); err != nil {
		return sql.NullInt64{}, err
	}
	return sql.NullInt64{Int64: id, Valid: true}, nil
}

func upsertTag(ctx context.Context, tx *sql.Tx, name string, now int64) (int64, error) {
	if name == "" {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tags(name, slug, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO NOTHING
	`, name, slug.Make(name), now, now); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM tags WHERE name = ?`, name).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// titleFromFilename turns "bescom_january.pdf" → "Bescom January". Not
// clever — the demo intent is "the row shows up with a plausible
// title", not linguistic accuracy. Anything better belongs in the
// content-extraction pipeline once it runs.
func titleFromFilename(name string) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	base = strings.NewReplacer("_", " ", "-", " ", ".", " ").Replace(base)
	fields := strings.Fields(base)
	for i, w := range fields {
		if len(w) == 0 {
			continue
		}
		if w[0] >= 'a' && w[0] <= 'z' {
			fields[i] = string(w[0]-32) + w[1:]
		}
	}
	return strings.Join(fields, " ")
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func makeSavedViewIngest(d *db.DB, ownerID, now int64) func(context.Context, demo.ManifestSavedView) (bool, error) {
	return func(ctx context.Context, view demo.ManifestSavedView) (bool, error) {
		display := view.Display
		if display == "" {
			display = "list"
		}
		shared := 0
		if view.Shared {
			shared = 1
		}
		var created bool
		err := d.WriteTx(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, `
				INSERT INTO saved_views(owner_id, name, filter_json, display, position, shared, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(owner_id, name) DO NOTHING
			`, ownerID, view.Name, string(view.FilterJSON), display, view.Position, shared, now, now)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			created = n == 1
			return err
		})
		return created, err
	}
}
