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
	"github.com/johnnybravo-xyz/suchi/core/lang"
	"github.com/johnnybravo-xyz/suchi/core/render/view"
	"github.com/johnnybravo-xyz/suchi/core/slug"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// PipelineVersionLLM is the "when did this doc last see the LLM
// classifier?" marker. Bump when the prompt template, validation contract, or
// result schema changes so `suchi rescan --stale llm` picks the doc
// up. Exported so main.go can read it into the version snapshot
// it hands to core/rescan (which doesn't import this plugin to
// keep the dep graph flat).
const PipelineVersionLLM = 1

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
	db     dbHandle
	log    *slog.Logger
}

// dbHandle mirrors the small surface of *core/db.DB that Handler
// touches. Left as an interface so tests can inject a mock without
// standing up a real SQLite.
type dbHandle interface {
	WriteTx(ctx context.Context, fn func(tx *sql.Tx) error) error
	ReadQueryRow(ctx context.Context, query string, args ...any) *sql.Row
	ReadQuery(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	SiblingTitles(ctx context.Context, docID int64, limit int) ([]string, error)
}

// NewHandler wraps a Plugin as a Subscriber. Returns nil when the
// plugin is nil so callers can pass `NewHandler(llm)` unconditionally
// and `if h != nil { disp.Register(h) }` in main.
func NewHandler(p *Plugin, db dbHandle, log *slog.Logger) *Handler {
	if p == nil {
		return nil
	}
	return &Handler{plugin: p, db: db, log: log.With("component", "llm-classifier.handler")}
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

	title, content, err := h.loadDoc(ctx, e.DocID)
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
	siblings, err := h.db.SiblingTitles(ctx, e.DocID, 5)
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
		"tag_count", len(res.Tags))

	// Low-confidence path: apply only the needs-review tag so an
	// operator sees the doc in the review queue. Leaves title,
	// correspondent, jd_category untouched.
	threshold := startRuntime.cfg.ConfidenceThreshold
	lowConfidence := res.Confidence < threshold
	if lowConfidence {
		log.Info("llm-classifier.low_confidence", "threshold", threshold)
	}

	if err := h.db.WriteTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()

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
			if err := tx.QueryRowContext(ctx,
				`SELECT correspondent_id FROM documents WHERE id = ?`, e.DocID,
			).Scan(&currentCorrespondent); err != nil {
				return err
			}
			// Parsed email headers and explicit metadata outrank a model guess.
			if !currentCorrespondent.Valid {
				corID, err := upsertByName(ctx, tx, "correspondents", res.Correspondent, now)
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

func (h *Handler) loadDoc(ctx context.Context, id int64) (title, content string, err error) {
	var contentNull sql.NullString
	err = h.db.ReadQueryRow(ctx, `
		SELECT title, content FROM documents
		WHERE id = ? AND trashed_at IS NULL
	`, id).Scan(&title, &contentNull)
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
	rows, err := h.db.ReadQuery(ctx,
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

func upsertByName(ctx context.Context, tx *sql.Tx, table, name string, now int64) (int64, error) {
	if name == "" {
		return 0, errors.New("empty name")
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s(name, slug, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET updated_at = excluded.updated_at
	`, table), name, slug.Make(name), now, now); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT id FROM %s WHERE name = ?`, table),
		name).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func upsertTagAndAttach(ctx context.Context, tx *sql.Tx, name string, docID int64, now int64) error {
	tagID, err := upsertByName(ctx, tx, "tags", name, now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO document_tags(document_id, tag_id) VALUES (?, ?)`,
		docID, tagID)
	return err
}
