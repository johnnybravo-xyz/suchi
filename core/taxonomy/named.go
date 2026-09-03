package taxonomy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/slug"
)

// NamedTable identifies a reference table with the shared name/slug columns.
type NamedTable uint8

const (
	TableTags NamedTable = iota + 1
	TableCorrespondents
	TableDocumentTypes
)

func (t NamedTable) sqlName() (string, error) {
	switch t {
	case TableTags:
		return "tags", nil
	case TableCorrespondents:
		return "correspondents", nil
	case TableDocumentTypes:
		return "document_types", nil
	default:
		return "", fmt.Errorf("taxonomy: invalid named table %d", t)
	}
}

// UpsertByName returns the id of name, creating the reference row when needed.
// An existing exact name takes precedence; otherwise a row with the same
// generated slug is reused without replacing its canonical name or slug.
func UpsertByName(ctx context.Context, tx *sql.Tx, table NamedTable, name string, now int64) (int64, error) {
	tableName, err := table.sqlName()
	if err != nil {
		return 0, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("taxonomy: empty name")
	}
	canonicalSlug := slug.Make(name)
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s(name, slug, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT DO NOTHING
	`, tableName), name, canonicalSlug, now, now); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx,
		fmt.Sprintf(`
			SELECT id
			FROM %s
			WHERE name = ? OR slug = ?
			ORDER BY CASE WHEN name = ? THEN 0 ELSE 1 END, id
			LIMIT 1
		`, tableName), name, canonicalSlug, name,
	).Scan(&id); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET updated_at = ? WHERE id = ?`, tableName),
		now, id,
	); err != nil {
		return 0, err
	}
	return id, nil
}
