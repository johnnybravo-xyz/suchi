package llmclassifier

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/lang"
	"github.com/johnnybravo-xyz/suchi/core/render/view"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// PipelineVersionLLM is the "when did this doc last see the LLM
// classifier?" marker. Bump when the model, prompt template, or
// JSON schema changes so `suchi rescan --stale llm` picks the doc
// up. Exported so main.go can read it into the version snapshot
// it hands to core/rescan (which doesn't import this plugin to
// keep the dep graph flat).
const PipelineVersionLLM = 1

// OnFallbackFn is the "run heuristics fallback for this doc" hook
// main.go wires. Called after the WriteTx commits when the LLM's
// verdict was low-confidence — the archive-based automation gets a
// chance to fill fields the LLM wasn't sure about. Nil means "no
// fallback wired" (headless deploys with LLM but no automations
// engine).
type OnFallbackFn func(ctx context.Context, docID int64) error

// Handler is the durable-outbox Subscriber that runs the classifier
// on `post-classify` jobs. Registered by main.go only when the plugin
// is enabled; otherwise post-classify jobs go to a dead-letter which
// is what we want ("someone enqueued this but nobody's configured to
// serve it").
//
// The plumbing is deliberately thin. Everything the Plugin needs to
// know about a doc is loaded here and passed into Classify(). Result
// application is confidence-gated:
//
//   - confidence >= ConfidenceThreshold → apply the fields
//     (title if currently empty, correspondent upsert, tags,
//     jd_category via code → id lookup)
//   - confidence <  ConfidenceThreshold → tag `needs-review`;
//     leave the doc in its current category
//
// Everything runs in one write tx so a partial application never
// lands. The plugin's own network call happens outside the tx to
// avoid holding the SQLite writer during a slow LLM call.
type Handler struct {
	plugin     *Plugin
	db         dbHandle
	log        *slog.Logger
	onFallback OnFallbackFn
}

// dbHandle mirrors the small surface of *core/db.DB that Handler
// touches. Left as an interface so tests can inject a mock without
// standing up a real SQLite.
type dbHandle interface {
	WriteTx(ctx context.Context, fn func(tx *sql.Tx) error) error
	ReadQueryRow(ctx context.Context, query string, args ...any) *sql.Row
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

// WithFallback wires the archive-heuristics fallback hook. Called
// after the WriteTx commits on the low-confidence branch — the
// operator gets both signals side by side, matching the "if LLM is
// unsure, corroborate with the archive" semantics main.go set up.
func (h *Handler) WithFallback(fn OnFallbackFn) *Handler {
	if h != nil {
		h.onFallback = fn
	}
	return h
}

// Kinds implements pluginapi.Subscriber.
func (h *Handler) Kinds() []string { return []string{Kind} }

// Handle is the Subscriber entrypoint.
func (h *Handler) Handle(ctx context.Context, e pluginapi.Event) error {
	log := h.log.With("doc_id", e.DocID)

	title, content, err := h.loadDoc(ctx, e.DocID)
	if err != nil {
		return fmt.Errorf("load doc: %w", err)
	}
	if content == "" && title == "" {
		log.Info("llm-classifier.skip.empty", "reason", "no title or content")
		return nil
	}

	res, err := h.plugin.Classify(ctx, title, content)
	if err != nil {
		// Best-effort classification — a transient LLM error goes
		// through the outbox retry path via a returned err. When it
		// hits attempts=5 it lands in state=dead and shows up in
		// /api/tasks/.
		return fmt.Errorf("classify: %w", err)
	}
	log.Info("llm-classifier.result",
		"confidence", res.Confidence,
		"title", res.Title,
		"correspondent", res.Correspondent,
		"jd_category", res.JDCategory,
		"tags", res.Tags)

	// Low-confidence path: apply only the needs-review tag so an
	// operator sees the doc in the review queue. Leaves title,
	// correspondent, jd_category untouched.
	threshold := h.plugin.Config().ConfidenceThreshold
	lowConfidence := res.Confidence < threshold
	if lowConfidence {
		log.Info("llm-classifier.low_confidence",
			"threshold", threshold,
			"reasoning", res.Reasoning)
	}

	if err := h.db.WriteTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().Unix()

		if lowConfidence {
			// Intentional: low-confidence docs keep pipeline_version_llm=0
			// so `suchi rescan --stale llm` re-tries them after prompt/model
			// tweaks. Bumping here would freeze the outcome at the current
			// version and hide the retry opportunity.
			return upsertTagAndAttach(ctx, tx, "needs-review", e.DocID, now)
		}

		// Update title only when currently empty — respect operator
		// edits and rules-engine picks.
		if res.Title != "" {
			if _, err := tx.ExecContext(ctx, `
				UPDATE documents
				SET title = CASE WHEN title = '' THEN ? ELSE title END,
				    updated_at = ?
				WHERE id = ?
			`, res.Title, now, e.DocID); err != nil {
				return err
			}
		}

		if res.Correspondent != "" {
			corID, err := upsertByName(ctx, tx, "correspondents", res.Correspondent, now)
			if err != nil {
				return err
			}
			// Only set the primary FK when unset — mirror the rules
			// engine's non-overwrite policy for setters.
			if _, err := tx.ExecContext(ctx, `
				UPDATE documents
				SET correspondent_id = COALESCE(correspondent_id, ?),
				    updated_at = ?
				WHERE id = ?
			`, corID, now, e.DocID); err != nil {
				return err
			}
			// Also register as sender in the multi-correspondent
			// junction, matching the AddDocCorrespondent semantics.
			if _, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO document_correspondents(document_id, correspondent_id, role)
				VALUES (?, ?, 'sender')
			`, e.DocID, corID); err != nil {
				return err
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
		// `suchi rescan --stale llm` to pick out docs still on an
		// older LLM config after the operator swaps model/prompt.
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

	// Low-confidence path fires the archive-heuristics fallback. LLM
	// was unsure; the automation may still corroborate a jd_category
	// or correspondent from the archive. Runs outside the WriteTx —
	// the automation opens its own; nested writes on the single-
	// writer pool would deadlock. Nil hook = no fallback wired.
	if lowConfidence && h.onFallback != nil {
		if err := h.onFallback(ctx, e.DocID); err != nil {
			log.Warn("llm-classifier.fallback.error", "err", err.Error())
		}
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

func upsertByName(ctx context.Context, tx *sql.Tx, table, name string, now int64) (int64, error) {
	if name == "" {
		return 0, errors.New("empty name")
	}
	slug := slugify(name)
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s(name, slug, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET updated_at = excluded.updated_at
	`, table), name, slug, now, now); err != nil {
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

func slugify(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	s := b.String()
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return strings.Trim(s, "-")
}
