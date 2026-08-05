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

	"github.com/suchi-dms/suchi/core/db"
)

func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Context carries the event-time facts an automation needs. Populated
// by the caller — the engine never queries side-channel data.
type Context struct {
	DocID      int64
	SourcePath string // consumption trigger only
	Filename   string // consumption trigger only
	MailRuleID int64  // consumption via mail intake
}

// docSnapshot mirrors rules.docSnapshot — a light in-memory read of
// the metadata an automation filter can key on. Loaded once per
// evaluation.
type docSnapshot struct {
	Title           string
	Content         string
	TagIDs          map[int64]bool
	CorrespondentID sql.NullInt64
	DocumentTypeID  sql.NullInt64
}

// ApplyOnDocumentAdded is the postingest hook. Every enabled workflow
// with a document_added trigger evaluates against the doc; matching
// triggers run their actions in one write tx.
//
// Errors on individual actions log a warning and skip; only DB-level
// failures return an error to the caller.
func ApplyOnDocumentAdded(ctx context.Context, d *db.DB, log *slog.Logger, docID int64) error {
	s := &Store{DB: d}
	wfs, err := s.ByTrigger(ctx, TriggerDocumentAdded)
	if err != nil {
		return fmt.Errorf("automations: load workflows: %w", err)
	}
	if len(wfs) == 0 {
		return nil
	}
	snap, err := loadSnapshot(ctx, d, docID)
	if err != nil {
		return fmt.Errorf("automations: snapshot: %w", err)
	}
	evCtx := Context{DocID: docID}
	return runMatching(ctx, d, log, wfs, evCtx, snap, TriggerDocumentAdded)
}

// ApplyOnDocumentUpdated mirrors the above for the update trigger.
// Called from PATCH /api/documents/{id} handlers after the write lands.
func ApplyOnDocumentUpdated(ctx context.Context, d *db.DB, log *slog.Logger, docID int64) error {
	s := &Store{DB: d}
	wfs, err := s.ByTrigger(ctx, TriggerDocumentUpdated)
	if err != nil {
		return fmt.Errorf("automations: load workflows: %w", err)
	}
	if len(wfs) == 0 {
		return nil
	}
	snap, err := loadSnapshot(ctx, d, docID)
	if err != nil {
		return fmt.Errorf("automations: snapshot: %w", err)
	}
	evCtx := Context{DocID: docID}
	return runMatching(ctx, d, log, wfs, evCtx, snap, TriggerDocumentUpdated)
}

// ApplyOnConsumption runs consumption-trigger automations. Producers
// (upload API, fs-watch, mail-intake) call this at the start of the
// post-ingest handler with whatever context they have — filename,
// source path, mail-rule id. Any missing field disables the
// corresponding filter without erroring.
func ApplyOnConsumption(ctx context.Context, d *db.DB, log *slog.Logger, docID int64, evCtx Context) error {
	s := &Store{DB: d}
	wfs, err := s.ByTrigger(ctx, TriggerConsumption)
	if err != nil {
		return fmt.Errorf("automations: load workflows: %w", err)
	}
	if len(wfs) == 0 {
		return nil
	}
	snap, err := loadSnapshot(ctx, d, docID)
	if err != nil {
		return fmt.Errorf("automations: snapshot: %w", err)
	}
	evCtx.DocID = docID
	return runMatching(ctx, d, log, wfs, evCtx, snap, TriggerConsumption)
}

func runMatching(ctx context.Context, d *db.DB, log *slog.Logger,
	wfs []Workflow, evCtx Context, snap *docSnapshot, t TriggerType) error {

	for _, w := range wfs {
		matched := false
		for _, tr := range w.Triggers {
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

		wlog := log.With("automation.id", w.ID, "automation.name", w.Name, "doc_id", evCtx.DocID)
		if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
			for _, a := range w.Actions {
				if err := runAction(ctx, tx, wlog, evCtx.DocID, a); err != nil {
					wlog.Warn("automations.action.error",
						"kind", a.Kind, "err", err.Error())
				}
			}
			return nil
		}); err != nil {
			wlog.Warn("automations.tx.error", "err", err.Error())
		}
	}
	return nil
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
	if tr.FilterContentRE != "" {
		re, err := regexp.Compile("(?i)" + tr.FilterContentRE)
		if err != nil {
			return false
		}
		if !re.MatchString(snap.Content) {
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

// runAction dispatches on Kind. Every action is idempotent: re-running
// the same automation on the same doc converges rather than diverging.
func runAction(ctx context.Context, tx *sql.Tx, log *slog.Logger, docID int64, a Action) error {
	switch a.Kind {
	case "assign_title":
		tpl, _ := a.Params["template"].(string)
		if tpl == "" {
			return errors.New("assign_title: template required")
		}
		title := expandTitle(ctx, tx, docID, tpl)
		_, err := tx.ExecContext(ctx,
			`UPDATE documents SET title = ? WHERE id = ?`, title, docID)
		return err

	case "assign_tags":
		ids := intList(a.Params["tag_ids"])
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO document_tags(document_id, tag_id) VALUES (?, ?)`,
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

	case "remove_owner":
		_, err := tx.ExecContext(ctx,
			`UPDATE documents SET owner_id = NULL WHERE id = ?`, docID)
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
	}
	return fmt.Errorf("unknown action kind %q", a.Kind)
}

// upsertCustomField writes into the type-native column that matches the
// field's data_type. Types outside the closed vocabulary land in
// value_text — best-effort rather than blocking the whole automation.
func upsertCustomField(ctx context.Context, tx *sql.Tx, docID, fieldID int64, val any) error {
	var dataType string
	err := tx.QueryRowContext(ctx,
		`SELECT data_type FROM custom_fields WHERE id = ?`, fieldID).Scan(&dataType)
	if err != nil {
		return fmt.Errorf("custom_field %d: %w", fieldID, err)
	}
	var (
		vText   sql.NullString
		vNumber sql.NullFloat64
		vInt    sql.NullInt64
		vBool   sql.NullInt64
		vDate   sql.NullInt64
	)
	switch dataType {
	case "text", "select", "url", "documentlink":
		if s, ok := val.(string); ok {
			vText = sql.NullString{String: s, Valid: true}
		}
	case "number", "monetary":
		if f, ok := val.(float64); ok {
			vNumber = sql.NullFloat64{Float64: f, Valid: true}
		}
	case "bool":
		if b, ok := val.(bool); ok {
			bi := int64(0)
			if b {
				bi = 1
			}
			vBool = sql.NullInt64{Int64: bi, Valid: true}
		}
	case "date":
		if n := intVal(val); n != 0 {
			vDate = sql.NullInt64{Int64: n, Valid: true}
		}
	case "multi":
		// Multi stores an array as JSON in value_text.
		raw, err := jsonMarshal(val)
		if err == nil {
			vText = sql.NullString{String: raw, Valid: true}
		}
	default:
		if s, ok := val.(string); ok {
			vText = sql.NullString{String: s, Valid: true}
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO document_custom_field_values(
			document_id, field_id,
			value_text, value_number, value_int, value_bool, value_date)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(document_id, field_id) DO UPDATE SET
			value_text   = excluded.value_text,
			value_number = excluded.value_number,
			value_int    = excluded.value_int,
			value_bool   = excluded.value_bool,
			value_date   = excluded.value_date
	`, docID, fieldID, vText, vNumber, vInt, vBool, vDate)
	return err
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
func expandTitle(ctx context.Context, tx *sql.Tx, docID int64, tpl string) string {
	var (
		title, corrName, typeName sql.NullString
		createdAt                 int64
	)
	_ = tx.QueryRowContext(ctx, `
		SELECT d.title, c.name, t.name, d.created_at
		FROM documents d
		LEFT JOIN correspondents c   ON c.id = d.correspondent_id
		LEFT JOIN document_types t   ON t.id = d.document_type_id
		WHERE d.id = ?`, docID).Scan(&title, &corrName, &typeName, &createdAt)

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
	return out
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
