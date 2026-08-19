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
func UpsertByName(ctx context.Context, tx *sql.Tx, table NamedTable, name string, now int64) (int64, error) {
	tableName, err := table.sqlName()
	if err != nil {
		return 0, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("taxonomy: empty name")
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s(name, slug, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET updated_at = excluded.updated_at
	`, tableName), name, slug.Make(name), now, now); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT id FROM %s WHERE name = ?`, tableName), name,
	).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}
