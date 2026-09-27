// SPDX-License-Identifier: AGPL-3.0-or-later

package jd_test

// Built-ins share the additive importer: applying or reapplying a preset must
// preserve both live and recoverable filing, not reset documents to Inbox.

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := db.Migrate(ctx, d, migs, log); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestApplyPresetPreservesFiledAndTrashedDocuments(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Seed a user (documents.owner_id FK).
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO users(id, email, display_name, role, created_at, updated_at)
		VALUES (1, 'u@t.local', 't', 'admin', 0, 0)
	`); err != nil {
		t.Fatal(err)
	}

	// Apply solo — plants the tree.
	if err := jd.ApplyPreset(ctx, d, log, "solo", jd.ApplyPresetOpts{SystemID: 1}); err != nil {
		t.Fatalf("initial preset apply: %v", err)
	}

	// Pick a non-inbox category to file the doc under.
	var nonInboxID int64
	if err := d.Read.QueryRowContext(ctx,
		`SELECT id FROM jd_categories WHERE system = 0 ORDER BY id LIMIT 1`).Scan(&nonInboxID); err != nil {
		t.Fatalf("pick non-inbox: %v", err)
	}

	// Insert one visible filed doc and one trashed doc. The latter is not
	// visible in the filing-tree picker, but its category FK must survive replacement.
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO documents(
			system_id, owner_id, title, original_blob, original_size,
			jd_category_id, created_at, added_at, updated_at
		) VALUES (1, 1, 'test.md', 'sha-x', 1, ?, 0, 0, 0);
		INSERT INTO documents(
			system_id, owner_id, title, original_blob, original_size,
			jd_category_id, created_at, added_at, updated_at, trashed_at
		) VALUES (1, 1, 'trashed.md', 'sha-trash', 1, ?, 0, 0, 0, 1)
	`, nonInboxID, nonInboxID); err != nil {
		t.Fatal(err)
	}

	// Household carries a reviewed semantic mapping for code 11. The
	// category is renamed in place so both live and recoverable FKs survive.
	if err := jd.ApplyPreset(ctx, d, log, "household", jd.ApplyPresetOpts{SystemID: 1}); err != nil {
		t.Fatalf("reviewed household transition: %v", err)
	}
	var renamedID int64
	var renamed string
	if err := d.Read.QueryRowContext(ctx,
		`SELECT id,name FROM jd_categories WHERE system_id=1 AND code=11`).Scan(&renamedID, &renamed); err != nil {
		t.Fatal(err)
	}
	if renamedID != nonInboxID || renamed != "Family IDs" {
		t.Fatalf("mapped category=(%d,%q), want preserved id %d named Family IDs", renamedID, renamed, nonInboxID)
	}
	if err := jd.ApplyPreset(ctx, d, log, "solo", jd.ApplyPresetOpts{SystemID: 1}); err != nil {
		t.Fatal(err)
	}

	var parked int
	if err := d.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM documents WHERE jd_category_id = ?`, nonInboxID).Scan(&parked); err != nil {
		t.Fatal(err)
	}
	if parked != 2 {
		t.Errorf("documents retaining their category = %d, want 2", parked)
	}
}

func TestPresetChangeReviewsMappingsAndSuspendsSourceRules(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO users(id,email,display_name,role,created_at,updated_at)
		VALUES(1,'admin@example.test','Admin','admin',0,0)`); err != nil {
		t.Fatal(err)
	}
	if err := jd.ApplyPreset(ctx, d, log, "freelance", jd.ApplyPresetOpts{SystemID: 1, ActorID: 1}); err != nil {
		t.Fatal(err)
	}
	var projectAgreementsID int64
	if err := d.Read.QueryRowContext(ctx, `SELECT id FROM jd_categories WHERE system_id=1 AND code=22`).Scan(&projectAgreementsID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO documents(system_id,owner_id,title,original_blob,original_size,jd_category_id,created_at,added_at,updated_at)
		VALUES(1,1,'live','transition-live',1,?,0,0,0);
		INSERT INTO documents(system_id,owner_id,title,original_blob,original_size,jd_category_id,created_at,added_at,updated_at,trashed_at)
		VALUES(1,1,'trashed','transition-trashed',1,?,0,0,0,1)`, projectAgreementsID, projectAgreementsID); err != nil {
		t.Fatal(err)
	}

	request := jd.PresetChangeRequest{PresetID: "smb_billing", SystemID: 1, ActorID: 1}
	preview, err := jd.PreviewPresetChange(ctx, d, request)
	if err != nil {
		t.Fatal(err)
	}
	collisions := map[int]struct {
		resolved, replace, suggested bool
		live, trashed                int
	}{}
	for _, collision := range preview.Collisions {
		collisions[collision.Code] = struct {
			resolved, replace, suggested bool
			live, trashed                int
		}{collision.Resolved, collision.Replace, collision.SuggestedReplace, collision.LiveDocuments, collision.TrashedDocuments}
	}
	for _, code := range []int{22, 23, 24, 25, 34, 35} {
		got := collisions[code]
		if got.resolved || !got.suggested {
			t.Errorf("collision %d = %+v, want reviewed replacement suggestion", code, got)
		}
	}
	if got := collisions[22]; got.live != 1 || got.trashed != 1 {
		t.Fatalf("code 22 document counts = %+v, want one live and one trashed", got)
	}

	request.Replacements = map[int]bool{22: true, 23: true, 24: true, 25: true, 34: true, 35: true}
	reviewed, err := jd.PreviewPresetChange(ctx, d, request)
	if err != nil {
		t.Fatal(err)
	}
	request.ExpectedStateHash = reviewed.StateHash
	if _, err := jd.ApplyPresetChange(ctx, d, log, request); err != nil {
		t.Fatal(err)
	}
	var vendorAgreementID int64
	var vendorName string
	if err := d.Read.QueryRowContext(ctx, `SELECT id,name FROM jd_categories WHERE system_id=1 AND code=22`).Scan(&vendorAgreementID, &vendorName); err != nil {
		t.Fatal(err)
	}
	if vendorAgreementID != projectAgreementsID || vendorName != "Orders & contracts" {
		t.Fatalf("mapped vendor category=(%d,%q), want preserved id %d", vendorAgreementID, vendorName, projectAgreementsID)
	}
	var activeSource, activeTarget int
	if err := d.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM automations WHERE preset_slug='freelance' AND suspended=0`).Scan(&activeSource); err != nil {
		t.Fatal(err)
	}
	if err := d.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM automations WHERE preset_slug='smb_billing' AND suspended=0`).Scan(&activeTarget); err != nil {
		t.Fatal(err)
	}
	if activeSource != 0 || activeTarget == 0 {
		t.Fatalf("active preset rules: freelance=%d smb=%d", activeSource, activeTarget)
	}
}

func TestPresetChangeFiltersAutomaticReplacementsAgainstCurrentTree(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := jd.ApplyPreset(ctx, d, log, "solo", jd.ApplyPresetOpts{SystemID: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write.ExecContext(ctx, `DELETE FROM jd_categories WHERE system_id=1 AND code=25`); err != nil {
		t.Fatal(err)
	}

	preview, err := jd.PreviewPresetChange(ctx, d, jd.PresetChangeRequest{PresetID: "household", SystemID: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range preview.CategoriesToAdd {
		if code == 25 {
			return
		}
	}
	t.Fatal("category 25 was not restored as an addition")
}

func TestComposePresetRejectsSetsSharingALane(t *testing.T) {
	if _, err := jd.ComposePreset([]string{"projects", "vendors"}); err == nil {
		t.Fatal("same-lane sets were accepted")
	}
}

func TestPresetMembershipCategoriesUseAdministrationLane(t *testing.T) {
	for _, tc := range []struct {
		preset string
		name   string
	}{
		{preset: "solo", name: "Education & memberships"},
		{preset: "household", name: "Memberships"},
	} {
		t.Run(tc.preset, func(t *testing.T) {
			d := openTestDB(t)
			if err := jd.ApplyPreset(t.Context(), d, slog.Default(), tc.preset, jd.ApplyPresetOpts{SystemID: 1}); err != nil {
				t.Fatal(err)
			}
			var area, version int
			var name string
			if err := d.Read.QueryRowContext(t.Context(), `
				SELECT c.area_start,c.name,s.preset_version
				FROM jd_categories c
				JOIN jd_systems s ON s.id=c.system_id
				WHERE c.system_id=1 AND c.code=14`).Scan(&area, &name, &version); err != nil {
				t.Fatal(err)
			}
			if area != 10 || name != tc.name || version != 1 {
				t.Fatalf("category 14 = area %d name %q preset version %d, want area 10 name %q version 1", area, name, version, tc.name)
			}
			var misplaced int
			if err := d.Read.QueryRowContext(t.Context(), `
				SELECT count(*) FROM jd_categories
				WHERE system_id=1 AND area_start<>10 AND lower(name) LIKE '%membership%'`).Scan(&misplaced); err != nil {
				t.Fatal(err)
			}
			if misplaced != 0 {
				t.Fatalf("membership categories outside administration lane = %d, want 0", misplaced)
			}
		})
	}
}

func TestPersonalPresetsUseFourFocusedLanes(t *testing.T) {
	for _, preset := range []string{"solo", "household"} {
		t.Run(preset, func(t *testing.T) {
			setIDs := jd.PresetSetIDs(preset)
			if len(setIDs) != 4 {
				t.Fatalf("filing sets = %v, want four", setIDs)
			}
			composed, err := jd.ComposePreset(setIDs)
			if err != nil {
				t.Fatal(err)
			}
			var userAreas int
			for _, area := range composed.Areas {
				if area.Code != 40 {
					userAreas++
				}
				if area.Code == 60 {
					t.Fatal("composed personal tree retained the 60-69 lane")
				}
			}
			if userAreas != 4 {
				t.Fatalf("composed user areas = %d, want 4", userAreas)
			}

			d := openTestDB(t)
			if err := jd.ApplyPreset(t.Context(), d, slog.Default(), preset, jd.ApplyPresetOpts{SystemID: 1}); err != nil {
				t.Fatal(err)
			}
			var areas, ambiguous int
			if err := d.Read.QueryRowContext(t.Context(), `
				SELECT
					(SELECT count(*) FROM jd_areas WHERE system_id=1 AND code_start<>40),
					(SELECT count(*) FROM jd_categories WHERE system_id=1 AND name IN
						('Banking','Rent & property','Appliances','Pets','Travel','School & activities','Bills & claims'))`).
				Scan(&areas, &ambiguous); err != nil {
				t.Fatal(err)
			}
			if areas != 4 || ambiguous != 0 {
				t.Fatalf("personal tree areas=%d ambiguous categories=%d, want 4 and 0", areas, ambiguous)
			}
			var housing, medicalBilling, money string
			if err := d.Read.QueryRowContext(t.Context(), `
				SELECT
					(SELECT name FROM jd_categories WHERE system_id=1 AND code=52),
					(SELECT name FROM jd_categories WHERE system_id=1 AND code=33),
					(SELECT name FROM jd_areas WHERE system_id=1 AND code_start=20)`).
				Scan(&housing, &medicalBilling, &money); err != nil {
				t.Fatal(err)
			}
			if housing != "Housing" || medicalBilling != "Medical billing" || money != "Money" {
				t.Fatalf("personal names = %q, %q, %q, want Housing, Medical billing, and Money",
					housing, medicalBilling, money)
			}
		})
	}
}

func TestBusinessPresetsAvoidUmbrellaCategories(t *testing.T) {
	for _, preset := range []string{"freelance", "smb_billing"} {
		t.Run(preset, func(t *testing.T) {
			d := openTestDB(t)
			if err := jd.ApplyPreset(t.Context(), d, slog.Default(), preset, jd.ApplyPresetOpts{SystemID: 1}); err != nil {
				t.Fatal(err)
			}
			var ambiguous int
			if err := d.Read.QueryRowContext(t.Context(), `
				SELECT count(*) FROM jd_categories
				WHERE system_id=1 AND name IN
					('Banking','Client records','Project records','Customer records','Vendor records',
					 'Receivables','Plans & agreements','Sign-off & payments')`).
				Scan(&ambiguous); err != nil {
				t.Fatal(err)
			}
			if ambiguous != 0 {
				t.Fatalf("umbrella categories = %d, want 0", ambiguous)
			}
			var collections, money string
			if err := d.Read.QueryRowContext(t.Context(), `
				SELECT
					(SELECT name FROM jd_categories WHERE system_id=1 AND code=14),
					(SELECT name FROM jd_areas WHERE system_id=1 AND code_start=30)`).
				Scan(&collections, &money); err != nil {
				t.Fatal(err)
			}
			if collections != "Collections & aging" || money != "Money" {
				t.Fatalf("business names = %q, %q, want Collections & aging and Money", collections, money)
			}
			if preset == "freelance" {
				var briefs, approvals string
				if err := d.Read.QueryRowContext(t.Context(), `
					SELECT
						(SELECT name FROM jd_categories WHERE system_id=1 AND code=22),
						(SELECT name FROM jd_categories WHERE system_id=1 AND code=25)`).
					Scan(&briefs, &approvals); err != nil {
					t.Fatal(err)
				}
				if briefs != "Briefs & plans" || approvals != "Approvals & sign-off" {
					t.Fatalf("freelance project category names = %q, %q, want Briefs & plans and Approvals & sign-off",
						briefs, approvals)
				}
			}
		})
	}
}

func TestApplyEveryPresetFromInboxOnlyBootstrap(t *testing.T) {
	for _, preset := range jd.Presets() {
		t.Run(preset.ID, func(t *testing.T) {
			d := openTestDB(t)
			ctx := context.Background()
			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
			if err := jd.EnsureBootstrapTree(ctx, d, log, jd.ModeJD, 1); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Write.ExecContext(ctx, `
				INSERT INTO users(id, email, display_name, role, created_at, updated_at)
				VALUES (1, 'u@t.local', 't', 'admin', 0, 0);
				INSERT INTO documents(system_id, owner_id, title, original_blob, original_size,
				                      jd_category_id, created_at, added_at, updated_at)
				VALUES (1, 1, 'waiting.pdf', 'sha-bootstrap', 1,
				        (SELECT id FROM jd_categories WHERE system = 1), 0, 0, 0)
			`); err != nil {
				t.Fatal(err)
			}
			if err := jd.ApplyPreset(ctx, d, log, preset.ID, jd.ApplyPresetOpts{SystemID: 1}); err != nil {
				t.Fatalf("apply from bootstrap: %v", err)
			}
			var onInbox int
			if err := d.Read.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM documents d
				JOIN jd_categories c ON c.id = d.jd_category_id
				WHERE c.system = 1
			`).Scan(&onInbox); err != nil {
				t.Fatal(err)
			}
			if onInbox != 1 {
				t.Fatalf("documents on new Inbox = %d, want 1", onInbox)
			}
		})
	}
}
