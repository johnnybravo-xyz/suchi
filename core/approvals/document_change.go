package approvals

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/documentstate"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	"github.com/johnnybravo-xyz/suchi/core/lang"
	"github.com/johnnybravo-xyz/suchi/core/render/view"
	"github.com/johnnybravo-xyz/suchi/core/settings"
	"github.com/johnnybravo-xyz/suchi/core/slug"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

const (
	DocumentChangeSlug        = "document-change"
	documentChangeKind        = "apply_document_change"
	AutomaticPolicyVersion    = "threshold-auto-v1"
	ReviewPolicyVersion       = "review-first-v1"
	ReviewReasonReviewFirst   = "review_first"
	ReviewReasonLowConfidence = "low_confidence"
	ReviewReasonImportantFact = "important_fact"
)

var ErrStaleProposal = errors.New("document change: proposal is stale or unbound")

// DocumentChange is a bounded metadata suggestion bound before inference starts.
type DocumentChange struct {
	Field         string                    `json:"field"`
	ValueID       int64                     `json:"value_id"`
	Value         string                    `json:"value"`
	Confidence    float64                   `json:"confidence"`
	Threshold     *float64                  `json:"threshold,omitempty"`
	Source        string                    `json:"source"`
	Baseline      *documentstate.Snapshot   `json:"baseline"`
	Supporters    []documentstate.Reference `json:"supporters"`
	Reason        string                    `json:"reason"`
	PolicyVersion string                    `json:"policy_version"`
}

func DocumentChangeSpec() Spec {
	return Spec{Start: "review", States: map[string]State{
		"review": {Kind: "approve", Assignee: "document_owner", Prompt: "Review suggested document metadata", Choices: []string{"apply", "reject"}, On: map[string]string{"apply": "apply", "reject": "end"}},
		"apply":  {Kind: documentChangeKind, On: map[string]string{"success": "end"}},
		"end":    {Kind: "end"},
	}}
}

// ProposeDocumentChangeInTx records review provenance atomically with the review.
// Generic workflows cannot mint a source/human binding or replace the
// mandatory review state of a queued proposal.
func ProposeDocumentChangeInTx(ctx context.Context, tx *sql.Tx, docID int64, change DocumentChange) error {
	reason, err := checkDocumentChange(ctx, tx, docID, change, nil)
	if err != nil {
		return err
	}
	change.Reason, change.PolicyVersion = reason, ReviewPolicyVersion
	raw, err := EncodeSpec(DocumentChangeSpec())
	if err != nil {
		return err
	}
	d, err := activeDefBySlug(ctx, tx, change.Baseline.SystemID, DocumentChangeSlug)
	if errors.Is(err, ErrNoDef) {
		if _, _, err = insertDef(ctx, tx, change.Baseline.SystemID, DocumentChangeSlug, raw, 0); err != nil {
			return err
		}
		d, err = activeDefBySlug(ctx, tx, change.Baseline.SystemID, DocumentChangeSlug)
	}
	if err != nil {
		return err
	}
	if !specsEqual(d.SpecJSON, raw) {
		return ErrStaleProposal
	}
	encoded, err := json.Marshal(change)
	if err != nil {
		return err
	}
	var vars map[string]any
	if err := json.Unmarshal(encoded, &vars); err != nil {
		return err
	}
	vars["owner_id"] = change.Baseline.OwnerID
	encoded, err = json.Marshal(vars)
	if err != nil {
		return err
	}
	// A new generation or permanently unusable review authorization must not
	// swallow a fresh proposal. Retain resolved tasks as immutable history.
	for {
		var existing Run
		err = tx.QueryRowContext(ctx, `SELECT id,current_state,revision FROM approval_runs WHERE def_id=? AND doc_id=? AND state='running'
 AND json_extract(vars_json,'$.field')=? AND json_extract(vars_json,'$.value_id')=?
 AND json_extract(vars_json,'$.value')=? AND json_extract(vars_json,'$.baseline') IS json_extract(?,'$.baseline')
 AND json_extract(vars_json,'$.supporters') IS json_extract(?,'$.supporters') LIMIT 1`, d.ID, docID, change.Field, change.ValueID, change.Value, string(encoded), string(encoded)).Scan(&existing.ID, &existing.CurrentState, &existing.revision)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return err
		}
		unusable, err := documentChangeAuthorizationUnusable(ctx, tx, existing)
		if err != nil {
			return err
		}
		if !unusable {
			return nil
		}
		if err := insertTransition(ctx, tx, existing.ID, existing.CurrentState, existing.CurrentState, "authorization_expired", nil, nil); err != nil {
			return err
		}
		if err := finalizeRun(ctx, tx, existing.ID, "failed"); err != nil {
			return err
		}
	}
	runID, err := insertRun(ctx, tx, change.Baseline.SystemID, d.ID, &docID, "review", vars, nil, 0)
	if err != nil {
		return err
	}
	return enqueueAdvance(ctx, tx, runID, "")
}

// ApplyAutomaticDocumentChangeInTx is the host's threshold-authorized path,
// separate from human review. Both paths share freshness checks and effects.
// A false result leaves the candidate eligible for the normal review path.
func ApplyAutomaticDocumentChangeInTx(ctx context.Context, tx *sql.Tx, log *slog.Logger, docID int64, change DocumentChange) (bool, error) {
	enabled, err := settings.ResolveAutoApply(ctx, tx)
	if err != nil {
		return false, err
	}
	if !enabled || change.Threshold == nil || change.Confidence < *change.Threshold {
		return false, nil
	}
	if change.Source != "llm" && change.Source != "archive" {
		return false, ErrStaleProposal
	}
	if _, err := checkDocumentChange(ctx, tx, docID, change, nil); err != nil {
		return false, err
	}
	if err := writeDocumentChangeValue(ctx, tx, docID, change.Baseline.SystemID, change, false); err != nil {
		return false, err
	}
	audit.LogInTx(ctx, tx, log, audit.Event{
		SystemID: change.Baseline.SystemID, Action: "document.suggestion_autoapply",
		ObjectKind: "document", ObjectID: docID,
		After: map[string]any{"field": change.Field, "source": change.Source,
			"confidence": change.Confidence, "threshold": *change.Threshold,
			"policy_version": AutomaticPolicyVersion},
	})
	return true, nil
}

// Only a resolved apply decision can have exhausted its authorization. Initial
// entry and open reviews still deduplicate, as do decisions whose bound session
// remains usable. This never substitutes a newer session for the recorded one.
func documentChangeAuthorizationUnusable(ctx context.Context, tx *sql.Tx, run Run) (bool, error) {
	revision := run.revision
	switch run.CurrentState {
	case "review":
		revision--
	case "apply":
		revision -= 2
	default:
		return false, nil
	}
	var principalJSON string
	err := tx.QueryRowContext(ctx, `SELECT resolution_principal_json FROM approval_tasks
 WHERE run_id=? AND state_key='review' AND state_revision=? AND status='resolved' AND resolved_choice='apply'
 AND NOT EXISTS (SELECT 1 FROM approval_tasks WHERE run_id=? AND status IN ('open','claimed'))
 ORDER BY id DESC LIMIT 1`, run.ID, revision, run.ID).Scan(&principalJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var proof resolutionPrincipal
	if err := json.Unmarshal([]byte(principalJSON), &proof); err != nil {
		return true, nil
	}
	if proof.SessionID == "" || proof.Principal.Kind != "user" || proof.Principal.TokenID != 0 {
		return true, nil
	}
	now := time.Now().Unix()
	if proof.AuthExpiresAt != 0 && proof.AuthExpiresAt <= now {
		return true, nil
	}
	var active int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id=? AND user_id=? AND expires_at>?`,
		proof.SessionID, proof.Principal.UserID, now).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return false, err
}

func validateDocumentChange(c DocumentChange) error {
	if len(c.Value) > 1024 || len(c.Source) > 64 || len(c.Supporters) > 16 || c.ValueID < 0 {
		return ErrStaleProposal
	}
	switch c.Field {
	case "jd_category":
		if c.ValueID <= 0 {
			return ErrStaleProposal
		}
	case "correspondent", "document_type", "tag":
		if c.ValueID == 0 && strings.TrimSpace(c.Value) == "" {
			return ErrStaleProposal
		}
	case "title":
		if strings.TrimSpace(c.Value) == "" || c.ValueID != 0 {
			return ErrStaleProposal
		}
	case "language":
		if c.ValueID != 0 || (c.Value != "" && lang.Format(c.Value) == "") {
			return ErrStaleProposal
		}
	default:
		return ErrStaleProposal
	}
	if math.IsNaN(c.Confidence) || math.IsInf(c.Confidence, 0) || c.Confidence < 0 || c.Confidence > 1 {
		return ErrStaleProposal
	}
	return nil
}

func checkDocumentChange(ctx context.Context, tx *sql.Tx, docID int64, c DocumentChange, reviewer *pluginapi.Principal) (string, error) {
	if err := validateDocumentChange(c); err != nil {
		return "", err
	}
	if c.Baseline == nil {
		return "", ErrStaleProposal
	}
	current, err := documentstate.Load(ctx, tx, docID)
	if err != nil {
		return "", ErrStaleProposal
	}
	if !current.SameSource(*c.Baseline) || current.FieldRevision(c.Field) != c.Baseline.FieldRevision(c.Field) {
		return "", ErrStaleProposal
	}
	reason := ReviewReasonReviewFirst
	if c.Threshold != nil && c.Confidence < *c.Threshold {
		reason = ReviewReasonLowConfidence
	}
	owner, err := currentActor(ctx, tx, &pluginapi.Principal{Kind: "user", UserID: current.OwnerID}, current.SystemID)
	if err != nil {
		return "", ErrForbidden
	}
	// Inference has owner-level visibility, never an administrator's corpus-wide
	// bypass. Keep that ceiling for archive matches and model naming context.
	owner.Role = "member"
	if reviewer != nil {
		// Only an authenticated interactive browser session can turn this review
		// into a write. Scoped API/OIDC bearer possession is not human review.
		if reviewer.SessionID == "" || reviewer.Kind != "user" || reviewer.TokenID != 0 {
			return "", ErrForbidden
		}
		if err := canReviewDocument(ctx, tx, reviewer, current.SystemID, docID, authz.PermChange); err != nil {
			return "", err
		}
		if _, err := documentChangeVocabulary(ctx, tx, current.SystemID, c, reviewer); err != nil {
			return "", err
		}
	}
	if err := validateDestination(ctx, tx, docID, current.SystemID, c); err != nil {
		return "", err
	}
	seen := make(map[int64]bool, len(c.Supporters))
	for _, ref := range c.Supporters {
		if ref.DocumentID <= 0 || ref.DocumentID == docID || seen[ref.DocumentID] {
			return "", ErrStaleProposal
		}
		seen[ref.DocumentID] = true
		support, err := documentstate.Load(ctx, tx, ref.DocumentID)
		if err != nil || support != ref.Snapshot || support.SystemID != current.SystemID {
			return "", ErrStaleProposal
		}
		if _, err := currentActor(ctx, tx, &pluginapi.Principal{Kind: "user", UserID: support.OwnerID}, support.SystemID); err != nil {
			return "", ErrForbidden
		}
		if err := canReviewDocument(ctx, tx, owner, current.SystemID, ref.DocumentID, authz.PermView); err != nil {
			return "", err
		}
		if reviewer != nil {
			if err := canReviewDocument(ctx, tx, reviewer, current.SystemID, ref.DocumentID, authz.PermView); err != nil {
				return "", err
			}
		}
	}
	return reason, nil
}

func canReviewDocument(ctx context.Context, tx *sql.Tx, actor *pluginapi.Principal, systemID, docID int64, permission authz.Perm) error {
	if actor == nil {
		return ErrForbidden
	}
	groups, err := authz.LoadGroupsInTx(ctx, tx, actor.UserID)
	if err != nil {
		return err
	}
	if err := (authz.ACLAuthorizer{}).CanInTx(ctx, tx, authz.Principal{UserID: actor.UserID, Role: actor.Role, Kind: actor.Kind, Groups: groups, SystemID: systemID, TokenSystemID: actor.TokenSystemID}, authz.KindDocument, docID, permission); err != nil {
		return ErrForbidden
	}
	return nil
}

func validateDestination(ctx context.Context, tx *sql.Tx, docID, systemID int64, c DocumentChange) error {
	var eligible bool
	switch c.Field {
	case "jd_category":
		if err := tx.QueryRowContext(ctx, `SELECT d.jd_category_id=COALESCE(s.inbox_category_id,0) FROM documents d JOIN jd_systems s ON s.id=d.system_id WHERE d.id=?`, docID).Scan(&eligible); err != nil {
			return err
		}
	case "correspondent":
		if err := tx.QueryRowContext(ctx, `SELECT correspondent_id IS NULL AND NOT EXISTS(SELECT 1 FROM document_correspondents WHERE document_id=documents.id AND role='sender') FROM documents WHERE id=?`, docID).Scan(&eligible); err != nil {
			return err
		}
	case "document_type":
		if err := tx.QueryRowContext(ctx, `SELECT document_type_id IS NULL FROM documents WHERE id=?`, docID).Scan(&eligible); err != nil {
			return err
		}
	case "language":
		if err := tx.QueryRowContext(ctx, `SELECT languages_locked=0 FROM documents WHERE id=?`, docID).Scan(&eligible); err != nil {
			return err
		}
	default:
		eligible = true
	}
	if !eligible {
		return ErrStaleProposal
	}
	if c.ValueID > 0 {
		table := map[string]string{"jd_category": "jd_categories", "correspondent": "correspondents", "document_type": "document_types", "tag": "tags"}[c.Field]
		var one int
		if table == "" {
			return ErrStaleProposal
		}
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM "+table+" WHERE id=? AND system_id=?", c.ValueID, systemID).Scan(&one); err != nil {
			return ErrStaleProposal
		}
	}
	return nil
}

// documentChangeVocabulary resolves existing admin-managed terms without an
// insert, using taxonomy.UpsertByName's exact-name then canonical-slug order.
// For these terms, a zero ID permits creation only for a current administrator.
func documentChangeVocabulary(ctx context.Context, tx *sql.Tx, systemID int64, c DocumentChange, actor *pluginapi.Principal) (int64, error) {
	if c.ValueID != 0 {
		return c.ValueID, nil
	}
	var table string
	switch c.Field {
	case "tag":
		table = "tags"
	case "document_type":
		table = "document_types"
	default:
		return 0, nil
	}
	name := strings.TrimSpace(c.Value)
	var id int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE system_id=? AND (name=? OR slug=?) ORDER BY CASE WHEN name=? THEN 0 ELSE 1 END, id LIMIT 1", systemID, name, slug.Make(name), name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if actor == nil || actor.Role != "admin" {
		return 0, ErrForbidden
	}
	return 0, nil
}

type documentChangeHandler struct{ log *slog.Logger }

func (documentChangeHandler) Kind() string { return documentChangeKind }
func (h documentChangeHandler) Handle(_ context.Context, run Run, _ State, _ string) (HandlerResult, error) {
	c, err := documentChangeFromVars(run.Vars)
	if err != nil {
		return HandlerResult{}, err
	}
	return HandlerResult{Event: "success", Effect: func(ctx context.Context, tx *sql.Tx) error { return applyDocumentChange(ctx, tx, h.log, run, c) }}, nil
}
func documentChangeFromVars(vars map[string]any) (DocumentChange, error) {
	var c DocumentChange
	raw, err := json.Marshal(vars)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	return c, validateDocumentChange(c)
}

func applyDocumentChange(ctx context.Context, tx *sql.Tx, log *slog.Logger, run Run, c DocumentChange) error {
	if run.DocID == nil || c.PolicyVersion != ReviewPolicyVersion {
		return ErrStaleProposal
	}
	d, err := defByID(ctx, tx, run.DefID)
	if err != nil {
		return err
	}
	raw, err := EncodeSpec(DocumentChangeSpec())
	if err != nil {
		return err
	}
	if d.Slug != DocumentChangeSlug || !specsEqual(d.SpecJSON, raw) || run.CurrentState != "apply" {
		return ErrStaleProposal
	}
	var principalJSON string
	if err := tx.QueryRowContext(ctx, `SELECT resolution_principal_json FROM approval_tasks WHERE run_id=? AND state_key='review' AND status='resolved' AND resolved_choice='apply' AND state_revision=? ORDER BY id DESC LIMIT 1`, run.ID, run.revision-2).Scan(&principalJSON); err != nil {
		return ErrStaleProposal
	}
	var proof resolutionPrincipal
	if err := json.Unmarshal([]byte(principalJSON), &proof); err != nil {
		return ErrStaleProposal
	}
	actor := proof.Principal
	actor.SessionID, actor.AuthExpiresAt = proof.SessionID, proof.AuthExpiresAt
	currentActor, err := currentActor(ctx, tx, &actor, run.SystemID)
	if err != nil {
		return err
	}
	if _, err := checkDocumentChange(ctx, tx, *run.DocID, c, currentActor); err != nil {
		return err
	}
	c.ValueID, err = documentChangeVocabulary(ctx, tx, run.SystemID, c, currentActor)
	if err != nil {
		return err
	}
	if err := writeDocumentChangeValue(ctx, tx, *run.DocID, run.SystemID, c, true); err != nil {
		return err
	}
	audit.LogInTx(ctx, tx, log, audit.Event{SystemID: run.SystemID, Actor: currentActor, Action: "document.suggestion_apply", ObjectKind: "document", ObjectID: *run.DocID, After: map[string]any{"field": c.Field, "reason": c.Reason, "policy_version": c.PolicyVersion, "run_id": run.ID}})
	return nil
}

// Authorization stays with the caller; this is the one metadata effect shared
// by a freshly checked automatic candidate and an authenticated human review.
func writeDocumentChangeValue(ctx context.Context, tx *sql.Tx, docID, systemID int64, c DocumentChange, reviewed bool) error {
	now := time.Now().Unix()
	if c.ValueID == 0 {
		var table taxonomy.NamedTable
		switch c.Field {
		case "tag":
			table = taxonomy.TableTags
		case "correspondent":
			table = taxonomy.TableCorrespondents
		case "document_type":
			table = taxonomy.TableDocumentTypes
		}
		if table != 0 {
			var err error
			c.ValueID, err = taxonomy.UpsertByName(ctx, tx, systemID, table, c.Value, now)
			if err != nil {
				return err
			}
		}
	}
	var result sql.Result
	var err error
	switch c.Field {
	case "jd_category":
		result, err = tx.ExecContext(ctx, `UPDATE documents SET jd_category_id=?,updated_at=? WHERE id=? AND jd_category_id<>?`, c.ValueID, now, docID, c.ValueID)
	case "correspondent":
		result, err = tx.ExecContext(ctx, `UPDATE documents SET correspondent_id=?,updated_at=? WHERE id=? AND correspondent_id IS NULL`, c.ValueID, now, docID)
	case "document_type":
		result, err = tx.ExecContext(ctx, `UPDATE documents SET document_type_id=?,updated_at=? WHERE id=? AND document_type_id IS NULL`, c.ValueID, now, docID)
	case "title":
		result, err = tx.ExecContext(ctx, `UPDATE documents SET title=?,updated_at=? WHERE id=? AND title<>?`, c.Value, now, docID, c.Value)
	case "language":
		value := lang.Format(c.Value)
		lock := 0
		if reviewed && value != "" {
			lock = 1
		}
		result, err = tx.ExecContext(ctx, `UPDATE documents SET languages=?,languages_locked=?,updated_at=? WHERE id=? AND (languages<>? OR languages_locked<>?)`, value, lock, now, docID, value, lock)
	case "tag":
		result, err = tx.ExecContext(ctx, `INSERT INTO document_tags(document_id,tag_id) VALUES(?,?) ON CONFLICT(document_id,tag_id) DO UPDATE SET classifier_owned=0 WHERE classifier_owned=1`, docID, c.ValueID)
	default:
		return ErrStaleProposal
	}
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrStaleProposal
	}
	return view.EnqueueMove(ctx, tx, docID)
}

// DocumentChangeProjection returns bounded display values only. The caller must
// first authorize the target and filter each supporter before exposing sources.
func DocumentChangeProjection(ctx context.Context, q systems.Queryer, docID int64, vars map[string]any) (map[string]any, error) {
	c, err := documentChangeFromVars(vars)
	out := map[string]any{"source_current": false, "review_conflict": true}
	if err != nil {
		return out, nil
	}
	reason := c.Reason
	if reason != ReviewReasonReviewFirst && reason != ReviewReasonLowConfidence {
		reason = "unknown"
	}
	policy := c.PolicyVersion
	if policy != ReviewPolicyVersion {
		policy = ""
	}
	out["field"], out["reason"], out["policy_version"], out["confidence"] = c.Field, reason, policy, c.Confidence
	switch c.Source {
	case "archive", "llm", "language-detector":
		out["source"] = c.Source
	}
	out["threshold"] = c.Threshold
	current, err := documentstate.Load(ctx, q, docID)
	if err != nil {
		return out, nil
	}
	fresh := c.Baseline != nil && current.SameSource(*c.Baseline)
	out["source_current"] = fresh
	out["review_conflict"] = !fresh || current.FieldRevision(c.Field) != c.Baseline.FieldRevision(c.Field) || c.PolicyVersion != ReviewPolicyVersion
	var currentValue string
	err = q.QueryRowContext(ctx, `SELECT CASE ? WHEN 'title' THEN d.title WHEN 'language' THEN trim(d.languages,',') WHEN 'correspondent' THEN COALESCE(c.name,'') WHEN 'document_type' THEN COALESCE(dt.name,'') WHEN 'jd_category' THEN COALESCE(j.name,'') WHEN 'tag' THEN COALESCE((SELECT group_concat(t.name,', ') FROM document_tags link JOIN tags t ON t.id=link.tag_id WHERE link.document_id=d.id AND link.classifier_owned=0),'') END FROM documents d LEFT JOIN correspondents c ON c.id=d.correspondent_id LEFT JOIN document_types dt ON dt.id=d.document_type_id LEFT JOIN jd_categories j ON j.id=d.jd_category_id WHERE d.id=?`, c.Field, docID).Scan(&currentValue)
	if err != nil {
		return nil, err
	}
	proposed := c.Value
	if c.ValueID > 0 {
		table := map[string]string{"jd_category": "jd_categories", "correspondent": "correspondents", "document_type": "document_types", "tag": "tags"}[c.Field]
		if err := q.QueryRowContext(ctx, "SELECT name FROM "+table+" WHERE id=? AND system_id=?", c.ValueID, current.SystemID).Scan(&proposed); err != nil {
			out["review_conflict"] = true
			proposed = "Unavailable"
		}
	}
	if len(currentValue) > 2048 {
		currentValue = currentValue[:2048]
	}
	if len(proposed) > 1024 {
		proposed = proposed[:1024]
	}
	out["current_value"], out["proposed_value"] = currentValue, proposed
	return out, nil
}
