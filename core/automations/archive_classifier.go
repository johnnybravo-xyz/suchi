// Archive classification applies high-confidence filing metadata from similar
// documents when enabled by policy, retaining other candidates for review.
package automations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/documentstate"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	"github.com/johnnybravo-xyz/suchi/core/settings"
	"github.com/johnnybravo-xyz/suchi/core/similar"
)

// ApplyFromArchive runs before user automations and the optional LLM. Current
// policy and bound evidence determine whether metadata applies or needs review.
func ApplyFromArchive(ctx context.Context, d *db.DB, log *slog.Logger, docID int64) error {
	if !settings.ResolveArchiveClassifierConfig(ctx, d).Enabled {
		return nil
	}
	input, err := readArchiveInput(ctx, d, docID)
	if err != nil || input == nil {
		return err
	}
	// Retrieval and snapshot capture are complete before opening the writer.
	log.Info("archive_classifier.considered", "doc_id", docID, "neighbours", len(input.neighbours))
	if len(input.neighbours) < 3 {
		return nil
	}
	return d.WriteTx(ctx, func(tx *sql.Tx) error {
		cfg := settings.ResolveArchiveClassifierConfig(ctx, d)
		if !cfg.Enabled {
			return nil
		}
		return proposeArchiveInput(ctx, tx, log, docID, input, cfg)
	})
}

type archiveInput struct {
	baseline   documentstate.Snapshot
	inboxID    int64
	target     neighbourMD
	neighbours []similar.Doc
	metadata   map[int64]neighbourMD
	snapshots  map[int64]documentstate.Snapshot
}

// readArchiveInput holds one read snapshot, not the write transaction, across
// ranking, ownership/ACL checks and metadata capture. Otherwise a source change
// between ranking and snapshot capture could bless evidence we never ranked.
func readArchiveInput(ctx context.Context, d *db.DB, docID int64) (*archiveInput, error) {
	tx, err := d.Read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	baseline, err := documentstate.Load(ctx, tx, docID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	allowed, err := systems.CanEnter(ctx, tx, baseline.OwnerID, baseline.SystemID)
	if err != nil || !allowed {
		return nil, err
	}
	groups, err := authz.LoadGroupsInTx(ctx, tx, baseline.OwnerID)
	if err != nil {
		return nil, err
	}
	neighbours, err := similar.TopDocsInTx(ctx, tx, docID, 10, &similar.Principal{
		UserID: baseline.OwnerID, Role: "user", Groups: groups, SystemID: baseline.SystemID,
	})
	if err != nil {
		return nil, fmt.Errorf("archive classifier: fetch neighbours: %w", err)
	}
	input := &archiveInput{baseline: baseline, neighbours: neighbours}
	if len(neighbours) < 3 {
		return input, nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(inbox_category_id, 0) FROM jd_systems WHERE id = ?`, baseline.SystemID).Scan(&input.inboxID); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(neighbours)+1)
	ids = append(ids, docID)
	for _, n := range neighbours {
		ids = append(ids, n.ID)
	}
	metadata, err := loadNeighbourMetadata(ctx, tx, baseline.SystemID, ids)
	if err != nil {
		return nil, err
	}
	input.target = metadata[docID]
	delete(metadata, docID)
	input.metadata = metadata
	input.snapshots = make(map[int64]documentstate.Snapshot, len(neighbours))
	for _, n := range neighbours {
		snapshot, err := documentstate.Load(ctx, tx, n.ID)
		if err != nil {
			return nil, err
		}
		input.snapshots[n.ID] = snapshot
	}
	return input, nil
}

func proposeArchiveInput(ctx context.Context, tx *sql.Tx, log *slog.Logger, docID int64, input *archiveInput, cfg settings.ArchiveClassifierConfig) error {
	current, err := documentstate.Load(ctx, tx, docID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if current != input.baseline {
		return nil
	}
	allowed, err := systems.CanEnter(ctx, tx, current.OwnerID, current.SystemID)
	if err != nil || !allowed {
		return err
	}
	groups, err := authz.LoadGroupsInTx(ctx, tx, current.OwnerID)
	if err != nil {
		return err
	}
	principal := authz.Principal{UserID: current.OwnerID, Role: "user", Groups: groups, SystemID: current.SystemID}
	// Every ranked neighbour contributes to the denominator. Reject the whole
	// sample if any source, owner, metadata, membership or permission changed.
	for _, n := range input.neighbours {
		snapshot, err := documentstate.Load(ctx, tx, n.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if snapshot != input.snapshots[n.ID] {
			return nil
		}
		if err := (authz.ACLAuthorizer{}).CanInTx(ctx, tx, principal, authz.KindDocument, n.ID, authz.PermView); err != nil {
			var denied *authz.ErrDenied
			if errors.As(err, &denied) {
				return nil
			}
			return err
		}
	}

	scalarTally := map[string]map[int64]float64{"jd_category": {}, "correspondent": {}, "document_type": {}}
	scalarSupporters := map[string]map[int64][]int64{"jd_category": {}, "correspondent": {}, "document_type": {}}
	tagTally := map[int64]float64{}
	tagSupporters := map[int64][]int64{}
	var totalScore float64
	for _, n := range input.neighbours {
		md := input.metadata[n.ID]
		totalScore += n.Score
		for _, value := range []struct {
			field string
			id    int64
		}{
			{"jd_category", md.JDCategoryID}, {"correspondent", md.CorrespondentID}, {"document_type", md.DocumentTypeID},
		} {
			if value.id != 0 {
				scalarTally[value.field][value.id] += n.Score
				scalarSupporters[value.field][value.id] = append(scalarSupporters[value.field][value.id], n.ID)
			}
		}
		for _, tagID := range md.TagIDs {
			tagTally[tagID] += n.Score
			tagSupporters[tagID] = append(tagSupporters[tagID], n.ID)
		}
	}
	if totalScore == 0 {
		return nil
	}
	applied := 0
	var pending []approvals.DocumentChange
	// The immutable retrieval baseline above rejects human/source races. Only
	// our own successful writes may advance the baseline used for application.
	applicationBaseline := current
	applyOrPropose := func(field string, valueID int64, confidence float64, ids []int64) error {
		supporters := make([]documentstate.Reference, len(ids))
		for i, id := range ids {
			supporters[i] = documentstate.Reference{DocumentID: id, Snapshot: input.snapshots[id]}
		}
		change := approvals.DocumentChange{
			Field: field, ValueID: valueID, Confidence: confidence,
			Threshold: &cfg.AutoThreshold, Source: "archive",
			Baseline: &applicationBaseline, Supporters: supporters,
		}
		didApply, err := approvals.ApplyAutomaticDocumentChangeInTx(ctx, tx, log, docID, change)
		if err != nil {
			return fmt.Errorf("archive classifier: apply %s: %w", field, err)
		}
		if didApply {
			applied++
			applicationBaseline, err = documentstate.Load(ctx, tx, docID)
			return err
		}
		change.Threshold = &cfg.ReviewThreshold
		pending = append(pending, change)
		return nil
	}
	for _, field := range []string{"jd_category", "correspondent", "document_type"} {
		if (field == "jd_category" && input.target.JDCategoryID != 0 && input.target.JDCategoryID != input.inboxID) ||
			(field == "correspondent" && input.target.CorrespondentID != 0) ||
			(field == "document_type" && input.target.DocumentTypeID != 0) {
			continue
		}
		winnerID, score := topScalar(scalarTally[field])
		if winnerID == 0 || (field == "jd_category" && winnerID == input.inboxID) {
			continue
		}
		confidence := score / totalScore
		if confidence < cfg.ReviewThreshold {
			continue
		}
		if err := applyOrPropose(field, winnerID, confidence, scalarSupporters[field][winnerID]); err != nil {
			return err
		}
	}
	for tagID, score := range tagTally {
		ids := tagSupporters[tagID]
		if float64(len(ids)) < 0.3*float64(len(input.neighbours)) || score/totalScore < cfg.ReviewThreshold {
			continue
		}
		// Include machine-owned tags here as well: inference must not take over
		// an existing marker. Neighbour evidence, unlike this existence check,
		// excludes machine-owned tags.
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM document_tags WHERE document_id = ? AND tag_id = ?)`, docID, tagID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			if err := applyOrPropose("tag", tagID, score/totalScore, ids); err != nil {
				return err
			}
		}
	}
	// Defer proposals until automatic writes finish so a later tag addition
	// cannot immediately invalidate an earlier review candidate from this batch.
	for _, change := range pending {
		if err := approvals.ProposeDocumentChangeInTx(ctx, tx, docID, change); err != nil {
			return fmt.Errorf("archive classifier: propose %s: %w", change.Field, err)
		}
	}
	log.Info("archive_classifier.wrote", "doc_id", docID, "applied", applied, "proposed", len(pending))
	return nil
}

// neighbourMD contains persisted metadata, not pending inference proposals.
type neighbourMD struct {
	JDCategoryID    int64
	CorrespondentID int64
	DocumentTypeID  int64
	TagIDs          []int64
}

func loadNeighbourMetadata(ctx context.Context, tx *sql.Tx, systemID int64, ids []int64) (map[int64]neighbourMD, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out := make(map[int64]neighbourMD, len(ids))
	placeholders, args := placeholderList(ids)
	args = append(args, systemID)
	rows, err := tx.QueryContext(ctx, `SELECT id, COALESCE(jd_category_id,0), COALESCE(correspondent_id,0), COALESCE(document_type_id,0)
		FROM documents WHERE id IN (`+placeholders+`) AND system_id = ? AND trashed_at IS NULL`, args...)
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
	trows, err := tx.QueryContext(ctx, `SELECT document_id, tag_id FROM document_tags
		WHERE document_id IN (`+placeholders+`) AND classifier_owned = 0
		AND EXISTS (SELECT 1 FROM documents WHERE id = document_id AND system_id = ? AND trashed_at IS NULL)`, args...)
	if err != nil {
		return nil, err
	}
	defer trows.Close()
	for trows.Next() {
		var docID, tagID int64
		if err := trows.Scan(&docID, &tagID); err != nil {
			return nil, err
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
	var winnerID int64
	var winnerScore float64
	for id, score := range tally {
		if score > winnerScore || (score == winnerScore && id < winnerID) {
			winnerID, winnerScore = id, score
		}
	}
	return winnerID, winnerScore
}
