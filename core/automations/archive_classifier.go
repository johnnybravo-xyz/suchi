// Archive classification learns filing metadata from similar documents.
//
// Fetches the top-K similar existing documents (FTS5 more-like-this
// via core/similar), aggregates their core-four metadata
// (jd_category, correspondent, document_type, tags), applies
// confident winners, and sends weaker signals to the approvals inbox.

package automations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/settings"
	"github.com/johnnybravo-xyz/suchi/core/similar"
)

// ApplyFromArchive runs before user automations and the optional LLM. It reads
// settings on every call, so changes apply immediately without runtime wiring.
func ApplyFromArchive(ctx context.Context, d *db.DB, log *slog.Logger, docID int64) error {
	cfg := settings.ResolveArchiveClassifierConfig(ctx, d)
	if !cfg.Enabled {
		return nil
	}
	return d.WriteTx(ctx, func(tx *sql.Tx) error {
		return applyFromArchive(ctx, tx, d, log, docID, cfg)
	})
}

func applyFromArchive(ctx context.Context, tx *sql.Tx, d *db.DB, log *slog.Logger, docID int64, cfg settings.ArchiveClassifierConfig) error {
	// Load target-doc metadata + ownership. We need owner_id for the
	// visibility-scoped similar query — the automation runs "as the
	// document's owner", not the ingest producer.
	var (
		ownerID                                  int64
		inboxCategoryID                          int64
		jdCategoryID, correspondentID, docTypeID sql.NullInt64
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT owner_id, jd_category_id, correspondent_id, document_type_id
		  FROM documents WHERE id = ? AND trashed_at IS NULL
	`, docID).Scan(&ownerID, &jdCategoryID, &correspondentID, &docTypeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // doc trashed between enqueue and now — no-op
		}
		return fmt.Errorf("archive classifier: load doc: %w", err)
	}

	// Ignore already-present-inbox jd_category, since fresh uploads
	// land there at insert time. Treat "in inbox" as "field not yet
	// set" so heuristics can propose a real category. Inbox ID lives
	// under settings.jd_inbox_category_id (not a column on
	// jd_categories) — see core/jd/tree.go.
	if jdCategoryID.Valid {
		var inboxRaw sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT value_json FROM settings WHERE key = 'jd_inbox_category_id' LIMIT 1`).Scan(&inboxRaw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("archive classifier: load inbox category: %w", err)
		}
		if inboxRaw.Valid {
			if err := json.Unmarshal([]byte(inboxRaw.String), &inboxCategoryID); err != nil {
				return fmt.Errorf("archive classifier: decode inbox category: %w", err)
			}
			if inboxCategoryID != 0 && jdCategoryID.Int64 == inboxCategoryID {
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
		return fmt.Errorf("archive classifier: load tags: %w", err)
	}
	for trows.Next() {
		var id int64
		if err := trows.Scan(&id); err != nil {
			trows.Close()
			return err
		}
		existingTags[id] = true
	}
	if err := trows.Err(); err != nil {
		trows.Close()
		return fmt.Errorf("archive classifier: iterate tags: %w", err)
	}
	trows.Close()

	// Load owner's group memberships for the visibility splice.
	groups, err := authz.LoadGroups(ctx, d, ownerID)
	if err != nil {
		return fmt.Errorf("archive classifier: load groups: %w", err)
	}
	sp := &similar.Principal{
		UserID: ownerID,
		Role:   "user", // scope like the owner, not admin — heuristics must respect ACLs
		Groups: groups,
	}

	neighbours, err := similar.TopDocs(ctx, d, docID, 10, sp)
	if err != nil {
		return fmt.Errorf("archive classifier: fetch neighbours: %w", err)
	}
	// Filter by minimum score floor.
	kept := neighbours[:0]
	for _, n := range neighbours {
		if n.Score >= 0 {
			kept = append(kept, n)
		}
	}
	neighbours = kept
	log.Info("archive_classifier.considered",
		"doc_id", docID,
		"neighbours", len(neighbours),
		"min_score", 0)
	if len(neighbours) < 3 {
		// Not enough signal. The considered log above is the only trace
		// operators auditing "why didn't heuristics propose?" can grep for.
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
		return fmt.Errorf("archive classifier: load neighbour metadata: %w", err)
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

	autoapplied, proposed := 0, 0

	for _, field := range []string{"jd_category", "correspondent", "document_type"} {
		if skip[field] {
			continue
		}
		winnerID, winnerScore := topScalar(scalarTally[field])
		if winnerID == 0 {
			continue
		}
		if field == "jd_category" && winnerID == inboxCategoryID {
			continue
		}
		confidence := winnerScore / totalScore
		if confidence < cfg.ReviewThreshold {
			continue
		}
		supporters := scalarSupporters[field][winnerID]
		label, err := lookupLabel(ctx, tx, field, winnerID)
		if err != nil {
			return fmt.Errorf("archive classifier: label %s: %w", field, err)
		}
		if confidence >= cfg.AutoThreshold {
			changed, err := applyScalar(ctx, tx, field, docID, winnerID, inboxCategoryID)
			if err != nil {
				return fmt.Errorf("archive classifier: auto-apply %s: %w", field, err)
			}
			if !changed {
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
			autoapplied++
		} else {
			if err := approvals.ProposeDocumentChangeInTx(ctx, tx, docID, approvals.DocumentChange{
				Field: field, ValueID: winnerID, Label: label, Confidence: confidence,
				BasedOn: supporters, Source: "archive",
			}); err != nil {
				return fmt.Errorf("archive classifier: propose %s: %w", field, err)
			}
			proposed++
		}
	}

	// Tags: emit each candidate that meets the frequency floor.
	{
		threshold := 0.3 * float64(len(neighbours))
		for tagID, weightedScore := range tagTally {
			supporters := tagSupporters[tagID]
			if float64(len(supporters)) < threshold {
				continue
			}
			if existingTags[tagID] {
				continue
			}
			confidence := weightedScore / totalScore
			if confidence < cfg.ReviewThreshold {
				continue
			}
			label, err := lookupLabel(ctx, tx, "tag", tagID)
			if err != nil {
				return fmt.Errorf("archive classifier: label tag: %w", err)
			}
			if confidence >= cfg.AutoThreshold {
				if _, err := tx.ExecContext(ctx,
					`INSERT OR IGNORE INTO document_tags(document_id, tag_id) VALUES (?, ?)`,
					docID, tagID); err != nil {
					return fmt.Errorf("archive classifier: auto-apply tag %d: %w", tagID, err)
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
				autoapplied++
			} else {
				if err := approvals.ProposeDocumentChangeInTx(ctx, tx, docID, approvals.DocumentChange{
					Field: "tag", ValueID: tagID, Label: label, Confidence: confidence,
					BasedOn: supporters, Source: "archive",
				}); err != nil {
					return fmt.Errorf("archive classifier: propose tag %d: %w", tagID, err)
				}
				proposed++
			}
		}
	}

	log.Info("archive_classifier.wrote",
		"doc_id", docID,
		"autoapplied", autoapplied,
		"proposed", proposed)
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
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
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

func applyScalar(ctx context.Context, tx *sql.Tx, field string, docID, valueID, inboxCategoryID int64) (bool, error) {
	var (
		col       string
		condition string
		args      []any
	)
	switch field {
	case "jd_category":
		col = "jd_category_id"
		if inboxCategoryID != 0 {
			condition = col + " IS NULL OR " + col + " = ?"
			args = append(args, inboxCategoryID)
		}
	case "correspondent":
		col = "correspondent_id"
	case "document_type":
		col = "document_type_id"
	default:
		return false, fmt.Errorf("archive classifier: unknown field %q", field)
	}
	if condition == "" {
		condition = col + " IS NULL"
	}
	args = append([]any{valueID, time.Now().Unix(), docID}, args...)
	res, err := tx.ExecContext(ctx,
		"UPDATE documents SET "+col+" = ?, updated_at = ? WHERE id = ? AND ("+condition+")",
		args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func lookupLabel(ctx context.Context, tx *sql.Tx, field string, id int64) (string, error) {
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
		return "", fmt.Errorf("unknown field %q", field)
	}
	var s string
	err := tx.QueryRowContext(ctx,
		"SELECT "+col+" FROM "+table+" WHERE id = ?", id).Scan(&s)
	return s, err
}
