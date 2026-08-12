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
func (e *Engine) EnsureDef(ctx context.Context, slug string, spec Spec, actor *pluginapi.Principal) error {
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("approvals.ensure_def: invalid spec: %w", err)
	}
	current, err := activeDefBySlug(ctx, e.db.Read, slug)
	if err != nil && !errors.Is(err, ErrNoDef) {
		return fmt.Errorf("approvals.ensure_def: load current: %w", err)
	}
	newJSON, err := EncodeSpec(spec)
	if err != nil {
		return fmt.Errorf("approvals.ensure_def: encode: %w", err)
	}
	if err == nil && specsEqual(current.SpecJSON, newJSON) {
		// Active def already matches — nothing to do.
		return nil
	}
	if _, err := e.Register(ctx, spec, slug, actor); err != nil {
		return fmt.Errorf("approvals.ensure_def: register: %w", err)
	}
	if e.log != nil {
		e.log.Info("approvals.seed.registered", "slug", slug)
	}
	return nil
}

// EnsureDef is the top-level convenience. Uses the package-level
// engine — main.go's SetDefault must have run first.
func EnsureDef(ctx context.Context, slug string, spec Spec, actor *pluginapi.Principal) error {
	e := Default()
	if e == nil {
		return ErrEngineNotConfigured
	}
	return e.EnsureDef(ctx, slug, spec, actor)
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
			`SELECT id FROM users WHERE role = 'admin' ORDER BY id`)
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
