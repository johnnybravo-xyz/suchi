// apply_from_similar — the built-in "auto-file from archive" action.
//
// Fetches the top-K similar existing documents (FTS5 more-like-this
// via core/similar), aggregates their core-four metadata
// (jd_category, correspondent, document_type, tags), auto-applies
// the winners with confidence ≥ threshold_autoapply, and drops the
// weaker tier (>= threshold_propose but < threshold_autoapply) into
// document_proposals for the Tasks inbox.
//
// LLM interaction: when the LLM classifier plugin is configured and
// healthy, `SkipHeuristics` returns true and the action no-ops. The
// LLM handler re-invokes ApplyOnDocumentAdded on terminal failure to
// give heuristics a chance to fill gaps.
//
// Runs inside the automations WriteTx, so every write is atomic with
// the rest of the automation's actions and the whole postingest tail.

package automations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/suchi-dms/suchi/core/audit"
	"github.com/suchi-dms/suchi/core/authz"
	"github.com/suchi-dms/suchi/core/db"
	"github.com/suchi-dms/suchi/core/similar"
)

// heuristicsSkip is set by main.go at boot when the LLM plugin is
// wired. When true, the apply_from_similar action returns a no-op —
// LLM is authoritative in the current stack. On LLM terminal
// failure, the LLM handler re-invokes with ForceHeuristics on the
// context so the action ignores the flag.
var heuristicsSkip bool

// SetHeuristicsSkip is called from main.go once at boot. Not
// intended to be called from anywhere else — no locking. The flag
// only flips at process startup.
func SetHeuristicsSkip(v bool) { heuristicsSkip = v }

// forceHeuristicsKey is a request-scoped override the LLM handler
// uses to re-run heuristics after terminal LLM failure.
type forceHeuristicsKey struct{}

// WithForceHeuristics returns a context that forces the
// apply_from_similar action to run even when heuristicsSkip is true.
// The LLM classifier's error path uses this on retry-exhausted docs.
func WithForceHeuristics(ctx context.Context) context.Context {
	return context.WithValue(ctx, forceHeuristicsKey{}, true)
}

func isForcedHeuristics(ctx context.Context) bool {
	v, _ := ctx.Value(forceHeuristicsKey{}).(bool)
	return v
}

// applyFromSimilarParams is the JSON shape stored in workflow_actions.params_json.
// All fields have sane defaults so an operator seeing the automation
// in the visual builder doesn't have to fill anything to make it work.
type applyFromSimilarParams struct {
	Fields             []string `json:"fields"`
	TopK               int      `json:"top_k"`
	MinScore           float64  `json:"min_score"`
	ThresholdAutoapply float64  `json:"threshold_autoapply"`
	ThresholdPropose   float64  `json:"threshold_propose"`
	TagFrequencyMin    float64  `json:"tag_frequency_min"`
}

func (p *applyFromSimilarParams) withDefaults() {
	if len(p.Fields) == 0 {
		p.Fields = []string{"jd_category", "correspondent", "document_type", "tags"}
	}
	if p.TopK <= 0 {
		p.TopK = 10
	}
	// MinScore floors the accepted FTS matches. SQLite's BM25 returns
	// small magnitudes (often < 1) so the default is deliberately 0
	// — the FTS MATCH clause is already a strong filter, and an
	// operator raising this to something like 0.5 filters out weakly
	// overlapping docs without touching the SPA UI.
	if p.MinScore < 0 {
		p.MinScore = 0
	}
	if p.ThresholdAutoapply <= 0 {
		p.ThresholdAutoapply = 0.9
	}
	if p.ThresholdPropose <= 0 {
		p.ThresholdPropose = 0.5
	}
	if p.TagFrequencyMin <= 0 {
		p.TagFrequencyMin = 0.3
	}
}

func (p *applyFromSimilarParams) wants(field string) bool {
	for _, f := range p.Fields {
		if f == field {
			return true
		}
	}
	return false
}

// runApplyFromSimilar is the action handler. Called from apply.go's
// runAction switch inside the automations WriteTx.
func runApplyFromSimilar(ctx context.Context, tx *sql.Tx, d *db.DB, log *slog.Logger, docID int64, a Action) error {
	if heuristicsSkip && !isForcedHeuristics(ctx) {
		return nil
	}

	// Parse params (round-trip the map through JSON to hit the struct
	// tags). Missing fields fall back to defaults.
	var p applyFromSimilarParams
	if raw, err := json.Marshal(a.Params); err == nil {
		_ = json.Unmarshal(raw, &p)
	}
	p.withDefaults()

	// Load target-doc metadata + ownership. We need owner_id for the
	// visibility-scoped similar query — the automation runs "as the
	// document's owner", not the ingest producer.
	var (
		ownerID                                  int64
		jdCategoryID, correspondentID, docTypeID sql.NullInt64
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT owner_id, jd_category_id, correspondent_id, document_type_id
		  FROM documents WHERE id = ? AND trashed_at IS NULL
	`, docID).Scan(&ownerID, &jdCategoryID, &correspondentID, &docTypeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // doc trashed between enqueue and now — no-op
		}
		return fmt.Errorf("apply_from_similar: load doc: %w", err)
	}

	// Ignore already-present-inbox jd_category, since fresh uploads
	// land there at insert time. Treat "in inbox" as "field not yet
	// set" so heuristics can propose a real category. Inbox ID lives
	// under settings.jd_inbox_category_id (not a column on
	// jd_categories) — see core/jd/tree.go.
	if jdCategoryID.Valid {
		var inboxRaw sql.NullString
		_ = tx.QueryRowContext(ctx,
			`SELECT value_json FROM settings WHERE key = 'jd_inbox_category_id' LIMIT 1`).Scan(&inboxRaw)
		if inboxRaw.Valid {
			var inboxID int64
			_ = json.Unmarshal([]byte(inboxRaw.String), &inboxID)
			if inboxID != 0 && jdCategoryID.Int64 == inboxID {
				jdCategoryID = sql.NullInt64{}
			}
		}
	}

	// Existing tag ids so we skip proposing anything the doc already
	// carries.
	existingTags := map[int64]bool{}
	trows, err := tx.QueryContext(ctx,
		`SELECT tag_id FROM document_tags WHERE document_id = ?`, docID)
	if err != nil {
		return fmt.Errorf("apply_from_similar: load tags: %w", err)
	}
	for trows.Next() {
		var id int64
		if err := trows.Scan(&id); err != nil {
			trows.Close()
			return err
		}
		existingTags[id] = true
	}
	trows.Close()

	// Load owner's group memberships for the visibility splice.
	groups, err := authz.LoadGroups(ctx, d, ownerID)
	if err != nil {
		return fmt.Errorf("apply_from_similar: load groups: %w", err)
	}
	sp := &similar.Principal{
		UserID: ownerID,
		Role:   "user", // scope like the owner, not admin — heuristics must respect ACLs
		Groups: groups,
	}

	neighbours, err := similar.TopDocs(ctx, d, docID, p.TopK, sp)
	if err != nil {
		return fmt.Errorf("apply_from_similar: fetch neighbours: %w", err)
	}
	// Filter by minimum score floor.
	kept := neighbours[:0]
	for _, n := range neighbours {
		if n.Score >= p.MinScore {
			kept = append(kept, n)
		}
	}
	neighbours = kept
	if len(neighbours) < 3 {
		// Not enough signal. Silent no-op — no proposals row, no
		// slog line at info level. Operators auditing why heuristics
		// didn't propose can still find this branch by tracing docID.
		return nil
	}

	// Fetch each neighbour's core-four metadata and tag ids in one
	// pass. Placeholders live in a small slice we splice into the
	// SQL — no user input is stringified.
	ids := make([]int64, len(neighbours))
	scoreByID := make(map[int64]float64, len(neighbours))
	titleByID := make(map[int64]string, len(neighbours))
	for i, n := range neighbours {
		ids[i] = n.ID
		scoreByID[n.ID] = n.Score
		titleByID[n.ID] = n.Title
	}

	metadata, err := loadNeighbourMetadata(ctx, d, ids)
	if err != nil {
		return fmt.Errorf("apply_from_similar: load neighbour metadata: %w", err)
	}

	// Aggregate votes. `scalarTally` maps field → value → cumulative
	// score. `scalarLabels` caches the label the SPA card shows.
	scalarTally := map[string]map[int64]float64{
		"jd_category":   {},
		"correspondent": {},
		"document_type": {},
	}
	scalarSupporters := map[string]map[int64][]int64{
		"jd_category":   {},
		"correspondent": {},
		"document_type": {},
	}
	tagTally := map[int64]float64{}
	tagSupporters := map[int64][]int64{}
	var totalScore float64
	for _, id := range ids {
		md, ok := metadata[id]
		if !ok {
			continue
		}
		s := scoreByID[id]
		totalScore += s
		if md.JDCategoryID != 0 {
			scalarTally["jd_category"][md.JDCategoryID] += s
			scalarSupporters["jd_category"][md.JDCategoryID] = append(scalarSupporters["jd_category"][md.JDCategoryID], id)
		}
		if md.CorrespondentID != 0 {
			scalarTally["correspondent"][md.CorrespondentID] += s
			scalarSupporters["correspondent"][md.CorrespondentID] = append(scalarSupporters["correspondent"][md.CorrespondentID], id)
		}
		if md.DocumentTypeID != 0 {
			scalarTally["document_type"][md.DocumentTypeID] += s
			scalarSupporters["document_type"][md.DocumentTypeID] = append(scalarSupporters["document_type"][md.DocumentTypeID], id)
		}
		for _, tid := range md.TagIDs {
			tagTally[tid] += s
			tagSupporters[tid] = append(tagSupporters[tid], id)
		}
	}
	if totalScore == 0 {
		return nil
	}

	// Resolve, per requested field, into (winner, confidence,
	// supporters). Then bucket into auto-apply / propose / drop.
	skip := map[string]bool{}
	if jdCategoryID.Valid {
		skip["jd_category"] = true
	}
	if correspondentID.Valid {
		skip["correspondent"] = true
	}
	if docTypeID.Valid {
		skip["document_type"] = true
	}

	for _, field := range []string{"jd_category", "correspondent", "document_type"} {
		if !p.wants(field) || skip[field] {
			continue
		}
		winnerID, winnerScore := topScalar(scalarTally[field])
		if winnerID == 0 {
			continue
		}
		confidence := winnerScore / totalScore
		if confidence < p.ThresholdPropose {
			continue
		}
		supporters := scalarSupporters[field][winnerID]
		label := lookupLabel(ctx, tx, field, winnerID)
		payload := map[string]any{
			"id":         winnerID,
			"label":      label,
			"supporters": supporters,
		}
		if confidence >= p.ThresholdAutoapply {
			if err := applyScalar(ctx, tx, field, docID, winnerID); err != nil {
				log.Warn("apply_from_similar.autoapply.write", "field", field, "err", err.Error())
				continue
			}
			audit.LogInTx(ctx, tx, log, audit.Event{
				Actor:      nil, // system actor
				Action:     "heuristics.autoapply",
				ObjectKind: "document",
				ObjectID:   docID,
				After: map[string]any{
					"field":      field,
					"value_id":   winnerID,
					"label":      label,
					"confidence": confidence,
					"based_on":   supporters,
				},
			})
		} else {
			if err := insertProposal(ctx, tx, docID, field, winnerID, payload, confidence, supporters); err != nil {
				log.Warn("apply_from_similar.propose.write", "field", field, "err", err.Error())
			}
		}
	}

	// Tags: emit each candidate that meets the frequency floor.
	if p.wants("tags") {
		threshold := p.TagFrequencyMin * float64(len(neighbours))
		for tagID, weightedScore := range tagTally {
			supporters := tagSupporters[tagID]
			if float64(len(supporters)) < threshold {
				continue
			}
			if existingTags[tagID] {
				continue
			}
			confidence := weightedScore / totalScore
			if confidence < p.ThresholdPropose {
				continue
			}
			label := lookupLabel(ctx, tx, "tag", tagID)
			payload := map[string]any{
				"id":         tagID,
				"label":      label,
				"supporters": supporters,
			}
			if confidence >= p.ThresholdAutoapply {
				if _, err := tx.ExecContext(ctx,
					`INSERT OR IGNORE INTO document_tags(document_id, tag_id) VALUES (?, ?)`,
					docID, tagID); err != nil {
					log.Warn("apply_from_similar.autoapply.tag", "tag_id", tagID, "err", err.Error())
					continue
				}
				audit.LogInTx(ctx, tx, log, audit.Event{
					Actor:      nil,
					Action:     "heuristics.autoapply",
					ObjectKind: "document",
					ObjectID:   docID,
					After: map[string]any{
						"field":      "tag",
						"value_id":   tagID,
						"label":      label,
						"confidence": confidence,
						"based_on":   supporters,
					},
				})
			} else {
				if err := insertProposal(ctx, tx, docID, "tag", tagID, payload, confidence, supporters); err != nil {
					log.Warn("apply_from_similar.propose.tag", "tag_id", tagID, "err", err.Error())
				}
			}
		}
	}

	return nil
}

// neighbourMD is the metadata bundle for one similar doc.
type neighbourMD struct {
	JDCategoryID    int64
	CorrespondentID int64
	DocumentTypeID  int64
	TagIDs          []int64
}

func loadNeighbourMetadata(ctx context.Context, d *db.DB, ids []int64) (map[int64]neighbourMD, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out := make(map[int64]neighbourMD, len(ids))

	// Scalars in one query.
	placeholders, args := placeholderList(ids)
	q := "SELECT id, COALESCE(jd_category_id,0), COALESCE(correspondent_id,0), COALESCE(document_type_id,0) " +
		"FROM documents WHERE id IN (" + placeholders + ")"
	rows, err := d.Read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var md neighbourMD
		if err := rows.Scan(&id, &md.JDCategoryID, &md.CorrespondentID, &md.DocumentTypeID); err != nil {
			rows.Close()
			return nil, err
		}
		out[id] = md
	}
	rows.Close()

	// Tags in one query.
	tq := "SELECT document_id, tag_id FROM document_tags WHERE document_id IN (" + placeholders + ")"
	trows, err := d.Read.QueryContext(ctx, tq, args...)
	if err != nil {
		return out, err
	}
	defer trows.Close()
	for trows.Next() {
		var docID, tagID int64
		if err := trows.Scan(&docID, &tagID); err != nil {
			return out, err
		}
		md := out[docID]
		md.TagIDs = append(md.TagIDs, tagID)
		out[docID] = md
	}
	return out, trows.Err()
}

func placeholderList(ids []int64) (string, []any) {
	if len(ids) == 0 {
		return "", nil
	}
	args := make([]any, len(ids))
	buf := make([]byte, 0, len(ids)*2)
	for i, id := range ids {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, '?')
		args[i] = id
	}
	return string(buf), args
}

func topScalar(tally map[int64]float64) (int64, float64) {
	var (
		winnerID    int64
		winnerScore float64
	)
	for id, s := range tally {
		if s > winnerScore || (s == winnerScore && id < winnerID) {
			winnerID = id
			winnerScore = s
		}
	}
	return winnerID, winnerScore
}

func applyScalar(ctx context.Context, tx *sql.Tx, field string, docID, valueID int64) error {
	var col string
	switch field {
	case "jd_category":
		col = "jd_category_id"
	case "correspondent":
		col = "correspondent_id"
	case "document_type":
		col = "document_type_id"
	default:
		return fmt.Errorf("apply_from_similar: unknown field %q", field)
	}
	// Only update when the field is still empty — belt-and-braces
	// against a rules-classifier write that landed after our tally
	// read but before our write. UPDATE ... WHERE col IS NULL keeps
	// the action idempotent + race-tolerant.
	_, err := tx.ExecContext(ctx,
		"UPDATE documents SET "+col+" = ?, updated_at = ? WHERE id = ? AND "+col+" IS NULL",
		valueID, time.Now().Unix(), docID)
	return err
}

func insertProposal(ctx context.Context, tx *sql.Tx, docID int64, field string, valueID int64,
	payload map[string]any, confidence float64, basedOn []int64) error {
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	rawBasedOn, err := json.Marshal(basedOn)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO document_proposals(
			document_id, field, value_id, value_json,
			confidence, based_on, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, docID, field, valueID, string(rawPayload),
		confidence, string(rawBasedOn), time.Now().Unix())
	return err
}

func lookupLabel(ctx context.Context, tx *sql.Tx, field string, id int64) string {
	var table, col string
	switch field {
	case "jd_category":
		table, col = "jd_categories", "COALESCE(code || ' ' || name, '')"
	case "correspondent":
		table, col = "correspondents", "COALESCE(name, '')"
	case "document_type":
		table, col = "document_types", "COALESCE(name, '')"
	case "tag":
		table, col = "tags", "COALESCE(name, '')"
	default:
		return ""
	}
	var s string
	_ = tx.QueryRowContext(ctx,
		"SELECT "+col+" FROM "+table+" WHERE id = ?", id).Scan(&s)
	return s
}
