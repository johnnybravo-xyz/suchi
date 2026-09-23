// SPDX-License-Identifier: AGPL-3.0-or-later

package importer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
)

// destination is read-only planning state. New systems have no allocated ID
// until Apply holds the writer and has verified the complete preview binding.
type destination struct {
	Target             systems.System
	OriginalCode       string
	ExistingSystemCode string
	Create             bool
	Introduce          bool
	Members            []int64
}

func resolveDestination(ctx context.Context, tx *sql.Tx, pf *presetfile.PresetFile, opts Options) (destination, error) {
	original, err := systems.Get(ctx, tx, systems.DefaultID)
	if err != nil {
		return destination{}, err
	}
	out := destination{OriginalCode: original.Code, ExistingSystemCode: opts.ExistingSystemCode, Members: []int64{}}
	code := opts.TargetSystem
	if pf.System != "" {
		if code != "" && code != pf.System {
			return out, fmt.Errorf("target_system conflicts with file system")
		}
		code = pf.System
	}
	if code != "" && !systems.ValidCode(code) {
		return out, fmt.Errorf("target_system: invalid system code")
	}
	if opts.ExistingSystemCode != "" && (!systems.ValidCode(opts.ExistingSystemCode) || original.Code != "" || pf.System == "" || opts.ExistingSystemCode == code) {
		return out, fmt.Errorf("existing_system_code requires a distinct valid code on the first prefixed import")
	}
	if original.Code == "" {
		if pf.System == "" {
			if code != "" {
				return out, fmt.Errorf("only a prefixed taxonomy file can introduce a system")
			}
			out.Target = original
		} else {
			out.Introduce = true
			if opts.ExistingSystemCode == "" {
				out.Target = original
				out.Target.Code, out.Target.Name = code, pf.Name
			} else {
				out.Create = true
				out.Target = systems.System{Code: code, Name: pf.Name, Taxonomy: "jd"}
			}
		}
	} else if code == "" {
		out.Target = original
	} else {
		out.Target, err = systems.ByCode(ctx, tx, code)
		if errors.Is(err, sql.ErrNoRows) && pf.System != "" {
			out.Create = true
			out.Target = systems.System{Code: code, Name: pf.Name, Taxonomy: "jd"}
		} else if err != nil {
			return out, err
		}
	}
	if out.Introduce {
		// Even an unexpected pre-introduction conflicting row must fail in
		// Preview, rather than reaching a uniqueness error halfway through Apply.
		for _, candidate := range []string{code, opts.ExistingSystemCode} {
			if candidate == "" {
				continue
			}
			_, err := systems.ByCode(ctx, tx, candidate)
			if err == nil {
				return out, fmt.Errorf("system code already exists")
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return out, err
			}
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT user_id FROM jd_system_members WHERE system_id=? ORDER BY user_id`, out.Target.ID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return out, err
		}
		out.Members = append(out.Members, id)
	}
	return out, rows.Err()
}

func applyDestination(ctx context.Context, tx *sql.Tx, p *importPlan) error {
	now := time.Now().Unix()
	d := &p.destination
	if d.Introduce {
		code := d.Target.Code
		if d.Create {
			code = d.ExistingSystemCode
		}
		if err := systems.SetCode(ctx, tx, systems.DefaultID, code, now); err != nil {
			return err
		}
		if !d.Create {
			if err := systems.Rename(ctx, tx, systems.DefaultID, d.Target.Name, now); err != nil {
				return err
			}
		}
	}
	if d.Create {
		id, err := systems.Create(ctx, tx, d.Target.Code, d.Target.Name, d.Target.Taxonomy, now)
		if err != nil {
			return err
		}
		d.Target.ID = id
	}
	return nil
}
