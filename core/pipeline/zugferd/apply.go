// Apply glue: take a parsed Invoice + a doc id and write the
// custom-field values. Kept in this package (rather than post-ingest)
// so post-ingest stays thin and the ZUGFeRD shape stays owned in one
// place.

package zugferd

import (
	"context"
	"database/sql"
	"time"

	"github.com/suchi-dms/suchi/core/db"
)

// InvoiceFieldNames are the custom-field keys we own. Kept as a
// package constant so tests + operator docs share one source of truth.
var InvoiceFieldNames = []struct {
	Name     string
	DataType string
}{
	{"invoice_number", "text"},
	{"invoice_date", "date"},
	{"invoice_currency", "text"},
	{"invoice_total_gross", "monetary"},
	{"invoice_total_net", "monetary"},
	{"invoice_seller", "text"},
	{"invoice_seller_vat", "text"},
	{"invoice_buyer", "text"},
}

// Apply writes the invoice into document_custom_field_values,
// creating the custom_fields rows if this is the first invoice on the
// instance. Idempotent: repeated calls on the same doc overwrite the
// existing values (ON CONFLICT DO UPDATE via a DELETE-then-INSERT
// pattern within the tx).
func Apply(ctx context.Context, d *db.DB, docID int64, inv *Invoice) error {
	if inv == nil {
		return nil
	}
	return d.WriteTx(ctx, func(tx *sql.Tx) error {
		fieldIDs, err := ensureFields(ctx, tx)
		if err != nil {
			return err
		}
		vals := map[string]any{
			"invoice_number":      inv.Number,
			"invoice_date":        inv.IssueDate, // stored via issue-date parser below
			"invoice_currency":    inv.Currency,
			"invoice_total_gross": inv.TotalGross,
			"invoice_total_net":   inv.TotalNet,
			"invoice_seller":      inv.SellerName,
			"invoice_seller_vat":  inv.SellerVAT,
			"invoice_buyer":       inv.BuyerName,
		}
		for name, v := range vals {
			fid := fieldIDs[name]
			if fid == 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM document_custom_field_values
				 WHERE document_id = ? AND field_id = ?`,
				docID, fid); err != nil {
				return err
			}
			if err := writeVal(ctx, tx, docID, fid, name, v); err != nil {
				return err
			}
		}
		return nil
	})
}

// ensureFields creates any missing invoice_* rows in custom_fields and
// returns the name→id lookup for all of them.
func ensureFields(ctx context.Context, tx *sql.Tx) (map[string]int64, error) {
	out := make(map[string]int64, len(InvoiceFieldNames))
	now := time.Now().Unix()
	for _, f := range InvoiceFieldNames {
		var id int64
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM custom_fields WHERE name = ?`, f.Name).Scan(&id)
		if err == sql.ErrNoRows {
			res, err := tx.ExecContext(ctx, `
				INSERT INTO custom_fields(name, data_type, created_at, updated_at)
				VALUES (?, ?, ?, ?)
			`, f.Name, f.DataType, now, now)
			if err != nil {
				return nil, err
			}
			id, err = res.LastInsertId()
			if err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		}
		out[f.Name] = id
	}
	return out, nil
}

// writeVal routes v into the right typed column. Skips empties so we
// don't clutter the values table with placeholder rows.
func writeVal(ctx context.Context, tx *sql.Tx, docID, fieldID int64, name string, v any) error {
	switch x := v.(type) {
	case string:
		if x == "" {
			return nil
		}
		if name == "invoice_date" {
			ts := parseISODate(x)
			if ts == 0 {
				return nil
			}
			_, err := tx.ExecContext(ctx, `
				INSERT INTO document_custom_field_values(document_id, field_id, value_date)
				VALUES (?, ?, ?)
			`, docID, fieldID, ts)
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO document_custom_field_values(document_id, field_id, value_text)
			VALUES (?, ?, ?)
		`, docID, fieldID, x)
		return err
	case float64:
		if x == 0 {
			return nil
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO document_custom_field_values(document_id, field_id, value_number)
			VALUES (?, ?, ?)
		`, docID, fieldID, x)
		return err
	}
	return nil
}

// parseISODate turns "YYYY-MM-DD" into a unix epoch (UTC midnight).
// Returns 0 on any parse failure so writeVal can skip.
func parseISODate(s string) int64 {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return 0
	}
	return t.UTC().Unix()
}
