package jd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/johnnybravo-xyz/suchi/core/db"
)

// Preset is one entry in the setup-wizard's JD picker. Blank presets
// carry an explicit opt-in flag so the wizard can guard them.
type Preset struct {
	ID          string
	Label       string
	Description string
	Tree        Tree
	Blank       bool
}

// ErrDocumentsExist means the caller tried to swap in a fresh tree
// while documents were still filed under non-inbox categories. Move
// the docs first (or trash them) before applying.
var ErrDocumentsExist = errors.New("jd: cannot replace tree while documents are filed under non-inbox categories")

// Presets returns the wizard's preset catalog in display order. Blank
// sits last on purpose.
func Presets() []Preset {
	return []Preset{
		presetSolo(),
		presetHousehold(),
		presetSMBBilling(),
		presetFreelance(),
		presetBlank(),
	}
}

// PresetByID looks up a preset by ID; second return is false when
// unknown.
func PresetByID(id string) (Preset, bool) {
	for _, p := range Presets() {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// ApplyPreset replaces the current JD tree with the preset's. Refuses
// when any doc is filed under a non-inbox category — the caller must
// move those first. Wrapped in one write tx so a partial failure
// leaves the previous tree intact.
func ApplyPreset(ctx context.Context, d *db.DB, log *slog.Logger, id string) error {
	p, ok := PresetByID(id)
	if !ok {
		return fmt.Errorf("unknown preset %q", id)
	}
	log = log.With("component", "jd.preset", "preset", id)
	return d.WriteTx(ctx, func(tx *sql.Tx) error {
		var stray int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM documents d
			JOIN jd_categories c ON c.id = d.jd_category_id
			WHERE d.trashed_at IS NULL AND c.system = 0
		`).Scan(&stray); err != nil {
			return fmt.Errorf("count non-inbox docs: %w", err)
		}
		if stray > 0 {
			return fmt.Errorf("%w: %d document(s) filed", ErrDocumentsExist, stray)
		}

		// Park inbox-only docs on the existing system category so the
		// FK stays intact while we swap tables.
		if _, err := tx.ExecContext(ctx, `
			UPDATE documents SET jd_category_id = (
				SELECT id FROM jd_categories WHERE system = 1 LIMIT 1
			) WHERE trashed_at IS NULL
		`); err != nil {
			return fmt.Errorf("park docs: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM jd_categories`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM jd_areas`); err != nil {
			return err
		}
		if err := seedInTx(ctx, tx, p.Tree, ModeJD); err != nil {
			return err
		}
		// Repoint every parked doc at the new inbox.
		var newInbox int64
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM jd_categories WHERE system = 1 LIMIT 1`).Scan(&newInbox); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE documents SET jd_category_id = ? WHERE trashed_at IS NULL
		`, newInbox); err != nil {
			return err
		}
		log.Info("jd.preset.applied", "areas", len(p.Tree.Areas))
		return nil
	})
}

// ---------- preset data ----------

func presetSolo() Preset {
	return Preset{
		ID:          "solo",
		Label:       "Personal filing",
		Description: "One person's admin: ID, taxes, health, receipts. Compact 4-area tree.",
		Tree: Tree{Areas: []Area{
			{Start: 10, End: 19, Name: "Life admin", Categories: []Category{
				{Code: 11, Name: "Identity", Description: "ID, passport, driver's license"},
				{Code: 12, Name: "Housing", Description: "Rent, mortgage, utilities"},
				{Code: 13, Name: "Insurance"},
				{Code: 14, Name: "Vehicles"},
			}},
			{Start: 20, End: 29, Name: "Money", Categories: []Category{
				{Code: 21, Name: "Banking"},
				{Code: 22, Name: "Investments"},
				{Code: 23, Name: "Taxes"},
				{Code: 24, Name: "Receipts"},
			}},
			{Start: 30, End: 39, Name: "Health", Categories: []Category{
				{Code: 31, Name: "Medical records"},
				{Code: 32, Name: "Prescriptions"},
				{Code: 33, Name: "Bills & claims"},
			}},
			{Start: 40, End: 49, Name: "System", Categories: []Category{
				{Code: 49, Name: "Inbox", System: true},
			}},
		}},
	}
}

func presetHousehold() Preset {
	return Preset{
		ID:          "household",
		Label:       "Household",
		Description: "Two adults, kids, shared finances. Adds a family area for school, activities, pets.",
		Tree: Tree{Areas: []Area{
			{Start: 10, End: 19, Name: "Household admin", Categories: []Category{
				{Code: 11, Name: "Family IDs"},
				{Code: 12, Name: "Home"},
				{Code: 13, Name: "Utilities"},
				{Code: 14, Name: "Insurance"},
			}},
			{Start: 20, End: 29, Name: "Finance", Categories: []Category{
				{Code: 21, Name: "Bank accounts"},
				{Code: 22, Name: "Investments"},
				{Code: 23, Name: "Taxes"},
				{Code: 24, Name: "Loans & credit"},
				{Code: 25, Name: "Receipts"},
			}},
			{Start: 30, End: 39, Name: "Health & medical", Categories: []Category{
				{Code: 31, Name: "Records"},
				{Code: 32, Name: "Bills & claims"},
				{Code: 33, Name: "Prescriptions"},
			}},
			{Start: 40, End: 49, Name: "System", Categories: []Category{
				{Code: 49, Name: "Inbox", System: true},
			}},
			{Start: 50, End: 59, Name: "Family life", Categories: []Category{
				{Code: 51, Name: "School & activities"},
				{Code: 52, Name: "Pets"},
				{Code: 53, Name: "Travel"},
			}},
		}},
	}
}

func presetSMBBilling() Preset {
	return Preset{
		ID:          "smb_billing",
		Label:       "Small-business billing",
		Description: "Shop / factory / SME admin: customer invoices, vendor bills, payroll, statutory compliance.",
		Tree: Tree{Areas: []Area{
			{Start: 10, End: 19, Name: "Customers", Categories: []Category{
				{Code: 11, Name: "Customer records"},
				{Code: 12, Name: "Invoices out"},
				{Code: 13, Name: "Delivery notes"},
				{Code: 14, Name: "Receivables"},
			}},
			{Start: 20, End: 29, Name: "Vendors", Categories: []Category{
				{Code: 21, Name: "Vendor records"},
				{Code: 22, Name: "Bills in"},
				{Code: 23, Name: "Purchase orders"},
				{Code: 24, Name: "Payments"},
			}},
			{Start: 30, End: 39, Name: "Payroll & HR", Categories: []Category{
				{Code: 31, Name: "Employee records"},
				{Code: 32, Name: "Payslips"},
				{Code: 33, Name: "Contracts"},
			}},
			{Start: 40, End: 49, Name: "System", Categories: []Category{
				{Code: 49, Name: "Inbox", System: true},
			}},
			{Start: 50, End: 59, Name: "Compliance & tax", Categories: []Category{
				{Code: 51, Name: "Tax filings"},
				{Code: 52, Name: "Statutory returns"},
				{Code: 53, Name: "Audits"},
				{Code: 54, Name: "Licenses & permits"},
			}},
		}},
	}
}

func presetFreelance() Preset {
	return Preset{
		ID:          "freelance",
		Label:       "Freelance / consultant",
		Description: "Client-driven project work: per-client folders, project files, invoices out, self-employment taxes.",
		Tree: Tree{Areas: []Area{
			{Start: 10, End: 19, Name: "Clients", Categories: []Category{
				{Code: 11, Name: "Client records"},
				{Code: 12, Name: "Contracts & NDAs"},
				{Code: 13, Name: "Invoices out"},
				{Code: 14, Name: "Statements of work"},
			}},
			{Start: 20, End: 29, Name: "Projects", Categories: []Category{
				{Code: 21, Name: "Active"},
				{Code: 22, Name: "Delivered"},
				{Code: 23, Name: "Assets & deliverables"},
			}},
			{Start: 30, End: 39, Name: "Finance", Categories: []Category{
				{Code: 31, Name: "Bank & receivables"},
				{Code: 32, Name: "Expenses"},
				{Code: 33, Name: "Self-employment tax"},
			}},
			{Start: 40, End: 49, Name: "System", Categories: []Category{
				{Code: 49, Name: "Inbox", System: true},
			}},
			{Start: 50, End: 59, Name: "Portfolio & marketing", Categories: []Category{
				{Code: 51, Name: "Case studies"},
				{Code: 52, Name: "Testimonials"},
				{Code: 53, Name: "Templates"},
			}},
		}},
	}
}

func presetBlank() Preset {
	return Preset{
		ID:          "blank",
		Label:       "Blank slate",
		Description: "No categories beyond Inbox. Harder to migrate away from — opt-in only.",
		Blank:       true,
		Tree:        FlatTree,
	}
}
