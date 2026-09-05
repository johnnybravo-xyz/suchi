package llmclassifier

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/intelligence"
	"github.com/johnnybravo-xyz/suchi/core/lang"
	"github.com/johnnybravo-xyz/suchi/core/render/view"
	"github.com/johnnybravo-xyz/suchi/core/similar"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// PipelineVersionLLM is the "when did this doc last see the LLM
// classifier?" marker. Bump when the prompt template, validation contract, or
// result schema changes so `suchi rescan --stale llm` picks the doc
// up. Exported so main.go can read it into the version snapshot
// it hands to core/rescan (which doesn't import this plugin to
// keep the dep graph flat).
const PipelineVersionLLM = 2

// Handler is the durable-outbox Subscriber that runs the classifier on
// `post-classify` jobs. Main registers it in a disabled state at boot so the
// first settings save can activate classification without restarting.
//
// The plumbing is deliberately thin. Everything the Plugin needs to
// know about a doc is loaded here and passed into Classify(). Result
// application is confidence-gated:
//
//   - confidence >= ConfidenceThreshold → apply the fields
//     (suggested title, unresolved correspondent, tags,
//     jd_category via code → id lookup)
//   - confidence < ConfidenceThreshold → tag `needs-review`, retain the
//     current metadata, and offer a title suggestion for review
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
	log := h.log.With("doc_id", e.DocID)
	startRuntime := h.plugin.rt.Load()
	if startRuntime == nil {
		log.Debug("llm-classifier.skip.disabled")
		return nil
	}

	title, content, sourceBlob, err := h.loadDoc(ctx, e.DocID)
	if err != nil {
		return fmt.Errorf("load doc: %w", err)
	}
	if content == "" && title == "" {
		log.Info("llm-classifier.skip.empty", "reason", "no title or content")
		return nil
	}

	// Load the installation's JD categories so the model gets real
	// codes + names instead of a bare "integer 10-99" hint. Empty
	// slice on read error keeps classify best-effort — the model
	// falls back to guessing rather than blocking on a taxonomy read.
	jdCats, err := h.loadJDCategories(ctx)
	if err != nil {
		log.Warn("llm-classifier.jd_load_error", "err", err.Error())
	}

	// Fetch sibling titles for few-shot title stability — the model
	// mirrors the pattern of previously-accepted titles instead of
	// inventing new phrasings ("Electricity bill - August 2026" vs
	// "Utilities:electricity bill - 10/2026"). One-classify-per-doc
	// invariant means we inject them into THIS call, not a follow-up.
	// Empty slice on read error is a clean no-op.
	siblings, err := h.loadSiblingTitles(ctx, e.DocID, 5)
	if err != nil {
		log.Warn("llm-classifier.siblings_load_error", "err", err.Error())
	}

	res, err := h.plugin.Classify(ctx, title, content, jdCats, siblings)
	if err != nil {
		if errors.Is(err, ErrDisabled) {
			return nil
		}
		// Best-effort classification — a transient LLM error goes
		// through the outbox retry path via a returned err. When it
		// hits attempts=5 it lands in state=dead and shows up in
		// /api/tasks/.
		return fmt.Errorf("classify: %w", err)
	}
	if current := h.plugin.rt.Load(); current != startRuntime {
		if current == nil {
			log.Info("llm-classifier.result.discarded", "reason", "disabled during request")
			return nil
		}
		return errors.New("classifier configuration changed during request")
	}
	log.Info("llm-classifier.result",
		"confidence", res.Confidence,
		"jd_category", res.JDCategory,
		"has_title", res.Title != "",
		"has_correspondent", res.Correspondent != "",
		"tag_count", len(res.Tags),
		"date_count", len(res.Dates),
		"date_auto_apply", startRuntime.cfg.DateAutoApply,
		"auto_apply_threshold", startRuntime.cfg.ConfidenceThreshold)

	// Low-confidence path: apply only the needs-review tag so an
	// operator sees the doc in the review queue. Leaves title,
	// correspondent, jd_category untouched.
	threshold := startRuntime.cfg.ConfidenceThreshold
	lowConfidence := res.Confidence < threshold
	if lowConfidence {
		log.Info("llm-classifier.low_confidence", "threshold", threshold)
	}
	// Prepare date evidence before taking SQLite's writer.
	preparedDates, err := prepareDateCandidates(content, res.Dates, startRuntime.cfg.DateAutoApply, threshold)
	if err != nil {
		return fmt.Errorf("prepare date candidates: %w", err)
	}

	if err := h.db.WriteTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()
		if err := replaceDateCandidatesInTx(ctx, tx, e.DocID, sourceBlob, preparedDates, now); err != nil {
			return err
		}

		if lowConfidence {
			if err := upsertTagAndAttach(ctx, tx, "needs-review", e.DocID, now); err != nil {
				return err
			}
			if res.Title != "" && res.Title != title {
				if err := approvals.ProposeDocumentChangeInTx(ctx, tx, e.DocID, approvals.DocumentChange{
					Field: "title", Value: res.Title, Label: res.Title,
					Confidence: res.Confidence, Source: "llm",
				}); err != nil {
					return err
				}
			}
			// Confidence controls whether suggestions apply, not whether the
			// classifier completed. Stamp the successful revision so version 0
			// remains an unambiguous "never completed" marker.
			if _, err := tx.ExecContext(ctx, `
				UPDATE documents
				SET pipeline_version_llm = ?, updated_at = ?
				WHERE id = ?
			`, PipelineVersionLLM, now, e.DocID); err != nil {
				return err
			}
			return view.EnqueueMove(ctx, tx, e.DocID)
		}

		if res.Title != "" && res.Title != title {
			if _, err := tx.ExecContext(ctx, `
				UPDATE documents SET title = ?, updated_at = ? WHERE id = ?
			`, res.Title, now, e.DocID); err != nil {
				return err
			}
		}

		if res.Correspondent != "" {
			var currentCorrespondent sql.NullInt64
			if err := tx.QueryRowContext(ctx, `
				SELECT COALESCE(
					d.correspondent_id,
					(
						SELECT dc.correspondent_id
						FROM document_correspondents dc
						WHERE dc.document_id = d.id AND dc.role = 'sender'
						ORDER BY dc.position, dc.correspondent_id
						LIMIT 1
					)
				)
				FROM documents d
				WHERE d.id = ?
			`, e.DocID).Scan(&currentCorrespondent); err != nil {
				return err
			}
			// Parsed email headers and explicit metadata outrank a model guess.
			if currentCorrespondent.Valid {
				// Heal attachment rows created before sender inheritance also
				// populated the legacy primary FK.
				if _, err := tx.ExecContext(ctx, `
					UPDATE documents
					SET correspondent_id = ?
					WHERE id = ? AND correspondent_id IS NULL
				`, currentCorrespondent.Int64, e.DocID); err != nil {
					return err
				}
			} else {
				corID, err := taxonomy.UpsertByName(ctx, tx, taxonomy.TableCorrespondents,
					res.Correspondent, now)
				if err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `
					UPDATE documents
						SET correspondent_id = ?, updated_at = ?
						WHERE id = ? AND correspondent_id IS NULL
				`, corID, now, e.DocID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `
					INSERT OR IGNORE INTO document_correspondents(document_id, correspondent_id, role)
					VALUES (?, ?, 'sender')
				`, e.DocID, corID); err != nil {
					return err
				}
			}
		}

		if res.JDCategory > 0 {
			var catID int64
			err := tx.QueryRowContext(ctx,
				`SELECT id FROM jd_categories WHERE code = ?`, res.JDCategory).Scan(&catID)
			if err == nil {
				// Only move OUT of the inbox — never override an
				// already-classified doc. Operators expect the LLM to
				// clean up the review queue, not undo their filing.
				if _, err := tx.ExecContext(ctx, `
					UPDATE documents
					SET jd_category_id = CASE
					    WHEN jd_category_id = (
					        SELECT value_json FROM settings WHERE key = 'jd_inbox_category_id'
					    ) THEN ?
					    ELSE jd_category_id
					    END,
					    updated_at = ?
					WHERE id = ?
				`, catID, now, e.DocID); err != nil {
					return err
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}

		for _, tag := range res.Tags {
			tag = strings.TrimSpace(tag)
			if tag == "" {
				continue
			}
			if err := upsertTagAndAttach(ctx, tx, tag, e.DocID, now); err != nil {
				return err
			}
		}

		// Language — the LLM returns an ISO code (or short CSV) in
		// res.Language. Normalise via core/lang.Format and write only
		// when the doc isn't user-locked. The WHERE guard means a
		// human PATCH survives future LLM re-runs.
		if code := lang.Format(res.Language); code != "" {
			if _, err := tx.ExecContext(ctx, `
				UPDATE documents
				SET languages = ?, updated_at = ?
				WHERE id = ? AND languages_locked = 0
			`, code, now, e.DocID); err != nil {
				return err
			}
		}

		// Bump the LLM pipeline-version marker on this doc. Used by
		// `suchi rescan --stale llm` to pick out docs that predate a
		// classifier code or prompt revision. Configuration changes are
		// re-run only through an explicit selected rescan.
		// Kept as a bare integer here rather than an import from
		// core/postingest to keep the plugin's dep graph flat.
		if _, err := tx.ExecContext(ctx, `
			UPDATE documents
			SET pipeline_version_llm = ?, updated_at = ?
			WHERE id = ?
		`, PipelineVersionLLM, now, e.DocID); err != nil {
			return err
		}

		// Re-render the storage-path symlink after metadata changes.
		// Enqueue in the same tx so a crash between metadata write and
		// enqueue is impossible — the outbox pattern is the whole point.
		return view.EnqueueMove(ctx, tx, e.DocID)
	}); err != nil {
		return err
	}

	return nil
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
func (h *Handler) loadJDCategories(ctx context.Context) ([]JDCat, error) {
	rows, err := h.db.Read.QueryContext(ctx,
		`SELECT code, name FROM jd_categories WHERE system = 0 ORDER BY code`)
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

// Similar titles are scoped to the document owner's ACLs, even for an admin.
func (h *Handler) loadSiblingTitles(ctx context.Context, docID int64, limit int) ([]string, error) {
	var ownerID int64
	if err := h.db.Read.QueryRowContext(ctx,
		`SELECT owner_id FROM documents WHERE id = ?`, docID).Scan(&ownerID); err != nil {
		return nil, err
	}
	groups, err := authz.LoadGroups(ctx, h.db, ownerID)
	if err != nil {
		return nil, err
	}
	docs, err := similar.TopDocs(ctx, h.db, docID, limit, &similar.Principal{
		UserID: ownerID, Role: "user", Groups: groups,
	})
	if err != nil {
		return nil, err
	}
	titles := make([]string, 0, len(docs))
	for _, doc := range docs {
		if doc.Title != "" {
			titles = append(titles, doc.Title)
		}
	}
	return titles, nil
}

type preparedDateCandidate struct {
	intelligence.Candidate
	evidenceStart sql.NullInt64
	status        string
}

func prepareDateCandidates(content string, dates []DateCandidate, autoApply bool, autoApplyThreshold float64) ([]preparedDateCandidate, error) {
	if len(dates) == 0 {
		return nil, nil
	}
	lowerContent := strings.ToLower(content)
	prepared := make([]preparedDateCandidate, 0, len(dates))
	for _, date := range dates {
		candidate, err := intelligence.NewDateCandidate(
			date.Role, date.Value, date.Precision,
			date.RawText, date.Evidence, date.Confidence,
		)
		if err != nil {
			return nil, err
		}
		status := "pending"
		if autoApply && candidate.Confidence >= autoApplyThreshold {
			status = "accepted"
		}
		var evidenceStart sql.NullInt64
		if index := strings.Index(lowerContent, strings.ToLower(candidate.RawText)); index >= 0 {
			evidenceStart = sql.NullInt64{Int64: int64(index), Valid: true}
		}
		prepared = append(prepared, preparedDateCandidate{
			Candidate: candidate, evidenceStart: evidenceStart, status: status,
		})
	}
	return prepared, nil
}

func replaceDateCandidatesInTx(ctx context.Context, tx *sql.Tx, docID int64, sourceBlob string, dates []preparedDateCandidate, now int64) error {
	const extractor = "llm-classifier"
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM document_intelligence
		WHERE document_id = ? AND intelligence_type = ? AND extractor = ?
		  AND (status = 'pending' OR (status = 'accepted' AND reviewed_at IS NULL))
	`, docID, intelligence.TypeDate, extractor); err != nil {
		return err
	}
	for _, date := range dates {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO document_intelligence(
				document_id, intelligence_type, role, value_json, sort_value,
				raw_text, evidence_text, evidence_start, confidence, status,
				extractor, source_blob, extraction_version, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(document_id, intelligence_type, role, value_json, evidence_text, extractor)
			DO NOTHING
		`, docID, date.Type, date.Role, date.ValueJSON, date.SortValue,
			date.RawText, date.EvidenceText, date.evidenceStart, date.Confidence, date.status,
			extractor, sourceBlob, PipelineVersionLLM, now, now); err != nil {
			return err
		}
	}
	return nil
}

func upsertTagAndAttach(ctx context.Context, tx *sql.Tx, name string, docID int64, now int64) error {
	tagID, err := taxonomy.UpsertByName(ctx, tx, taxonomy.TableTags, name, now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO document_tags(document_id, tag_id) VALUES (?, ?)`,
		docID, tagID)
	return err
}
