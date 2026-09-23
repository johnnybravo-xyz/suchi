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
func ApplyOnDocumentAdded(ctx context.Context, d *db.DB, actions *Registry, log *slog.Logger, docID int64) error {
	_, err := ApplyOnDocumentAddedCount(ctx, d, actions, log, docID, 0)
	return err
}

// ApplyOnDocumentAddedCount is used by explicit refiles, where callers need
// to report how many automations matched the document.
func ApplyOnDocumentAddedCount(ctx context.Context, d *db.DB, actions *Registry, log *slog.Logger, docID, actorID int64) (int, error) {
	s := New(d, actions)
	var systemID int64
	if err := d.Read.QueryRowContext(ctx, `SELECT system_id FROM documents WHERE id = ?`, docID).Scan(&systemID); err != nil {
		return 0, err
	}
	atms, err := s.ByTrigger(ctx, systemID, TriggerDocumentAdded)
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
	return runMatching(ctx, d, actions, log, atms, evCtx, snap, TriggerDocumentAdded, actorID)
}

// ApplyOnDocumentUpdated mirrors the above for the update trigger.
// Called from PATCH /api/documents/{id} handlers after the write lands.
func ApplyOnDocumentUpdated(ctx context.Context, d *db.DB, actions *Registry, log *slog.Logger, docID int64) error {
	s := New(d, actions)
	var systemID int64
	if err := d.Read.QueryRowContext(ctx, `SELECT system_id FROM documents WHERE id = ?`, docID).Scan(&systemID); err != nil {
		return err
	}
	atms, err := s.ByTrigger(ctx, systemID, TriggerDocumentUpdated)
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
	_, err = runMatching(ctx, d, actions, log, atms, evCtx, snap, TriggerDocumentUpdated, 0)
	return err
}

// ApplyOnConsumption runs consumption-trigger automations. Producers
// (upload API, fs-watch, mail-intake) call this at the start of the
// post-ingest handler with whatever context they have — filename,
// source path, mail-rule id. Any missing field disables the
// corresponding filter without erroring.
func ApplyOnConsumption(ctx context.Context, d *db.DB, actions *Registry, log *slog.Logger, docID int64, evCtx Context) error {
	s := New(d, actions)
	var systemID int64
	if err := d.Read.QueryRowContext(ctx, `SELECT system_id FROM documents WHERE id = ?`, docID).Scan(&systemID); err != nil {
		return err
	}
	atms, err := s.ByTrigger(ctx, systemID, TriggerConsumption)
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
	_, err = runMatching(ctx, d, actions, log, atms, evCtx, snap, TriggerConsumption, 0)
	return err
}

func runMatching(ctx context.Context, d *db.DB, actions *Registry, log *slog.Logger,
	atms []Automation, evCtx Context, snap *docSnapshot, t TriggerType, actorID int64) (int, error) {

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
			if actorID != 0 {
				var allowed bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id=? AND disabled=0 AND role='admin')`, actorID).Scan(&allowed); err != nil {
					return err
				}
				if !allowed {
					return errors.New("automations: active admin required for refile")
				}
			}
			for _, a := range atm.Actions {
				if err := actions.execute(ctx, tx, evCtx.DocID, a); err != nil {
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

	// Machine-owned review markers are workflow state, not user-authorized
	// labels. They must not activate rules that can transfer or trash a doc.
	rows, err := d.Read.QueryContext(ctx,
		`SELECT tag_id FROM document_tags WHERE document_id = ? AND classifier_owned = 0`, docID)
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

// upsertCustomField uses the same validation and typed writer as the document
// API, keeping automation values consistent with direct edits.
func upsertCustomField(ctx context.Context, tx *sql.Tx, docID, fieldID int64, val any) error {
	var dataType, extra string
	err := tx.QueryRowContext(ctx,
		`SELECT data_type, extra_data FROM custom_fields WHERE id = ? AND system_id = (SELECT system_id FROM documents WHERE id = ?)`, fieldID, docID).
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
