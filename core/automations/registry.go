// SPDX-License-Identifier: AGPL-3.0-or-later

package automations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ActionTarget is resolved from the live document inside the caller's writer.
// It is not a principal or a grant of access to other documents.
type ActionTarget struct {
	DocID    int64
	SystemID int64
}

// ActionDefinition pairs configuration validation and transactional execution.
// Callbacks are trusted compiled code and must be concurrency-safe. Validate
// must only read; Execute must use tx for writes/enqueue and must not commit it,
// re-enter the writer, or perform network/subprocess work. Both must respect the
// target system and existing authorization boundaries.
type ActionDefinition struct {
	Kind         string
	Validate     func(context.Context, *sql.Tx, int64, map[string]any) error
	Execute      func(context.Context, *sql.Tx, ActionTarget, map[string]any) error
	allowTrashed bool // Only the built-in idempotent discard can revisit Trash.
}

// Registry owns a fixed action vocabulary. NewRegistry copies definitions;
// there is no registration mutation after construction and no global default.
type Registry struct {
	actions map[string]ActionDefinition
}

func NewRegistry(definitions []ActionDefinition) (*Registry, error) {
	r := &Registry{actions: make(map[string]ActionDefinition, len(definitions))}
	for _, definition := range definitions {
		if definition.Kind == "" || strings.TrimSpace(definition.Kind) != definition.Kind {
			return nil, fmt.Errorf("automations: invalid action kind %q", definition.Kind)
		}
		if _, exists := r.actions[definition.Kind]; exists {
			return nil, fmt.Errorf("automations: duplicate action kind %q", definition.Kind)
		}
		if definition.Validate == nil || definition.Execute == nil {
			return nil, fmt.Errorf("automations: action %q requires validation and execution", definition.Kind)
		}
		r.actions[definition.Kind] = definition
	}
	return r, nil
}

func (r *Registry) lookup(kind string) (ActionDefinition, error) {
	if kind == "" {
		return ActionDefinition{}, errors.New("kind required")
	}
	if r == nil {
		return ActionDefinition{}, errors.New("automations: action registry required")
	}
	definition, ok := r.actions[kind]
	if !ok {
		return ActionDefinition{}, fmt.Errorf("unsupported kind %q", kind)
	}
	return definition, nil
}

func (r *Registry) validate(ctx context.Context, tx *sql.Tx, systemID int64, action Action) error {
	definition, err := r.lookup(action.Kind)
	if err != nil {
		return err
	}
	return definition.Validate(ctx, tx, systemID, action.Params)
}

func (r *Registry) execute(ctx context.Context, tx *sql.Tx, docID int64, action Action) error {
	definition, err := r.lookup(action.Kind)
	if err != nil {
		return err
	}
	target := ActionTarget{DocID: docID}
	if err := tx.QueryRowContext(ctx, `SELECT system_id FROM documents WHERE id = ? AND (trashed_at IS NULL OR ?)`, docID, definition.allowTrashed).Scan(&target.SystemID); err != nil {
		return err
	}
	if err := definition.Validate(ctx, tx, target.SystemID, action.Params); err != nil {
		return err
	}
	return definition.Execute(ctx, tx, target, action.Params)
}
