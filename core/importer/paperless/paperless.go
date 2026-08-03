package paperless

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/suchi-dms/suchi/core/blob"
	"github.com/suchi-dms/suchi/core/db"
	"github.com/suchi-dms/suchi/core/jd"
)

// Options carries CLI flags to Run. Zero-value is not useful; the caller
// (main.go) fills in DataDir + OwnerEmail.
type Options struct {
	BundleRoot string // path to the exporter output dir
	OwnerEmail string // must resolve to a users row; imported docs land under this user
	DryRun     bool   // parse+plan but do not write
}

// Report is what Run returns. Zero values are meaningful (0 tags means
// no tag rows, not "unknown").
type Report struct {
	Tags            int
	Correspondents  int
	DocumentTypes   int
	StoragePaths    int
	CustomFields    int
	Documents       int
	DocumentsSkipped int // paperless_id_legacy already imported
	Notes           int
	Blobs           int // count of Put calls (both original + archive)
	Warnings        []string
}

// Run imports the bundle at opts.BundleRoot. Idempotent: reruns replay
// reference tables (upsert) and skip documents whose paperless_id_legacy
// is already present.
func Run(ctx context.Context, d *db.DB, cas *blob.CAS, log *slog.Logger, opts Options) (*Report, error) {
	log = log.With("component", "import.paperless", "bundle", opts.BundleRoot, "dry_run", opts.DryRun)
	rep := &Report{}

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

	// PK-remap tables: Paperless PK → suchi PK. Populated by phase 1
	// (reference tables), consumed by phase 2 (documents).
	tagMap := map[int64]int64{}
	corMap := map[int64]int64{}
	dtMap := map[int64]int64{}
	spMap := map[int64]int64{}
	cfMap := map[int64]int64{}

	// ---------- phase 1: reference tables (upsert-verbatim) ----------

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
		rep.Tags++
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
		rep.Correspondents++
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
		rep.DocumentTypes++
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
		rep.StoragePaths++
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
	}

	// Index custom-field instances by document PK for phase-2 lookup.
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

	// ---------- phase 2: documents ----------

	for _, o := range buckets[ModelDocument] {
		var f DocumentFields
		if err := json.Unmarshal(o.Fields, &f); err != nil {
			return nil, fmt.Errorf("decode document pk=%d: %w", o.PK, err)
		}
		res, err := importDoc(ctx, d, cas, log, opts, docInput{
			PaperlessID: o.PK,
			Fields:      f,
			OwnerID:     ownerID,
			InboxCat:    inboxCat,
			TagMap:      tagMap,
			CorMap:      corMap,
			DTMap:       dtMap,
			SPMap:       spMap,
			CFMap:       cfMap,
			Instances:   instancesByDoc[o.PK],
			Notes:       notesByDoc[o.PK],
		})
		if err != nil {
			return nil, err
		}
		switch res.status {
		case docImported:
			rep.Documents++
			rep.Blobs += res.blobs
			rep.Notes += res.notes
		case docSkipped:
			rep.DocumentsSkipped++
		}
		if res.warn != "" {
			rep.Warnings = append(rep.Warnings, res.warn)
		}
	}

	log.Info("import.paperless.done",
		"tags", rep.Tags,
		"correspondents", rep.Correspondents,
		"document_types", rep.DocumentTypes,
		"storage_paths", rep.StoragePaths,
		"custom_fields", rep.CustomFields,
		"documents", rep.Documents,
		"documents_skipped", rep.DocumentsSkipped,
		"notes", rep.Notes,
		"blobs_written", rep.Blobs,
		"warnings", len(rep.Warnings),
	)
	return rep, nil
}

// docInput bundles what importDoc needs. Grouping keeps the signature honest.
type docInput struct {
	PaperlessID int64
	Fields      DocumentFields
	OwnerID     int64
	InboxCat    int64
	TagMap      map[int64]int64
	CorMap      map[int64]int64
	DTMap       map[int64]int64
	SPMap       map[int64]int64
	CFMap       map[int64]int64
	Instances   []CustomFieldInstance
	Notes       []NoteFields
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
	log = log.With("paperless_pk", in.PaperlessID)

	// Idempotency: if this Paperless doc has already been imported, skip.
	if !opts.DryRun {
		var have int
		err := d.Read.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM documents WHERE paperless_id_legacy = ?`,
			in.PaperlessID).Scan(&have)
		if err != nil {
			return docResult{}, err
		}
		if have > 0 {
			log.Debug("import.doc.skip", "reason", "already_imported")
			return docResult{status: docSkipped}, nil
		}
	}

	// Blobs. The original is required; the archive is optional.
	origPath, archPath := FilePaths(opts.BundleRoot, in.Fields)
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
				jd_category_id, paperless_id_legacy, archive_serial_number,
				added_at, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			in.OwnerID, origRef.SHA256, origRef.Size, archiveBlob, archiveSize,
			in.Fields.Title, content, mime, correspondentID, documentTypeID, storagePathID,
			in.InboxCat, in.PaperlessID, asn,
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

		// Custom-field values. Paperless value is JSON-typed; suchi splits
		// into typed columns. We attempt integer → number → text in order.
		for _, cfi := range in.Instances {
			fieldID, ok := in.CFMap[cfi.Field]
			if !ok {
				continue
			}
			if err := writeCustomFieldValue(ctx, tx, docID, fieldID, cfi.Value); err != nil {
				return err
			}
		}

		// Notes. user is optional; NULL is fine.
		for _, n := range in.Notes {
			var userID any
			if n.User != nil {
				userID = *n.User
			}
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
		`, f.Name, f.Slug, defaultString(f.Color, "#a6cee3"),
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
		`, f.Name, f.Slug, f.MatchAlg, f.Match, boolInt(f.Insensitive), now, now); err != nil {
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
		`, f.Name, f.Slug, f.MatchAlg, f.Match, boolInt(f.Insensitive), now, now); err != nil {
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
		`, f.Name, f.Slug, f.Path, f.MatchAlg, f.Match, boolInt(f.Insensitive), now, now); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT id FROM storage_paths WHERE name = ?`, f.Name).Scan(&id)
	})
	return id, err
}

// Paperless data_type enum → suchi data_type string.
var paperlessCFDataType = map[int]string{
	1: "text",
	2: "date",
	3: "bool",
	4: "number",
	5: "monetary",
	6: "documentlink",
	7: "url",
	8: "select",
}

func upsertCustomField(ctx context.Context, d *db.DB, dry bool, f CustomFieldFields) (int64, error) {
	if dry {
		return 0, nil
	}
	dt, ok := paperlessCFDataType[f.DataType]
	if !ok {
		dt = "text" // best-effort fallback
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

func defaultString(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
