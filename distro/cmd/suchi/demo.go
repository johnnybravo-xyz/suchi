// `suchi demo` materializes a versioned corpus manifest into a dedicated
// showcase archive. Corpus content and product-story choices live in the
// sibling suchi-demo repository; this command owns only generic ingestion.

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
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
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
	corpusURL := fs.String("corpus-url", "", "HTTPS URL of a compatible suchi-demo corpus tarball; defaults to the tested release")
	corpusDirFlag := fs.String("corpus-dir", "", "path to an already-extracted corpus directory (skips tarball fetch + extract)")
	fetchOnly := fs.Bool("fetch-only", false, "download + extract the corpus into the cache; do not seed the DB")
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

	// Resolve the tested release unless the operator supplied a local
	// tarball or extracted directory. --fetch-only exits after extraction.
	var corpusDir string
	switch {
	case *corpusDirFlag != "":
		corpusDir = *corpusDirFlag
		fmt.Printf("corpus dir: %s\n", corpusDir)
	default:
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

	// Look up an owner for corpus fixtures. Prefer an admin; fall back to
	// the first user. A normal fresh install must complete setup first.
	var demoUser int64
	_ = d.Read.QueryRowContext(ctx, `
		SELECT id FROM users
		WHERE disabled = 0
		ORDER BY role = 'admin' DESC, id ASC
		LIMIT 1
	`).Scan(&demoUser)

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
		ingest := makeFixtureIngest(d, cas, demoUser, now)
		viewIngest := makeSavedViewIngest(d, demoUser, now)
		automationIngest := makeAutomationIngest(d, now)
		stats, err := demo.SeedFromManifest(ctx, demo.SeedOptions{
			CorpusDir:        corpusDir,
			Log:              log,
			FixtureIngest:    ingest,
			SavedViewIngest:  viewIngest,
			AutomationIngest: automationIngest,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "manifest seed: %v\n", err)
			return 1
		}
		fmt.Printf("manifest seed: seeded=%d existing=%d skipped=%d failed=%d would-seed=%d views=%d existing-views=%d views-failed=%d automations=%d existing-automations=%d automations-failed=%d\n",
			stats.Seeded, stats.Existing, stats.Skipped, stats.Failed, stats.WouldSeed,
			stats.ViewsSeeded, stats.ViewsExisting, stats.ViewsFailed,
			stats.AutomationsSeeded, stats.AutomationsExisting, stats.AutomationsFailed)
		if stats.Skipped+stats.Failed+stats.ViewsFailed+stats.AutomationsFailed > 0 {
			fmt.Fprintln(os.Stderr, "manifest seed incomplete; fix the corpus errors above and retry")
			return 1
		}
	}

	fmt.Println("demo seed complete — start `suchi serve` and browse the doc list.")
	return 0
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
func makeFixtureIngest(d *db.DB, cas *blob.CAS, ownerID int64, now int64) func(context.Context, demo.ManifestFixture, string) (bool, error) {
	return func(ctx context.Context, f demo.ManifestFixture, path string) (bool, error) {
		// 1. Stream the file into the CAS.
		file, err := os.Open(path)
		if err != nil {
			return false, fmt.Errorf("open fixture: %w", err)
		}
		defer file.Close()
		ref, err := cas.Put(file)
		if err != nil {
			return false, fmt.Errorf("cas put: %w", err)
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
		created := false
		err = d.WriteTx(ctx, func(tx *sql.Tx) error {
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
			created = true

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
		return created, err
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

func makeAutomationIngest(d *db.DB, now int64) func(context.Context, int, presetfile.SeedAutomation) (bool, error) {
	return func(ctx context.Context, order int, seed presetfile.SeedAutomation) (bool, error) {
		created := false
		err := d.WriteTx(ctx, func(tx *sql.Tx) error {
			var existing int64
			err := tx.QueryRowContext(ctx, `SELECT id FROM automations WHERE name = ?`, seed.Name).Scan(&existing)
			switch {
			case err == nil:
				return nil
			case err != sql.ErrNoRows:
				return err
			}

			res, err := tx.ExecContext(ctx, `
				INSERT INTO automations(name, order_index, enabled, created_at, updated_at)
				VALUES (?, ?, 1, ?, ?)
			`, seed.Name, order, now, now)
			if err != nil {
				return err
			}
			automationID, err := res.LastInsertId()
			if err != nil {
				return err
			}

			filterTagID, err := upsertTag(ctx, tx, seed.Trigger.FilterTag, now)
			if err != nil {
				return err
			}
			filterCorrID, err := upsertCorrespondent(ctx, tx, seed.Trigger.FilterCorrespondent, now)
			if err != nil {
				return err
			}
			filterDocTypeID, err := upsertDocumentType(ctx, tx, seed.Trigger.FilterDocumentType, now)
			if err != nil {
				return err
			}
			triggerType := map[int]string{1: "consumption", 2: "document_added", 3: "document_updated"}[seed.Trigger.Type]
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO automation_triggers(
					automation_id, type, filter_path, filter_filename,
					filter_tag_id, filter_corr_id, filter_doctype_id,
					filter_title_re, filter_content_re, created_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			`, automationID, triggerType,
				nullIfEmpty(seed.Trigger.FilterPath), nullIfEmpty(seed.Trigger.FilterFilename),
				nullIfZero(filterTagID), nullableInt64(filterCorrID), nullableInt64(filterDocTypeID),
				nullIfEmpty(seed.Trigger.FilterTitleMatching), nullIfEmpty(seed.Trigger.FilterContentMatching), now); err != nil {
				return err
			}

			for i, action := range seed.Actions {
				params, err := resolveDemoActionParams(ctx, tx, action.Params, now)
				if err != nil {
					return fmt.Errorf("action %d: %w", i, err)
				}
				body, err := json.Marshal(params)
				if err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO automation_actions(automation_id, order_index, kind, params_json, created_at)
					VALUES (?, ?, ?, ?, ?)
				`, automationID, i, action.Kind, string(body), now); err != nil {
					return err
				}
			}
			created = true
			return nil
		})
		return created, err
	}
}

func resolveDemoActionParams(ctx context.Context, tx *sql.Tx, params map[string]any, now int64) (map[string]any, error) {
	out := make(map[string]any, len(params))
	for key, value := range params {
		out[key] = value
	}
	if names := stringList(out["tags"]); len(names) > 0 {
		ids := make([]int64, 0, len(names))
		for _, name := range names {
			id, err := upsertTag(ctx, tx, name, now)
			if err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		delete(out, "tags")
		out["tag_ids"] = ids
	} else if name, ok := out["tag"].(string); ok && name != "" {
		id, err := upsertTag(ctx, tx, name, now)
		if err != nil {
			return nil, err
		}
		delete(out, "tag")
		out["tag_ids"] = []int64{id}
	}
	if name, ok := out["document_type"].(string); ok && name != "" {
		id, err := upsertDocumentType(ctx, tx, name, now)
		if err != nil {
			return nil, err
		}
		delete(out, "document_type")
		out["document_type_id"] = id.Int64
	}
	if name, ok := out["correspondent"].(string); ok && name != "" {
		id, err := upsertCorrespondent(ctx, tx, name, now)
		if err != nil {
			return nil, err
		}
		delete(out, "correspondent")
		out["correspondent_id"] = id.Int64
	}
	if code, ok := numericInt(out["jd_category_code"]); ok {
		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM jd_categories WHERE code = ?`, code).Scan(&id); err != nil {
			return nil, fmt.Errorf("jd category code %d: %w", code, err)
		}
		delete(out, "jd_category_code")
		out["jd_category_id"] = id
	}
	return out, nil
}

func stringList(value any) []string {
	switch values := value.(type) {
	case []string:
		return values
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok && text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func numericInt(value any) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, true
	case int64:
		return int(number), true
	case float64:
		return int(number), true
	default:
		return 0, false
	}
}

func nullIfZero(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func nullableInt64(id sql.NullInt64) any {
	if !id.Valid {
		return nil
	}
	return id.Int64
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
