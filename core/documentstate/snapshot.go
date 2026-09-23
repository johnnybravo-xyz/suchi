// SPDX-License-Identifier: AGPL-3.0-or-later

// Package documentstate binds inferred proposals to durable source and field generations.
package documentstate

import (
	"context"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
)

type Snapshot struct {
	SystemID              int64  `json:"system_id"`
	OwnerID               int64  `json:"owner_id"`
	SourceRevision        int64  `json:"source_revision"`
	TitleRevision         int64  `json:"title_revision"`
	CorrespondentRevision int64  `json:"correspondent_revision"`
	DocumentTypeRevision  int64  `json:"document_type_revision"`
	CategoryRevision      int64  `json:"category_revision"`
	TagsRevision          int64  `json:"tags_revision"`
	LanguageRevision      int64  `json:"language_revision"`
	SourceBlob            string `json:"source_blob"`
}

type Reference struct {
	DocumentID int64    `json:"document_id"`
	Snapshot   Snapshot `json:"snapshot"`
}

func Load(ctx context.Context, q systems.Queryer, docID int64) (Snapshot, error) {
	var s Snapshot
	err := q.QueryRowContext(ctx, `SELECT system_id, owner_id, source_revision, title_revision,
 correspondent_revision, document_type_revision, category_revision, tags_revision,
 language_revision, original_blob FROM documents WHERE id = ? AND trashed_at IS NULL`, docID).Scan(
		&s.SystemID, &s.OwnerID, &s.SourceRevision, &s.TitleRevision, &s.CorrespondentRevision,
		&s.DocumentTypeRevision, &s.CategoryRevision, &s.TagsRevision, &s.LanguageRevision, &s.SourceBlob)
	return s, err
}

func (s Snapshot) FieldRevision(field string) int64 {
	switch field {
	case "title":
		return s.TitleRevision
	case "correspondent":
		return s.CorrespondentRevision
	case "document_type":
		return s.DocumentTypeRevision
	case "jd_category":
		return s.CategoryRevision
	case "tag":
		return s.TagsRevision
	case "language":
		return s.LanguageRevision
	default:
		return -1
	}
}

func (s Snapshot) SameSource(other Snapshot) bool {
	return s.SystemID == other.SystemID && s.OwnerID == other.OwnerID && s.SourceBlob == other.SourceBlob && s.SourceRevision == other.SourceRevision
}
