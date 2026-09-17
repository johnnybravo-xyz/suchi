package llmclassifier

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/documentstate"
	"github.com/johnnybravo-xyz/suchi/core/intelligence"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	"github.com/johnnybravo-xyz/suchi/core/lang"
	"github.com/johnnybravo-xyz/suchi/core/render/view"
	"github.com/johnnybravo-xyz/suchi/core/settings"
	"github.com/johnnybravo-xyz/suchi/core/similar"
	"github.com/johnnybravo-xyz/suchi/core/slug"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// PipelineVersionLLM is the "when did this doc last see the LLM
// classifier?" marker. Bump when the prompt template, validation contract, or
// result schema changes so `suchi rescan --stale llm` picks the doc
// up. Exported so main.go can read it into the version snapshot
// it hands to core/rescan (which doesn't import this plugin to
// keep the dep graph flat).
const PipelineVersionLLM = 3

// Handler is the durable-outbox Subscriber that runs the classifier on
// `post-classify` jobs. Main registers it in a disabled state at boot so the
// first settings save can activate classification without restarting.
//
// The plumbing is deliberately thin. Everything the Plugin needs to
// know about a doc is loaded here before Classify(). Source-bound inferences
// apply only under the current application mode and confidence threshold.
//
// Everything runs in one write tx so a partial application never
// lands. The plugin's own network call happens outside the tx to
// avoid holding the SQLite writer during a slow LLM call.
type Handler struct {
	plugin *Plugin
	db     *db.DB
	log    *slog.Logger
}

// NewHandler returns nil when the plugin is nil.
func NewHandler(p *Plugin, database *db.DB, log *slog.Logger) *Handler {
	if p == nil {
		return nil
	}
	return &Handler{plugin: p, db: database, log: log.With("component", "llm-classifier.handler")}
}

// Kinds implements pluginapi.Subscriber.
func (h *Handler) Kinds() []string { return []string{Kind} }

// Handle is the Subscriber entrypoint.
func (h *Handler) Handle(ctx context.Context, e pluginapi.Event) error {
	startRuntime := h.plugin.rt.Load()
	if startRuntime == nil {
		return nil
	}
	// Load the revision first and confirm after reading text so the model never
	// receives text from a different generation than the retained baseline.
	baseline, err := documentstate.Load(ctx, h.db.Read, e.DocID)
	if err != nil {
		return err
	}
	title, content, _, err := h.loadDoc(ctx, e.DocID)
	if err != nil {
		return err
	}
	confirmed, err := documentstate.Load(ctx, h.db.Read, e.DocID)
	if err != nil {
		return err
	}
	if baseline != confirmed {
		return errors.New("classifier document changed while loading")
	}
	if e.SystemID != 0 && e.SystemID != baseline.SystemID {
		return errors.New("classifier: mismatched job system")
	}
	if content == "" && title == "" {
		return nil
	}
	jdCats, err := h.loadJDCategories(ctx, baseline.SystemID)
	if err != nil {
		return err
	}
	siblings, supporters, err := h.loadNamingContext(ctx, e.DocID, baseline, 5)
	if err != nil {
		return err
	}
	res, err := h.plugin.Classify(ctx, title, content, jdCats, siblings)
	if errors.Is(err, ErrDisabled) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("classify: %w", err)
	}
	threshold := startRuntime.cfg.ConfidenceThreshold
	dates, err := prepareDateCandidates(content, res.Dates, threshold)
	if err != nil {
		return err
	}
	return h.db.WriteTx(ctx, func(tx *sql.Tx) error {
		if h.plugin.rt.Load() != startRuntime {
			return errors.New("classifier configuration changed during request")
		}
		current, err := documentstate.Load(ctx, tx, e.DocID)
		if err != nil {
			return err
		}
		if !baseline.SameSource(current) {
			return errors.New("classifier source changed during request")
		}
		if err := checkNamingContext(ctx, tx, e.DocID, current, supporters); err != nil {
			return err
		}
		autoApply, err := settings.ResolveAutoApply(ctx, tx)
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		if err := replaceDateCandidatesInTx(ctx, tx, e.DocID, baseline, dates, autoApply, now); err != nil {
			return err
		}
		applicationBaseline := current
		var pending []approvals.DocumentChange
		propose := func(change approvals.DocumentChange) error {
			if baseline.FieldRevision(change.Field) != current.FieldRevision(change.Field) {
				return nil // A human/rule reassertion wins, including same-value ABA.
			}
			change.Confidence, change.Threshold = res.Confidence, &threshold
			change.Source, change.Baseline = "llm", &applicationBaseline
			change.Supporters = supporters
			if autoApply {
				applied, err := approvals.ApplyAutomaticDocumentChangeInTx(ctx, tx, h.log, e.DocID, change)
				if err != nil {
					return err
				}
				if applied {
					applicationBaseline, err = documentstate.Load(ctx, tx, e.DocID)
					return err
				}
			}
			pending = append(pending, change)
			return nil
		}
		if res.Title != "" && res.Title != title {
			if err := propose(approvals.DocumentChange{Field: "title", Value: res.Title, Label: res.Title}); err != nil {
				return err
			}
		}
		var hasCorrespondent, inInbox, languageLocked bool
		var languages string
		if err := tx.QueryRowContext(ctx, `
			SELECT (d.correspondent_id IS NOT NULL OR EXISTS (
			        SELECT 1 FROM document_correspondents dc WHERE dc.document_id=d.id AND dc.role='sender')),
			       COALESCE(d.jd_category_id = js.inbox_category_id, 0), d.languages_locked, COALESCE(d.languages, '')
			FROM documents d JOIN jd_systems js ON js.id=d.system_id WHERE d.id=?
		`, e.DocID).Scan(&hasCorrespondent, &inInbox, &languageLocked, &languages); err != nil {
			return err
		}
		if !hasCorrespondent && res.Correspondent != "" {
			if err := propose(approvals.DocumentChange{Field: "correspondent", Value: res.Correspondent, Label: res.Correspondent}); err != nil {
				return err
			}
		}
		if inInbox && res.JDCategory > 0 {
			var catID int64
			var label string
			err := tx.QueryRowContext(ctx, `SELECT id, name FROM jd_categories WHERE system_id=? AND code=? AND system=0`, baseline.SystemID, res.JDCategory).Scan(&catID, &label)
			if err == nil {
				if err := propose(approvals.DocumentChange{Field: "jd_category", ValueID: catID, Label: label}); err != nil {
					return err
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		modelRequestedReview := false
		for _, tag := range res.Tags {
			if slug.Make(tag) == "needs-review" {
				modelRequestedReview = true
				continue
			}
			var attached bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
				SELECT 1 FROM document_tags dt JOIN tags t ON t.id=dt.tag_id
				WHERE dt.document_id=? AND (t.name=? COLLATE NOCASE OR t.slug=?))`,
				e.DocID, tag, slug.Make(tag)).Scan(&attached); err != nil {
				return err
			}
			if !attached {
				if err := propose(approvals.DocumentChange{Field: "tag", Value: tag, Label: tag}); err != nil {
					return err
				}
			}
		}
		if code := lang.Format(res.Language); !languageLocked && code != "" && code != languages {
			if err := propose(approvals.DocumentChange{Field: "language", Value: code, Label: code}); err != nil {
				return err
			}
		}
		// Bind review fallback after our own writes, not an intermediate tag
		// revision. The captured-vs-entry comparison above still protects humans.
		for _, change := range pending {
			change.Baseline = &applicationBaseline
			if err := approvals.ProposeDocumentChangeInTx(ctx, tx, e.DocID, change); err != nil {
				return err
			}
		}
		var needsReview bool
		if err := tx.QueryRowContext(ctx, `SELECT
			EXISTS (SELECT 1 FROM document_intelligence WHERE document_id=? AND status='pending')
			OR EXISTS (SELECT 1 FROM approval_runs r JOIN approval_defs d ON d.id=r.def_id
				WHERE r.doc_id=? AND r.state='running' AND d.slug=?)
		`, e.DocID, e.DocID, approvals.DocumentChangeSlug).Scan(&needsReview); err != nil {
			return err
		}
		// Internal review markers are not model vocabulary and cannot clear a
		// human-owned assignment. A score never resolves outstanding review.
		if needsReview {
			if _, err := upsertTagAndAttach(ctx, tx, "needs-review", e.DocID, now, true); err != nil {
				return err
			}
		} else if res.Confidence >= threshold && !modelRequestedReview && baseline.FieldRevision("tag") == current.FieldRevision("tag") {
			if _, err := tx.ExecContext(ctx, `DELETE FROM document_tags
				WHERE document_id=? AND classifier_owned=1 AND tag_id IN (
					SELECT id FROM tags WHERE system_id=? AND slug='needs-review'
				)`, e.DocID, baseline.SystemID); err != nil {
				return err
			}
		}
		if h.plugin.rt.Load() != startRuntime {
			return errors.New("classifier configuration changed while persisting result")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE documents SET pipeline_version_llm=?, updated_at=? WHERE id=?`, PipelineVersionLLM, now, e.DocID); err != nil {
			return err
		}
		return view.EnqueueMove(ctx, tx, e.DocID)
	})
}

func (h *Handler) loadDoc(ctx context.Context, id int64) (title, content, sourceBlob string, err error) {
	var contentNull sql.NullString
	err = h.db.Read.QueryRowContext(ctx, `
		SELECT title, content, original_blob FROM documents
		WHERE id = ? AND trashed_at IS NULL
	`, id).Scan(&title, &contentNull, &sourceBlob)
	if err != nil {
		return
	}
	if contentNull.Valid {
		content = contentNull.String
	}
	return
}

// loadJDCategories returns the operator's user-facing JD categories in
// code order. System-only rows (Inbox and its siblings) are excluded —
// the LLM should never pick them as a suggested classification, and
// dropping them from the prompt cuts token cost.
func (h *Handler) loadJDCategories(ctx context.Context, systemID int64) ([]JDCat, error) {
	rows, err := h.db.Read.QueryContext(ctx,
		`SELECT code, name FROM jd_categories WHERE system_id = ? AND system = 0 ORDER BY code`, systemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JDCat
	for rows.Next() {
		var c JDCat
		if err := rows.Scan(&c.Code, &c.Name); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Keep authorized selection, prompt titles and provenance in one read snapshot.
// Similar titles are scoped to the document owner's ACLs, even for an admin.
func (h *Handler) loadNamingContext(ctx context.Context, docID int64, baseline documentstate.Snapshot, limit int) ([]string, []documentstate.Reference, error) {
	tx, err := h.db.Read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	current, err := documentstate.Load(ctx, tx, docID)
	if err != nil {
		return nil, nil, err
	}
	if current != baseline {
		return nil, nil, errors.New("classifier document changed while loading naming context")
	}
	groups, err := authz.LoadGroupsInTx(ctx, tx, baseline.OwnerID)
	if err != nil {
		return nil, nil, err
	}
	docs, err := similar.TopDocsInTx(ctx, tx, docID, limit, &similar.Principal{
		UserID: baseline.OwnerID, Role: "user", Groups: groups, SystemID: baseline.SystemID,
	})
	if err != nil {
		return nil, nil, err
	}
	titles := make([]string, 0, len(docs))
	supporters := make([]documentstate.Reference, 0, len(docs))
	for _, doc := range docs {
		if doc.Title == "" {
			continue
		}
		snapshot, err := documentstate.Load(ctx, tx, doc.ID)
		if err != nil {
			return nil, nil, err
		}
		titles = append(titles, doc.Title)
		supporters = append(supporters, documentstate.Reference{DocumentID: doc.ID, Snapshot: snapshot})
	}
	if err := checkNamingContext(ctx, tx, docID, baseline, supporters); err != nil {
		return nil, nil, err
	}
	return titles, supporters, nil
}

// Recheck all model inputs before persisting even a date-only result. Dates
// still need exact target-text evidence; sibling titles only inform naming.
func checkNamingContext(ctx context.Context, tx *sql.Tx, docID int64, target documentstate.Snapshot, supporters []documentstate.Reference) error {
	allowed, err := systems.CanEnter(ctx, tx, target.OwnerID, target.SystemID)
	if err != nil {
		return err
	}
	if !allowed {
		return errors.New("classifier document owner no longer authorized")
	}
	groups, err := authz.LoadGroupsInTx(ctx, tx, target.OwnerID)
	if err != nil {
		return err
	}
	principal := authz.Principal{UserID: target.OwnerID, Role: "user", Groups: groups, SystemID: target.SystemID}
	if err := (authz.ACLAuthorizer{}).CanInTx(ctx, tx, principal, authz.KindDocument, docID, authz.PermView); err != nil {
		return err
	}
	for _, ref := range supporters {
		current, err := documentstate.Load(ctx, tx, ref.DocumentID)
		if err != nil {
			return err
		}
		if current != ref.Snapshot || current.SystemID != target.SystemID {
			return errors.New("classifier naming context changed during request")
		}
		allowed, err := systems.CanEnter(ctx, tx, current.OwnerID, current.SystemID)
		if err != nil {
			return err
		}
		if !allowed {
			return errors.New("classifier naming source owner no longer authorized")
		}
		if err := (authz.ACLAuthorizer{}).CanInTx(ctx, tx, principal, authz.KindDocument, ref.DocumentID, authz.PermView); err != nil {
			return err
		}
	}
	return nil
}

type preparedDateCandidate struct {
	intelligence.Candidate
	evidenceStart sql.NullInt64
	reason        string
	autoEligible  bool
}

func prepareDateCandidates(content string, dates []DateCandidate, threshold float64) ([]preparedDateCandidate, error) {
	prepared := make([]preparedDateCandidate, 0, len(dates))
	for _, date := range dates {
		candidate, err := intelligence.NewDateCandidate(date.Role, date.Value, date.Precision, date.RawText, date.Evidence, date.Confidence)
		if err != nil {
			return nil, err
		}
		start, valid := intelligence.EvidenceStart(content, candidate)
		if !valid {
			continue
		}
		reason := approvals.ReviewReasonImportantFact
		if candidate.Confidence < threshold {
			reason = approvals.ReviewReasonLowConfidence
		}
		prepared = append(prepared, preparedDateCandidate{
			Candidate: candidate, evidenceStart: sql.NullInt64{Int64: start, Valid: true}, reason: reason,
			autoEligible: candidate.Confidence >= threshold,
		})
	}
	return prepared, nil
}

func replaceDateCandidatesInTx(ctx context.Context, tx *sql.Tx, docID int64, source documentstate.Snapshot, dates []preparedDateCandidate, autoApply bool, now int64) error {
	const extractor = "llm-classifier"
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM document_intelligence
		WHERE document_id = ? AND intelligence_type = ? AND extractor = ?
		  AND status = 'pending'
	`, docID, intelligence.TypeDate, extractor); err != nil {
		return err
	}
	for _, date := range dates {
		status, reason, policyVersion := "pending", date.reason, approvals.ReviewPolicyVersion
		if autoApply && date.autoEligible {
			status, reason, policyVersion = "accepted", "confidence_threshold", approvals.AutomaticPolicyVersion
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO document_intelligence(
				document_id, intelligence_type, role, value_json, sort_value,
				raw_text, evidence_text, evidence_start, confidence, status,
				extractor, source_blob, source_revision, gate_reason, gate_policy_version, extraction_version, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(document_id, intelligence_type, role, value_json, evidence_text, extractor)
			DO NOTHING
		`, docID, date.Type, date.Role, date.ValueJSON, date.SortValue,
			date.RawText, date.EvidenceText, date.evidenceStart, date.Confidence, status,
			extractor, source.SourceBlob, source.SourceRevision, reason, policyVersion, PipelineVersionLLM, now, now); err != nil {
			return err
		}
	}
	return nil
}

func upsertTagAndAttach(ctx context.Context, tx *sql.Tx, name string, docID int64, now int64, classifierOwned bool) (int64, error) {
	var systemID int64
	if err := tx.QueryRowContext(ctx, `SELECT system_id FROM documents WHERE id = ?`, docID).Scan(&systemID); err != nil {
		return 0, err
	}
	tagID, err := taxonomy.UpsertByName(ctx, tx, systemID, taxonomy.TableTags, name, now)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO document_tags(document_id, tag_id, classifier_owned) VALUES (?, ?, ?)`,
		docID, tagID, classifierOwned)
	return tagID, err
}
