package approvals

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

const (
	DocumentChangeSlug = "document-change"
	documentChangeKind = "apply_document_change"
)

// DocumentChange is a bounded metadata suggestion produced by a classifier.
// The approval run stores this provenance and applies it only after review.
type DocumentChange struct {
	Field      string
	ValueID    int64
	Value      string
	Label      string
	Confidence float64
	BasedOn    []int64
	Source     string
}

func DocumentChangeSpec() Spec {
	return Spec{
		Start: "review",
		States: map[string]State{
			"review": {
				Kind: "approve", Assignee: "document_owner",
				Prompt:  "Review suggested document metadata",
				Choices: []string{"apply", "reject"},
				On:      map[string]string{"apply": "apply", "reject": "end"},
			},
			"apply": {Kind: documentChangeKind, On: map[string]string{"success": "end"}},
			"end":   {Kind: "end"},
		},
	}
}

// ProposeDocumentChangeInTx creates one deduplicated approval run in the
// caller's transaction. It lazily seeds the built-in definition so a fresh
// installation does not need a restart after setup.
func ProposeDocumentChangeInTx(ctx context.Context, tx *sql.Tx, docID int64, change DocumentChange) error {
	if err := validateDocumentChange(change); err != nil {
		return err
	}
	var ownerID int64
	if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM documents WHERE id = ?`, docID).Scan(&ownerID); err != nil {
		return fmt.Errorf("document change: load owner: %w", err)
	}

	d, err := activeDefBySlug(ctx, tx, DocumentChangeSlug)
	if errors.Is(err, ErrNoDef) {
		raw, encodeErr := EncodeSpec(DocumentChangeSpec())
		if encodeErr != nil {
			return encodeErr
		}
		if _, _, err = insertDef(ctx, tx, DocumentChangeSlug, raw, 0); err != nil {
			return fmt.Errorf("document change: seed definition: %w", err)
		}
		d, err = activeDefBySlug(ctx, tx, DocumentChangeSlug)
	}
	if err != nil {
		return err
	}

	var existing int64
	err = tx.QueryRowContext(ctx, `
		SELECT r.id
		FROM approval_runs r
		WHERE r.def_id = ? AND r.doc_id = ? AND r.state = 'running'
		  AND json_extract(r.vars_json, '$.field') = ?
		  AND COALESCE(json_extract(r.vars_json, '$.value_id'), 0) = ?
		  AND COALESCE(json_extract(r.vars_json, '$.value'), '') = ?
		LIMIT 1
	`, d.ID, docID, change.Field, change.ValueID, change.Value).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("document change: deduplicate: %w", err)
	}

	vars := map[string]any{
		"owner_id": ownerID, "field": change.Field, "value_id": change.ValueID,
		"value": change.Value, "label": change.Label, "confidence": change.Confidence,
		"based_on": change.BasedOn, "source": change.Source,
	}
	_, err = startInTx(ctx, tx, DocumentChangeSlug, docID, vars, nil)
	return err
}

func validateDocumentChange(change DocumentChange) error {
	switch change.Field {
	case "jd_category", "correspondent", "document_type", "tag":
		if change.ValueID <= 0 {
			return fmt.Errorf("document change: %s requires value_id", change.Field)
		}
	case "title":
		if strings.TrimSpace(change.Value) == "" {
			return errors.New("document change: title requires value")
		}
	default:
		return fmt.Errorf("document change: unsupported field %q", change.Field)
	}
	if change.Confidence < 0 || change.Confidence > 1 {
		return errors.New("document change: confidence must be between 0 and 1")
	}
	return nil
}

type documentChangeHandler struct{ log *slog.Logger }

func (documentChangeHandler) Kind() string { return documentChangeKind }

func (h documentChangeHandler) Handle(_ context.Context, run Run, _ State, _ string) (HandlerResult, error) {
	change, err := documentChangeFromVars(run.Vars)
	if err != nil {
		return HandlerResult{}, err
	}
	return HandlerResult{
		Event: "success",
		Effect: func(ctx context.Context, tx *sql.Tx) error {
			return applyDocumentChange(ctx, tx, h.log, run, change)
		},
	}, nil
}

func documentChangeFromVars(vars map[string]any) (DocumentChange, error) {
	valueID, _ := int64Var(vars, "value_id")
	change := DocumentChange{
		Field: stringVar(vars, "field"), ValueID: valueID,
		Value: stringVar(vars, "value"), Label: stringVar(vars, "label"),
		Confidence: float64Var(vars, "confidence"), Source: stringVar(vars, "source"),
	}
	if raw, ok := vars["based_on"].([]any); ok {
		for _, item := range raw {
			if id, ok := item.(float64); ok {
				change.BasedOn = append(change.BasedOn, int64(id))
			}
		}
	} else if ids, ok := vars["based_on"].([]int64); ok {
		change.BasedOn = append([]int64(nil), ids...)
	}
	return change, validateDocumentChange(change)
}

func applyDocumentChange(ctx context.Context, tx *sql.Tx, log *slog.Logger, run Run, change DocumentChange) error {
	if run.DocID == nil {
		return errors.New("document change: approval has no document")
	}
	docID := *run.DocID
	now := time.Now().Unix()
	var result sql.Result
	var err error
	switch change.Field {
	case "jd_category":
		result, err = tx.ExecContext(ctx, `
			UPDATE documents SET jd_category_id = ?, updated_at = ?
			WHERE id = ? AND (jd_category_id IS NULL OR jd_category_id = (
				SELECT CAST(value_json AS INTEGER) FROM settings WHERE key = 'jd_inbox_category_id'
			))`, change.ValueID, now, docID)
	case "correspondent":
		result, err = tx.ExecContext(ctx, `UPDATE documents SET correspondent_id = ?, updated_at = ? WHERE id = ? AND correspondent_id IS NULL`, change.ValueID, now, docID)
	case "document_type":
		result, err = tx.ExecContext(ctx, `UPDATE documents SET document_type_id = ?, updated_at = ? WHERE id = ? AND document_type_id IS NULL`, change.ValueID, now, docID)
	case "tag":
		result, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO document_tags(document_id, tag_id) VALUES (?, ?)`, docID, change.ValueID)
	case "title":
		result, err = tx.ExecContext(ctx, `UPDATE documents SET title = ?, updated_at = ? WHERE id = ?`, change.Value, now, docID)
	}
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	audit.LogInTx(ctx, tx, log, audit.Event{
		Actor: actorForRun(ctx, tx, run.ID), Action: "document.suggestion_apply",
		ObjectKind: "document", ObjectID: docID,
		After: map[string]any{"field": change.Field, "value_id": change.ValueID, "value": change.Value,
			"label": change.Label, "confidence": change.Confidence, "source": change.Source, "changed": changed > 0},
	})
	return nil
}

func actorForRun(ctx context.Context, tx *sql.Tx, runID int64) *pluginapi.Principal {
	var tag string
	if err := tx.QueryRowContext(ctx, `
		SELECT resolved_by FROM approval_tasks WHERE run_id = ? AND status = 'resolved'
		ORDER BY resolved_at DESC, id DESC LIMIT 1
	`, runID).Scan(&tag); err != nil {
		return nil
	}
	parts := strings.SplitN(tag, ":", 2)
	if len(parts) != 2 {
		return nil
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		return nil
	}
	if parts[0] == "token" {
		return &pluginapi.Principal{Kind: "token", TokenID: id}
	}
	return &pluginapi.Principal{Kind: "user", UserID: id}
}

func stringVar(vars map[string]any, key string) string {
	v, _ := vars[key].(string)
	return v
}

func float64Var(vars map[string]any, key string) float64 {
	switch v := vars[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	}
	return 0
}
