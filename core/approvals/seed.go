// Approval-def seeding — the "ships in the box" primitive for
// built-in approval flows. Same shape as automations.Seed, adapted
// for the approval_defs schema (which auto-versions by slug).
//
// Idempotency invariant: if an active def with matching spec_json
// already exists for the slug, EnsureDef is a no-op. If none
// exists, or the current active def's spec differs, a new version
// is registered (Register auto-deactivates the prior active row).
// Calling EnsureDef on every boot is safe.

package approvals

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

// EnsureDef is the seeder entrypoint. Callers pass a Spec and the
// slug they want it stored under; if the active def matches, this
// is a no-op. Otherwise a new version is registered.
//
// actor is optional — pass a synthesized "system" principal (with
// UserID=0) to attribute the seed to the process itself.
func (e *Engine) EnsureDef(ctx context.Context, systemID int64, slug string, spec Spec, actor *pluginapi.Principal) error {
	return e.db.WriteTx(ctx, func(tx *sql.Tx) error {
		return EnsureDefInTx(ctx, tx, systemID, slug, spec, actor)
	})
}

// EnsureDefInTx seeds a builtin in the caller's atomic system transaction.
func EnsureDefInTx(ctx context.Context, tx *sql.Tx, systemID int64, slug string, spec Spec, actor *pluginapi.Principal) error {
	currentPrincipal, err := currentActor(ctx, tx, actor, systemID)
	if err != nil {
		return err
	}
	if currentPrincipal != nil && currentPrincipal.UserID != 0 && currentPrincipal.Role != "admin" {
		return ErrForbidden
	}
	actor = currentPrincipal
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("approvals.ensure_def: invalid spec: %w", err)
	}
	current, err := activeDefBySlug(ctx, tx, systemID, slug)
	if err != nil && !errors.Is(err, ErrNoDef) {
		return err
	}
	raw, err := EncodeSpec(spec)
	if err != nil {
		return err
	}
	if specsEqual(current.SpecJSON, raw) {
		return nil
	}
	var actorID int64
	if actor != nil {
		actorID = actor.UserID
	}
	_, _, err = insertDef(ctx, tx, systemID, slug, raw, actorID)
	return err
}

// specsEqual compares two spec JSON blobs by round-tripping through
// map[string]any so key ordering + whitespace differences don't
// trigger a version bump. Cheap for the ~few-hundred-byte payloads
// we care about.
func specsEqual(a, b string) bool {
	var av, bv any
	if err := json.Unmarshal([]byte(a), &av); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &bv); err != nil {
		return false
	}
	an, _ := json.Marshal(av)
	bn, _ := json.Marshal(bv)
	return string(an) == string(bn)
}

// AdminAssigneeResolver expands `role:admin` into every admin's
// user id. Wire via `engine.SetAssigneeResolver`. Reuses the engine's
// DB handle — one query per Resolve call is fine at task-creation
// rates.
type AdminAssigneeResolver struct {
	Engine *Engine
	Log    *slog.Logger
}

// Resolve implements AssigneeResolver.
func (r AdminAssigneeResolver) Resolve(ctx context.Context, assignee string) ([]int64, error) {
	// Fall through to the built-in for non-role assignees so we
	// don't lose user:N handling when this resolver is wired.
	if assignee == "" {
		return nil, fmt.Errorf("%w: empty assignee", ErrBadAssignee)
	}
	if assignee == "role:admin" {
		rows, err := r.Engine.db.Read.QueryContext(ctx,
			`SELECT id FROM users WHERE role = 'admin' AND disabled = 0 ORDER BY id`)
		if err != nil {
			return nil, fmt.Errorf("role:admin resolve: %w", err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(out) == 0 {
			// No admins yet — the assignee is unresolvable. The runner
			// treats this as an error and the advance job retries, so
			// a fresh-instance first-boot before setup completes leaves
			// the task pending harmlessly.
			return nil, fmt.Errorf("%w: role:admin has zero users", ErrRoleUnresolved)
		}
		return out, nil
	}
	// Anything else — delegate to the built-in user-only resolver.
	return userOnlyResolver{}.Resolve(ctx, assignee)
}
