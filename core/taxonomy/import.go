package taxonomy

import (
	"context"

	"github.com/johnnybravo-xyz/suchi/core/db"
)

// ImportMode selects replacement only while no filed documents exist.
func ImportMode(ctx context.Context, d *db.DB) (string, error) {
	var filed int
	err := d.Read.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM documents d
		JOIN jd_categories c ON c.id = d.jd_category_id
		WHERE d.trashed_at IS NULL AND c.system = 0
	`).Scan(&filed)
	if err != nil {
		return "", err
	}
	if filed == 0 {
		return "replace", nil
	}
	return "merge", nil
}
