// Apply — evaluate every automation whose trigger includes the given
// event type, filter by conditions, and run actions in order.
//
// Called from postingest.postContentSteps for `document_added`, from
// PATCH /api/documents/{id} for `document_updated`, and from
// upload-time for `consumption` (batch 2).

package automations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/customfield"
	"github.com/johnnybravo-xyz/suchi/core/db"
)

// Context carries the event-time facts an automation needs. Populated
// by the caller — the engine never queries side-channel data.
//
// The Email* fields are set by mail-intake producers (emailwatch) from
// the parsed message metadata. HasEmail discriminates "this doc came
// from an email producer that populated the Email* fields" from "this
// doc came from a non-email producer, so email filters should reject" —
// without it, an unset EmailHasAttachment on a non-email doc would
// look identical to false and could accidentally match a
// require-no-attachment filter.
type Context struct {
	DocID              int64
	SourcePath         string // consumption trigger only
	Filename           string // consumption trigger only
	MailRuleID         int64  // consumption via mail intake
	HasEmail           bool
	EmailFrom          string
	EmailSubject       string
	EmailFolder        string
	EmailHasAttachment bool
}

// docSnapshot is a light in-memory read of the metadata an automation
// filter can key on. Loaded once per
// evaluation.
type docSnapshot struct {
	Title           string
	Content         string
	TagIDs          map[int64]bool
	CorrespondentID sql.NullInt64
	DocumentTypeID  sql.NullInt64
}

// ApplyOnDocumentAdded is the postingest hook. Every enabled automation
// with a document_added trigger evaluates against the doc; matching
// triggers run their actions in one write tx.
//
// Action failures roll back that automation and return to the caller so the
// surrounding job or request can report the failure accurately.
func ApplyOnDocumentAdded(ctx context.Context, d *db.DB, log *slog.Logger, docID int64) error {
	_, err := ApplyOnDocumentAddedCount(ctx, d, log, docID)
	return err
}

// ApplyOnDocumentAddedCount is used by explicit refiles, where callers need
// to report how many automations matched the document.
func ApplyOnDocumentAddedCount(ctx context.Context, d *db.DB, log *slog.Logger, docID int64) (int, error) {
	s := &Store{DB: d}
	atms, err := s.ByTrigger(ctx, TriggerDocumentAdded)
	if err != nil {
		return 0, fmt.Errorf("automations: load automations: %w", err)
	}
	if len(atms) == 0 {
		return 0, nil
	}
	snap, err := loadSnapshot(ctx, d, docID)
	if err != nil {
		return 0, fmt.Errorf("automations: snapshot: %w", err)
	}
	evCtx := Context{DocID: docID}
	return runMatching(ctx, d, log, atms, evCtx, snap, TriggerDocumentAdded)
}

// ApplyOnDocumentUpdated mirrors the above for the update trigger.
// Called from PATCH /api/documents/{id} handlers after the write lands.
func ApplyOnDocumentUpdated(ctx context.Context, d *db.DB, log *slog.Logger, docID int64) error {
	s := &Store{DB: d}
	atms, err := s.ByTrigger(ctx, TriggerDocumentUpdated)
	if err != nil {
		return fmt.Errorf("automations: load automations: %w", err)
	}
	if len(atms) == 0 {
		return nil
	}
	snap, err := loadSnapshot(ctx, d, docID)
	if err != nil {
		return fmt.Errorf("automations: snapshot: %w", err)
	}
	evCtx := Context{DocID: docID}
	_, err = runMatching(ctx, d, log, atms, evCtx, snap, TriggerDocumentUpdated)
	return err
}

// ApplyOnConsumption runs consumption-trigger automations. Producers
// (upload API, fs-watch, mail-intake) call this at the start of the
// post-ingest handler with whatever context they have — filename,
// source path, mail-rule id. Any missing field disables the
// corresponding filter without erroring.
func ApplyOnConsumption(ctx context.Context, d *db.DB, log *slog.Logger, docID int64, evCtx Context) error {
	s := &Store{DB: d}
	atms, err := s.ByTrigger(ctx, TriggerConsumption)
	if err != nil {
		return fmt.Errorf("automations: load automations: %w", err)
	}
	if len(atms) == 0 {
		return nil
	}
	snap, err := loadSnapshot(ctx, d, docID)
	if err != nil {
		return fmt.Errorf("automations: snapshot: %w", err)
	}
	evCtx.DocID = docID
	_, err = runMatching(ctx, d, log, atms, evCtx, snap, TriggerConsumption)
	return err
}

func runMatching(ctx context.Context, d *db.DB, log *slog.Logger,
	atms []Automation, evCtx Context, snap *docSnapshot, t TriggerType) (int, error) {

	matchedCount := 0
	for _, atm := range atms {
		matched := false
		for _, tr := range atm.Triggers {
			if tr.Type != t {
				continue
			}
			if matchesTrigger(tr, snap, evCtx) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		matchedCount++

		alog := log.With("automation.id", atm.ID, "automation.name", atm.Name, "doc_id", evCtx.DocID)
		if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
			for _, a := range atm.Actions {
				if err := runAction(ctx, tx, evCtx.DocID, a); err != nil {
					return fmt.Errorf("action %q: %w", a.Kind, err)
				}
			}
			return nil
		}); err != nil {
			alog.Error("automations.run.error", "err", err.Error())
			return matchedCount, fmt.Errorf("automation %d (%q): %w", atm.ID, atm.Name, err)
		}
	}
	return matchedCount, nil
}

func matchesTrigger(tr Trigger, snap *docSnapshot, evCtx Context) bool {
	// Path/filename filters — consumption trigger only. If either is set
	// on a document_added/updated trigger it's a no-op (evCtx fields are
	// zero) and the trigger won't match. That's intentional — mixing
	// consumption filters with post-ingest triggers is a config bug.
	if tr.FilterPath != "" {
		ok, _ := filepath.Match(tr.FilterPath, evCtx.SourcePath)
		if !ok {
			return false
		}
	}
	if tr.FilterFilename != "" {
		ok, _ := filepath.Match(tr.FilterFilename, evCtx.Filename)
		if !ok {
			return false
		}
	}
	if tr.FilterMailRuleID != 0 && tr.FilterMailRuleID != evCtx.MailRuleID {
		return false
	}
	if tr.FilterTagID != 0 && !snap.TagIDs[tr.FilterTagID] {
		return false
	}
	if tr.FilterCorrID != 0 {
		if !snap.CorrespondentID.Valid || snap.CorrespondentID.Int64 != tr.FilterCorrID {
			return false
		}
	}
	if tr.FilterDocTypeID != 0 {
		if !snap.DocumentTypeID.Valid || snap.DocumentTypeID.Int64 != tr.FilterDocTypeID {
			return false
		}
	}
	if tr.FilterTitleRE != "" {
		re, err := regexp.Compile("(?i)" + tr.FilterTitleRE)
		if err != nil || !re.MatchString(snap.Title) {
			return false
		}
	}
	if tr.FilterContentRE != "" {
		re, err := regexp.Compile("(?i)" + tr.FilterContentRE)
		if err != nil {
			return false
		}
		if !re.MatchString(snap.Content) {
			return false
		}
	}
	// Email filters. If any of the four is set, the doc must have
	// been produced by a mail-intake path (HasEmail=true) — otherwise
	// filter fails. Mirrors filepath.Match semantics for from/subject
	// (glob) to stay consistent with FilterFilename above; folder is
	// literal equality; has_attachment is three-state via *bool.
	if tr.FilterEmailFrom != "" {
		if !evCtx.HasEmail {
			return false
		}
		ok, _ := filepath.Match(tr.FilterEmailFrom, evCtx.EmailFrom)
		if !ok {
			return false
		}
	}
	if tr.FilterEmailSubject != "" {
		if !evCtx.HasEmail {
			return false
		}
		ok, _ := filepath.Match(tr.FilterEmailSubject, evCtx.EmailSubject)
		if !ok {
			return false
		}
	}
	if tr.FilterEmailFolder != "" {
		if !evCtx.HasEmail {
			return false
		}
		if tr.FilterEmailFolder != evCtx.EmailFolder {
			return false
		}
	}
	if tr.FilterEmailHasAttachment != nil {
		if !evCtx.HasEmail {
			return false
		}
		if *tr.FilterEmailHasAttachment != evCtx.EmailHasAttachment {
			return false
		}
	}
	return true
}

func loadSnapshot(ctx context.Context, d *db.DB, docID int64) (*docSnapshot, error) {
	var s docSnapshot
	var content sql.NullString
	err := d.Read.QueryRowContext(ctx, `
		SELECT COALESCE(title, ''), content, correspondent_id, document_type_id
		FROM documents WHERE id = ?
	`, docID).Scan(&s.Title, &content, &s.CorrespondentID, &s.DocumentTypeID)
	if err != nil {
		return nil, err
	}
	if content.Valid {
		s.Content = content.String
	}

	rows, err := d.Read.QueryContext(ctx,
		`SELECT tag_id FROM document_tags WHERE document_id = ?`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	s.TagIDs = map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		s.TagIDs[id] = true
	}
	return &s, rows.Err()
}

// runAction dispatches on Kind. Metadata actions converge when rerun.
func runAction(ctx context.Context, tx *sql.Tx, docID int64, a Action) error {
	switch a.Kind {
	case "assign_title":
		tpl, _ := a.Params["template"].(string)
		if tpl == "" {
			return errors.New("assign_title: template required")
		}
		title, err := expandTitle(ctx, tx, docID, tpl)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE documents SET title = ? WHERE id = ?`, title, docID)
		return err

	case "assign_tags":
		ids := intList(a.Params["tag_ids"])
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO document_tags(document_id, tag_id) VALUES (?, ?)
				 ON CONFLICT(document_id, tag_id) DO UPDATE SET classifier_owned = 0`,
				docID, id); err != nil {
				return err
			}
		}
		return nil

	case "assign_correspondent":
		id := intVal(a.Params["correspondent_id"])
		if id == 0 {
			return errors.New("assign_correspondent: correspondent_id required")
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE documents SET correspondent_id = ? WHERE id = ?`, id, docID)
		return err

	case "assign_document_type":
		id := intVal(a.Params["document_type_id"])
		if id == 0 {
			return errors.New("assign_document_type: document_type_id required")
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE documents SET document_type_id = ? WHERE id = ?`, id, docID)
		return err

	case "assign_jd_category":
		id := intVal(a.Params["jd_category_id"])
		if id == 0 {
			return errors.New("assign_jd_category: jd_category_id required")
		}
		// Verify the category exists so a stale automation doesn't
		// silently move docs to a deleted category id and dangle the FK.
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM jd_categories WHERE id = ?`, id).Scan(&exists); err != nil {
			return fmt.Errorf("assign_jd_category: unknown category id %d", id)
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE documents SET jd_category_id = ? WHERE id = ?`, id, docID)
		return err

	case "assign_storage_path":
		id := intVal(a.Params["storage_path_id"])
		if id == 0 {
			return errors.New("assign_storage_path: storage_path_id required")
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE documents SET storage_path_id = ? WHERE id = ?`, id, docID)
		return err

	case "assign_owner":
		id := intVal(a.Params["owner_id"])
		if id == 0 {
			return errors.New("assign_owner: owner_id required")
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE documents SET owner_id = ? WHERE id = ?`, id, docID)
		return err

	case "remove_tags":
		ids := intList(a.Params["tag_ids"])
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM document_tags WHERE document_id = ? AND tag_id = ?`,
				docID, id); err != nil {
				return err
			}
		}
		return nil

	case "remove_document_type":
		_, err := tx.ExecContext(ctx,
			`UPDATE documents SET document_type_id = NULL WHERE id = ?`, docID)
		return err

	case "remove_storage_path":
		_, err := tx.ExecContext(ctx,
			`UPDATE documents SET storage_path_id = NULL WHERE id = ?`, docID)
		return err

	case "remove_correspondents":
		// The doc has one primary correspondent_id column and, via
		// document_correspondents, zero or more secondary correspondents
		// with roles. This action clears both — matches the "remove all"
		// intent the config typically wants.
		ids := intList(a.Params["correspondent_ids"])
		if len(ids) == 0 {
			// No ids given → clear the primary and every junction row.
			if _, err := tx.ExecContext(ctx,
				`UPDATE documents SET correspondent_id = NULL WHERE id = ?`, docID); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx,
				`DELETE FROM document_correspondents WHERE document_id = ?`, docID)
			return err
		}
		// With ids: unset primary if it matches; drop junction rows for
		// those correspondent ids.
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx,
				`UPDATE documents SET correspondent_id = NULL WHERE id = ? AND correspondent_id = ?`,
				docID, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM document_correspondents WHERE document_id = ? AND correspondent_id = ?`,
				docID, id); err != nil {
				return err
			}
		}
		return nil

	case "assign_custom_field":
		fieldID := intVal(a.Params["field_id"])
		if fieldID == 0 {
			return errors.New("assign_custom_field: field_id required")
		}
		return upsertCustomField(ctx, tx, docID, fieldID, a.Params["value"])

	case "remove_custom_field":
		fieldID := intVal(a.Params["field_id"])
		if fieldID == 0 {
			return errors.New("remove_custom_field: field_id required")
		}
		_, err := tx.ExecContext(ctx,
			`DELETE FROM document_custom_field_values WHERE document_id = ? AND field_id = ?`,
			docID, fieldID)
		return err

	case "discard":
		// Same soft-trash the SPA's POST /api/documents/{id}/trash uses:
		// set trashed_at; the blob stays in the CAS for `suchi gc`. No
		// shared helper today — the HTTP handler does the UPDATE inline
		// and we can't import core/api from here. Guarded by trashed_at
		// IS NULL so re-firing on an already-trashed doc is a no-op.
		now := time.Now().Unix()
		_, err := tx.ExecContext(ctx,
			`UPDATE documents SET trashed_at = ?, updated_at = ?
			 WHERE id = ? AND trashed_at IS NULL`,
			now, now, docID)
		return err
	}
	return fmt.Errorf("unknown action kind %q", a.Kind)
}

// upsertCustomField uses the same validation and typed writer as the document
// API, keeping automation values consistent with direct edits.
func upsertCustomField(ctx context.Context, tx *sql.Tx, docID, fieldID int64, val any) error {
	var dataType, extra string
	err := tx.QueryRowContext(ctx,
		`SELECT data_type, extra_data FROM custom_fields WHERE id = ?`, fieldID).
		Scan(&dataType, &extra)
	if err != nil {
		return fmt.Errorf("custom_field %d: %w", fieldID, err)
	}
	handler := customfield.Lookup(dataType)
	typed, err := handler.Validate(json.RawMessage(extra), val)
	if err != nil {
		return fmt.Errorf("custom_field %d: %w", fieldID, err)
	}
	return handler.Write(ctx, tx, docID, fieldID, typed)
}

// expandTitle is a tiny template resolver — enough for the mobile
// apps' common cases. Placeholders:
//
//	{{title}}          current title
//	{{correspondent}}  correspondent name or ""
//	{{document_type}}  document_type name or ""
//	{{date}}           YYYY-MM-DD of documents.created_at
//
// Missing values render empty. Anything else is passed through
// verbatim so `{{unknown}}` is visible in the result — surfacing the
// typo rather than silently swallowing it.
func expandTitle(ctx context.Context, tx *sql.Tx, docID int64, tpl string) (string, error) {
	var (
		title, corrName, typeName sql.NullString
		createdAt                 int64
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT d.title, c.name, t.name, d.created_at
		FROM documents d
		LEFT JOIN correspondents c   ON c.id = d.correspondent_id
		LEFT JOIN document_types t   ON t.id = d.document_type_id
		WHERE d.id = ?`, docID).Scan(&title, &corrName, &typeName, &createdAt); err != nil {
		return "", fmt.Errorf("assign_title: load document: %w", err)
	}

	repl := map[string]string{
		"{{title}}":         nullStr(title),
		"{{correspondent}}": nullStr(corrName),
		"{{document_type}}": nullStr(typeName),
		"{{date}}":          time.Unix(createdAt, 0).UTC().Format("2006-01-02"),
	}
	out := tpl
	for k, v := range repl {
		out = strings.ReplaceAll(out, k, v)
	}
	return out, nil
}

func nullStr(s sql.NullString) string {
	if !s.Valid {
		return ""
	}
	return s.String
}

func intVal(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int:
		return int64(t)
	case int64:
		return t
	}
	return 0
}

func intList(v any) []int64 {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]int64, 0, len(arr))
	for _, x := range arr {
		if n := intVal(x); n != 0 {
			out = append(out, n)
		}
	}
	return out
}
