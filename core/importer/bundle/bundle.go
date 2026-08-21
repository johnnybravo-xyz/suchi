package bundle

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/slug"
)

// Options carries CLI flags to Run. Every field is documented; see
// Validate() for the invariants across them.
//
// Zero value is not runnable — BundleRoot is required always, OwnerEmail
// is required unless DryRun. Validate() enforces these so the CLI and
// any programmatic caller share one source of truth.
type Options struct {
	// BundleRoot is the path to the exporter output dir. Required.
	BundleRoot string

	// OwnerEmail resolves to a users row; imported documents land under
	// that user. Required unless DryRun is set.
	OwnerEmail string

	// DryRun parses + plans the import but does not write to the DB or CAS.
	// Owner resolution is skipped so the mode works before any user exists.
	DryRun bool

	// Category-resolution flags. At most ONE of Flat / MapJD / AutoJD may
	// be set — they are mutually exclusive strategies for picking a JD
	// category per imported document.
	//
	//   Flat=true         : every doc → inbox. No rules consulted.
	//   MapJD != nil      : first-match user rules; unmatched → inbox.
	//   AutoJD=true       : built-in heuristic rules (AutoMapping()).
	//   (none set)        : safe default — every doc → inbox.
	Flat   bool
	MapJD  *Mapping
	AutoJD bool

	// Report accumulates FULL / PARTIAL / FAILED entries per model class.
	// Optional — Run allocates one when nil. Callers pass in their own when
	// they want to write it to a file afterwards (see the --report CLI flag).
	Report *MigrationReport
}

// Validate returns an error if opts violates any invariant. Callers
// (CLI parsers, integration tests, agents driving the importer) should
// call this before Run — Run will call it too, but returning the error
// earlier gives better error messages next to the flag definitions.
func (opts Options) Validate() error {
	if opts.BundleRoot == "" {
		return errors.New("BundleRoot is required")
	}
	if !opts.DryRun && opts.OwnerEmail == "" {
		return errors.New("OwnerEmail is required unless DryRun is set")
	}
	picked := 0
	if opts.Flat {
		picked++
	}
	if opts.MapJD != nil {
		picked++
	}
	if opts.AutoJD {
		picked++
	}
	if picked > 1 {
		return errors.New("at most one of Flat, MapJD, AutoJD may be set — they are mutually exclusive category-resolution strategies")
	}
	return nil
}

// Report is what Run returns. Zero values are meaningful (0 tags means
// no tag rows, not "unknown").
type Report struct {
	Tags             int
	Correspondents   int
	DocumentTypes    int
	StoragePaths     int
	CustomFields     int
	Documents        int
	DocumentsSkipped int // legacy_id already imported
	Notes            int
	Blobs            int // count of Put calls (both original + archive)
	MappedByRule     int // documents whose JD category came from --map-jd (vs inbox fallback)
	Warnings         []string
}

// Run imports the bundle at opts.BundleRoot. Idempotent: reruns replay
// reference tables (upsert) and skip documents whose legacy_id
// is already present.
func Run(ctx context.Context, d *db.DB, cas *blob.CAS, log *slog.Logger, opts Options) (*Report, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	// Resolve the effective mapping up front. Precedence (highest wins):
	//   --flat  → nil (everything to inbox; JD resolution skipped)
	//   --map-jd → the user's file
	//   --auto-jd → the embedded AutoMapping()
	//   (nothing) → nil
	effective := opts.MapJD
	source := "user"
	if opts.Flat {
		effective = nil
		source = "flat"
	} else if effective == nil && opts.AutoJD {
		auto, err := AutoMapping()
		if err != nil {
			return nil, err
		}
		effective = auto
		source = "auto"
	} else if effective == nil {
		source = "none"
	}
	log = log.With("component", "import.bundle", "bundle", opts.BundleRoot, "dry_run", opts.DryRun, "map_source", source)
	rep := &Report{}
	mrep := opts.Report
	if mrep == nil {
		mrep = NewMigrationReport(opts.BundleRoot, "")
	}
	mrep.SetStrategy(source)

	// Resolve owner up front so we fail fast on a bad --owner-email.
	var ownerID int64
	if !opts.DryRun {
		if err := d.Read.QueryRowContext(ctx,
			`SELECT id FROM users WHERE email = ? AND disabled = 0`,
			opts.OwnerEmail).Scan(&ownerID); err != nil {
			return nil, fmt.Errorf("resolve owner %q: %w", opts.OwnerEmail, err)
		}
	}

	// Inbox pointer for uncategorized imports.
	inboxCat, err := jd.InboxCategoryID(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("resolve inbox category: %w", err)
	}

	// Read every manifest file into one slice. Order does not matter — we
	// bucket by model on the way in.
	objs, err := LoadManifests(opts.BundleRoot)
	if err != nil {
		return nil, err
	}

	// Bucket by model.
	buckets := map[string][]Object{}
	for _, o := range objs {
		buckets[o.Model] = append(buckets[o.Model], o)
	}

	// PK-remap tables: source PK → suchi PK. Populated by the reference-table
	// pass and consumed by the document pass.
	tagMap := map[int64]int64{}
	corMap := map[int64]int64{}
	dtMap := map[int64]int64{}
	spMap := map[int64]int64{}
	cfMap := map[int64]int64{}

	// Name lookup for --map-jd rules. Same PK space as the *Map remaps,
	// but carries the human-readable name the ruleset compares against.
	names := nameSets{
		tags:           map[int64]string{},
		correspondents: map[int64]string{},
		documentTypes:  map[int64]string{},
		storagePaths:   map[int64]string{},
	}

	// JD code → suchi jd_categories.id, for --map-jd category lookup.
	// Loaded once here so the per-doc resolve does not re-query.
	codeToCat, err := loadJDCodeMap(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("load jd code map: %w", err)
	}

	// ---------- reference tables (upsert-verbatim) ----------

	for _, o := range buckets[ModelTag] {
		var f TagFields
		if err := json.Unmarshal(o.Fields, &f); err != nil {
			return nil, fmt.Errorf("decode tag pk=%d: %w", o.PK, err)
		}
		id, err := upsertTag(ctx, d, opts.DryRun, f)
		if err != nil {
			return nil, err
		}
		tagMap[o.PK] = id
		names.tags[o.PK] = f.Name
		rep.Tags++
		mrep.Full(KindTag, f.Name, f.Name)
	}
	for _, o := range buckets[ModelCorrespondent] {
		var f CorrespondentFields
		if err := json.Unmarshal(o.Fields, &f); err != nil {
			return nil, fmt.Errorf("decode correspondent pk=%d: %w", o.PK, err)
		}
		id, err := upsertCorrespondent(ctx, d, opts.DryRun, f)
		if err != nil {
			return nil, err
		}
		corMap[o.PK] = id
		names.correspondents[o.PK] = f.Name
		rep.Correspondents++
		mrep.Full(KindCorrespondent, f.Name, f.Name)
	}
	for _, o := range buckets[ModelDocumentType] {
		var f DocumentTypeFields
		if err := json.Unmarshal(o.Fields, &f); err != nil {
			return nil, fmt.Errorf("decode document_type pk=%d: %w", o.PK, err)
		}
		id, err := upsertDocumentType(ctx, d, opts.DryRun, f)
		if err != nil {
			return nil, err
		}
		dtMap[o.PK] = id
		names.documentTypes[o.PK] = f.Name
		rep.DocumentTypes++
		mrep.Full(KindDocumentType, f.Name, f.Name)
	}
	for _, o := range buckets[ModelStoragePath] {
		var f StoragePathFields
		if err := json.Unmarshal(o.Fields, &f); err != nil {
			return nil, fmt.Errorf("decode storage_path pk=%d: %w", o.PK, err)
		}
		id, err := upsertStoragePath(ctx, d, opts.DryRun, f)
		if err != nil {
			return nil, err
		}
		spMap[o.PK] = id
		names.storagePaths[o.PK] = f.Name
		rep.StoragePaths++
		mrep.Full(KindStoragePath, f.Name, f.Name)
	}
	for _, o := range buckets[ModelCustomField] {
		var f CustomFieldFields
		if err := json.Unmarshal(o.Fields, &f); err != nil {
			return nil, fmt.Errorf("decode custom_field pk=%d: %w", o.PK, err)
		}
		id, err := upsertCustomField(ctx, d, opts.DryRun, f)
		if err != nil {
			return nil, err
		}
		cfMap[o.PK] = id
		rep.CustomFields++
		mrep.Full(KindCustomField, f.Name, f.Name)
	}

	// Index custom-field instances by document PK for the document pass.
	instancesByDoc := map[int64][]CustomFieldInstance{}
	for _, o := range buckets[ModelFieldInstance] {
		var f CustomFieldInstance
		if err := json.Unmarshal(o.Fields, &f); err != nil {
			return nil, fmt.Errorf("decode field_instance pk=%d: %w", o.PK, err)
		}
		instancesByDoc[f.Document] = append(instancesByDoc[f.Document], f)
	}
	// Same for notes.
	notesByDoc := map[int64][]NoteFields{}
	for _, o := range buckets[ModelNote] {
		var f NoteFields
		if err := json.Unmarshal(o.Fields, &f); err != nil {
			return nil, fmt.Errorf("decode note pk=%d: %w", o.PK, err)
		}
		notesByDoc[f.Document] = append(notesByDoc[f.Document], f)
	}

	// ---------- documents ----------

	for _, o := range buckets[ModelDocument] {
		var f DocumentFields
		if err := json.Unmarshal(o.Fields, &f); err != nil {
			return nil, fmt.Errorf("decode document pk=%d: %w", o.PK, err)
		}
		// Category resolution:
		//   --flat        → inbox, always. Never consult a rule.
		//   --map-jd + rule hit → jd_categories.id for that code (if it
		//                          resolves; else warn + inbox).
		//   otherwise     → inbox.
		catID := inboxCat
		mapped := false
		if effective != nil {
			if code := effective.Resolve(f, names); code != 0 {
				if id, ok := codeToCat[code]; ok {
					catID = id
					mapped = true
				} else {
					rep.Warnings = append(rep.Warnings,
						fmt.Sprintf("doc pk=%d: rule matched JD code %d but no such category — falling back to inbox", o.PK, code))
				}
			}
		}
		res, err := importDoc(ctx, d, cas, log, opts, docInput{
			LegacyID:  o.PK,
			Fields:    f,
			OwnerID:   ownerID,
			InboxCat:  catID,
			TagMap:    tagMap,
			CorMap:    corMap,
			DTMap:     dtMap,
			SPMap:     spMap,
			CFMap:     cfMap,
			Instances: instancesByDoc[o.PK],
			Notes:     notesByDoc[o.PK],
		})
		if err != nil {
			return nil, err
		}
		switch res.status {
		case docImported:
			rep.Documents++
			rep.Blobs += res.blobs
			rep.Notes += res.notes
			if mapped {
				rep.MappedByRule++
			}
			mrep.Full(KindDocument, f.Title, f.Title)
			for i := 0; i < res.notes; i++ {
				mrep.Full(KindNote, fmt.Sprintf("note on %q", f.Title), f.Title)
			}
		case docSkipped:
			rep.DocumentsSkipped++
		}
		if res.warn != "" {
			rep.Warnings = append(rep.Warnings, res.warn)
		}
	}

	// ---------- workflows + saved views ----------

	if _, err := ImportWorkflows(ctx, d, log, objs, opts.DryRun, tagMap, corMap, dtMap, spMap, cfMap, mrep); err != nil {
		return nil, fmt.Errorf("import workflows: %w", err)
	}
	if _, err := ImportSavedViews(ctx, d, log, objs, opts.DryRun, ownerID, tagMap, corMap, dtMap, mrep); err != nil {
		return nil, fmt.Errorf("import saved views: %w", err)
	}

	mrep.Finalize()

	log.Info("import.bundle.done",
		"tags", rep.Tags,
		"correspondents", rep.Correspondents,
		"document_types", rep.DocumentTypes,
		"storage_paths", rep.StoragePaths,
		"custom_fields", rep.CustomFields,
		"documents", rep.Documents,
		"documents_skipped", rep.DocumentsSkipped,
		"mapped_by_rule", rep.MappedByRule,
		"notes", rep.Notes,
		"blobs_written", rep.Blobs,
		"warnings", len(rep.Warnings),
	)
	return rep, nil
}

// docInput bundles what importDoc needs. Grouping keeps the signature honest.
type docInput struct {
	LegacyID  int64
	Fields    DocumentFields
	OwnerID   int64
	InboxCat  int64
	TagMap    map[int64]int64
	CorMap    map[int64]int64
	DTMap     map[int64]int64
	SPMap     map[int64]int64
	CFMap     map[int64]int64
	Instances []CustomFieldInstance
	Notes     []NoteFields
}

type docStatus int

const (
	docImported docStatus = iota
	docSkipped
)

type docResult struct {
	status docStatus
	blobs  int
	notes  int
	warn   string
}

func importDoc(ctx context.Context, d *db.DB, cas *blob.CAS, log *slog.Logger, opts Options, in docInput) (docResult, error) {
	log = log.With("legacy_pk", in.LegacyID)

	// Idempotency: if this imported doc has already been imported, skip.
	if !opts.DryRun {
		var have int
		err := d.Read.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM documents WHERE legacy_id = ?`,
			in.LegacyID).Scan(&have)
		if err != nil {
			return docResult{}, err
		}
		if have > 0 {
			log.Debug("import.doc.skip", "reason", "already_imported")
			return docResult{status: docSkipped}, nil
		}
	}

	// Blobs. The original is required; the archive is optional.
	origPath, archPath, err := FilePaths(opts.BundleRoot, in.Fields)
	if err != nil {
		return docResult{}, err
	}
	origInfo, err := os.Stat(origPath)
	if err != nil {
		return docResult{}, fmt.Errorf("original file %s: %w", origPath, err)
	}

	var origRef, archRef struct {
		SHA256 string
		Size   int64
	}
	blobs := 0

	if !opts.DryRun {
		f, err := os.Open(origPath)
		if err != nil {
			return docResult{}, err
		}
		ref, err := cas.Put(f)
		f.Close()
		if err != nil {
			return docResult{}, fmt.Errorf("cas put original: %w", err)
		}
		origRef.SHA256, origRef.Size = ref.SHA256, ref.Size
		blobs++
	} else {
		origRef.Size = origInfo.Size()
	}

	if archPath != "" {
		if _, err := os.Stat(archPath); err == nil {
			if !opts.DryRun {
				f, err := os.Open(archPath)
				if err != nil {
					return docResult{}, err
				}
				ref, err := cas.Put(f)
				f.Close()
				if err != nil {
					return docResult{}, fmt.Errorf("cas put archive: %w", err)
				}
				archRef.SHA256, archRef.Size = ref.SHA256, ref.Size
				blobs++
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return docResult{}, fmt.Errorf("stat archive: %w", err)
		}
	}

	if opts.DryRun {
		return docResult{status: docImported, blobs: blobs}, nil
	}

	// Single transaction: doc row + tag junctions + custom-field values + notes.
	notesWritten := 0
	err = d.WriteTx(ctx, func(tx *sql.Tx) error {
		created := ParseTime(in.Fields.Created)
		added := ParseTime(in.Fields.Added)
		updated := ParseTime(in.Fields.Modified)
		if created == 0 {
			created = time.Now().Unix()
		}
		if added == 0 {
			added = created
		}
		if updated == 0 {
			updated = added
		}

		var archiveBlob any
		var archiveSize any
		if archRef.SHA256 != "" {
			archiveBlob = archRef.SHA256
			archiveSize = archRef.Size
		}
		var correspondentID any
		if in.Fields.Correspondent != nil {
			if v, ok := in.CorMap[*in.Fields.Correspondent]; ok {
				correspondentID = v
			}
		}
		var documentTypeID any
		if in.Fields.DocumentType != nil {
			if v, ok := in.DTMap[*in.Fields.DocumentType]; ok {
				documentTypeID = v
			}
		}
		var storagePathID any
		if in.Fields.StoragePath != nil {
			if v, ok := in.SPMap[*in.Fields.StoragePath]; ok {
				storagePathID = v
			}
		}
		var asn any
		if in.Fields.ArchiveSerialNo != nil {
			asn = *in.Fields.ArchiveSerialNo
		}
		var content any
		if in.Fields.Content != "" {
			content = in.Fields.Content
		}
		var mime any
		if in.Fields.MimeType != "" {
			mime = in.Fields.MimeType
		}

		res, err := tx.ExecContext(ctx, `
			INSERT INTO documents (
				owner_id, original_blob, original_size, archive_blob, archive_size,
				title, content, mime_type, correspondent_id, document_type_id, storage_path_id,
				jd_category_id, legacy_id, archive_serial_number,
				added_at, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			in.OwnerID, origRef.SHA256, origRef.Size, archiveBlob, archiveSize,
			in.Fields.Title, content, mime, correspondentID, documentTypeID, storagePathID,
			in.InboxCat, in.LegacyID, asn,
			added, created, updated,
		)
		if err != nil {
			return fmt.Errorf("insert document: %w", err)
		}
		docID, err := res.LastInsertId()
		if err != nil {
			return err
		}

		// Tag junctions.
		for _, tagPK := range in.Fields.Tags {
			tagID, ok := in.TagMap[tagPK]
			if !ok {
				continue // manifest inconsistency — log-worthy but non-fatal
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO document_tags(document_id, tag_id) VALUES (?, ?)`,
				docID, tagID); err != nil {
				return fmt.Errorf("tag junction: %w", err)
			}
		}

		// Custom-field values. value is JSON-typed; suchi splits
		// into typed columns. We attempt integer → number → text in order.
		for _, cfi := range in.Instances {
			fieldID, ok := in.CFMap[cfi.Field]
			if !ok {
				continue
			}
			if err := writeCustomFieldValue(ctx, tx, docID, fieldID, cfi.EffectiveValue()); err != nil {
				return err
			}
		}

		// Notes. user is optional; NULL is fine.
		for _, n := range in.Notes {
			// The source-side user PK does not remap to a suchi user row; the
			// simplest correct behaviour is to attribute the note to the
			// import's OwnerID so the FOREIGN KEY constraint holds. If the
			// note carries no user at all, use OwnerID for the same reason.
			userID := in.OwnerID
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO notes(document_id, user_id, note, created_at)
				VALUES (?, ?, ?, ?)
			`, docID, userID, n.Note, ParseTime(n.Created)); err != nil {
				return fmt.Errorf("note insert: %w", err)
			}
			notesWritten++
		}

		return nil
	})
	if err != nil {
		return docResult{}, err
	}

	return docResult{status: docImported, blobs: blobs, notes: notesWritten}, nil
}

func writeCustomFieldValue(ctx context.Context, tx *sql.Tx, docID, fieldID int64, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	// Try integer first.
	var i int64
	if err := json.Unmarshal(raw, &i); err == nil {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO document_custom_field_values(document_id, field_id, value_int)
			VALUES (?, ?, ?)
		`, docID, fieldID, i)
		return err
	}
	// Then float.
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO document_custom_field_values(document_id, field_id, value_number)
			VALUES (?, ?, ?)
		`, docID, fieldID, f)
		return err
	}
	// Then bool.
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		v := 0
		if b {
			v = 1
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO document_custom_field_values(document_id, field_id, value_bool)
			VALUES (?, ?, ?)
		`, docID, fieldID, v)
		return err
	}
	// Fall back to string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO document_custom_field_values(document_id, field_id, value_text)
			VALUES (?, ?, ?)
		`, docID, fieldID, s)
		return err
	}
	// Ignore unrecognized shapes rather than fail the whole import for
	// one exotic value. The report should carry a warning for these.
	return nil
}

// ---------- reference-table upserts ----------

func upsertTag(ctx context.Context, d *db.DB, dry bool, f TagFields) (int64, error) {
	if dry {
		return 0, nil
	}
	now := time.Now().Unix()
	var id int64
	err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tags(name, slug, color, matching_algorithm, match, is_insensitive, is_inbox_tag, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET
				slug = excluded.slug,
				color = excluded.color,
				matching_algorithm = excluded.matching_algorithm,
				match = excluded.match,
				is_insensitive = excluded.is_insensitive,
				is_inbox_tag = excluded.is_inbox_tag,
				updated_at = excluded.updated_at
		`, f.Name, defaultSlug(f.Slug, f.Name), defaultString(f.Color, "#a6cee3"),
			f.MatchAlg, f.Match, boolInt(f.Insensitive), boolInt(f.IsInboxTag), now, now); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT id FROM tags WHERE name = ?`, f.Name).Scan(&id)
	})
	return id, err
}

func upsertCorrespondent(ctx context.Context, d *db.DB, dry bool, f CorrespondentFields) (int64, error) {
	if dry {
		return 0, nil
	}
	now := time.Now().Unix()
	var id int64
	err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO correspondents(name, slug, matching_algorithm, match, is_insensitive, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET
				slug = excluded.slug,
				matching_algorithm = excluded.matching_algorithm,
				match = excluded.match,
				is_insensitive = excluded.is_insensitive,
				updated_at = excluded.updated_at
		`, f.Name, defaultSlug(f.Slug, f.Name), f.MatchAlg, f.Match, boolInt(f.Insensitive), now, now); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT id FROM correspondents WHERE name = ?`, f.Name).Scan(&id)
	})
	return id, err
}

func upsertDocumentType(ctx context.Context, d *db.DB, dry bool, f DocumentTypeFields) (int64, error) {
	if dry {
		return 0, nil
	}
	now := time.Now().Unix()
	var id int64
	err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO document_types(name, slug, matching_algorithm, match, is_insensitive, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET
				slug = excluded.slug,
				matching_algorithm = excluded.matching_algorithm,
				match = excluded.match,
				is_insensitive = excluded.is_insensitive,
				updated_at = excluded.updated_at
		`, f.Name, defaultSlug(f.Slug, f.Name), f.MatchAlg, f.Match, boolInt(f.Insensitive), now, now); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT id FROM document_types WHERE name = ?`, f.Name).Scan(&id)
	})
	return id, err
}

func upsertStoragePath(ctx context.Context, d *db.DB, dry bool, f StoragePathFields) (int64, error) {
	if dry {
		return 0, nil
	}
	now := time.Now().Unix()
	var id int64
	err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO storage_paths(name, slug, path, matching_algorithm, match, is_insensitive, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET
				slug = excluded.slug,
				path = excluded.path,
				matching_algorithm = excluded.matching_algorithm,
				match = excluded.match,
				is_insensitive = excluded.is_insensitive,
				updated_at = excluded.updated_at
		`, f.Name, defaultSlug(f.Slug, f.Name), f.Path, f.MatchAlg, f.Match, boolInt(f.Insensitive), now, now); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT id FROM storage_paths WHERE name = ?`, f.Name).Scan(&id)
	})
	return id, err
}

// source data_type enum → suchi data_type string. paperless v2 used ints;
// v3 emits string enums directly.
var bundleCFDataTypeInt = map[int]string{
	1: "text",
	2: "date",
	3: "bool",
	4: "number",
	5: "monetary",
	6: "documentlink",
	7: "url",
	8: "select",
}

var bundleCFDataTypeString = map[string]string{
	"string":       "text",
	"text":         "text",
	"date":         "date",
	"boolean":      "bool",
	"bool":         "bool",
	"integer":      "number",
	"float":        "number",
	"number":       "number",
	"monetary":     "monetary",
	"documentlink": "documentlink",
	"url":          "url",
	"select":       "select",
}

func upsertCustomField(ctx context.Context, d *db.DB, dry bool, f CustomFieldFields) (int64, error) {
	if dry {
		return 0, nil
	}
	dt := "text"
	if len(f.DataType) > 0 {
		var s string
		var i int
		if err := json.Unmarshal(f.DataType, &s); err == nil {
			if v, ok := bundleCFDataTypeString[s]; ok {
				dt = v
			}
		} else if err := json.Unmarshal(f.DataType, &i); err == nil {
			if v, ok := bundleCFDataTypeInt[i]; ok {
				dt = v
			}
		}
	}
	extra := "{}"
	if len(f.ExtraData) > 0 {
		extra = string(f.ExtraData)
	}
	now := time.Now().Unix()
	var id int64
	err := d.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO custom_fields(name, data_type, extra_data, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET
				data_type = excluded.data_type,
				extra_data = excluded.extra_data,
				updated_at = excluded.updated_at
		`, f.Name, dt, extra, now, now); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT id FROM custom_fields WHERE name = ?`, f.Name).Scan(&id)
	})
	return id, err
}

// loadJDCodeMap returns a map from JD code (jd_categories.code) → row id.
// Called once at import start; --map-jd resolves rule categories through
// this map instead of round-tripping the DB per document.
func loadJDCodeMap(ctx context.Context, d *db.DB) (map[int]int64, error) {
	rows, err := d.Read.QueryContext(ctx, `SELECT code, id FROM jd_categories`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]int64{}
	for rows.Next() {
		var code int
		var id int64
		if err := rows.Scan(&code, &id); err != nil {
			return nil, err
		}
		out[code] = id
	}
	return out, rows.Err()
}

func defaultString(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// defaultSlug uses the slug when non-empty, otherwise derives from name.
func defaultSlug(s, name string) string {
	if s != "" {
		return s
	}
	return slug.Make(name)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
