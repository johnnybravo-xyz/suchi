package systems

import "context"

// IDs lists every filing system in stable order for trusted server operations.
func IDs(ctx context.Context, q Queryer) ([]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM jd_systems ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
