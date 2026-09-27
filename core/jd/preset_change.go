// SPDX-License-Identifier: AGPL-3.0-or-later

package jd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jd/importer"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
)

var ErrBlankPresetConfirmation = errors.New("blank filing tree requires explicit confirmation")

type PresetChangeRequest struct {
	PresetID          string
	SetIDs            []string
	Replacements      map[int]bool
	Remaps            map[int]int
	SkipSeeds         bool
	ConfirmBlank      bool
	ExpectedStateHash string
	SystemID          int64
	ActorID           int64
}

type PresetChange struct {
	importer.Diff
	FromPresetID  string   `json:"from_preset_id,omitempty"`
	CurrentSetIDs []string `json:"current_set_ids"`
	SetIDs        []string `json:"set_ids"`
}

func PreviewPresetChange(ctx context.Context, d *db.DB, request PresetChangeRequest) (*PresetChange, error) {
	return planPresetChange(ctx, d, nil, request, false)
}

func ApplyPresetChange(ctx context.Context, d *db.DB, log *slog.Logger, request PresetChangeRequest) (*PresetChange, error) {
	if len(request.SetIDs) == 0 && request.PresetID == "blank" && !request.ConfirmBlank {
		return nil, ErrBlankPresetConfirmation
	}
	return planPresetChange(ctx, d, log, request, true)
}

func planPresetChange(ctx context.Context, d *db.DB, log *slog.Logger, request PresetChangeRequest, apply bool) (*PresetChange, error) {
	if request.SystemID == 0 {
		request.SystemID = systems.DefaultID
	}
	current, err := systems.Get(ctx, d.Read, request.SystemID)
	if err != nil {
		return nil, err
	}
	target, setIDs, contentHash, err := presetChangeTarget(request)
	if err != nil {
		return nil, err
	}
	if apply && len(setIDs) == 0 && !request.ConfirmBlank {
		return nil, ErrBlankPresetConfirmation
	}
	currentSetIDs := currentPresetSetIDs(current)
	replacements := automaticReplacements(currentSetIDs, setIDs)
	rows, err := d.Read.QueryContext(ctx, `SELECT code FROM jd_categories WHERE system_id=? AND system=0`, request.SystemID)
	if err != nil {
		return nil, err
	}
	existingCodes := map[int]bool{}
	for rows.Next() {
		var code int
		if err := rows.Scan(&code); err != nil {
			rows.Close()
			return nil, err
		}
		existingCodes[code] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	incomingCodes := map[int]bool{}
	for _, area := range target.Areas {
		if area.Code == 40 {
			continue
		}
		for _, category := range area.Categories {
			incomingCodes[category.Code] = true
		}
	}
	for code := range replacements {
		if !existingCodes[code] || !incomingCodes[code] {
			delete(replacements, code)
		}
	}
	for code, replace := range request.Replacements {
		if replace {
			replacements[code] = true
		}
	}
	opts := importer.Options{
		TargetSystem: current.Code, ActorID: request.ActorID, SkipSeeds: request.SkipSeeds,
		Remaps: request.Remaps, Replacements: replacements, SwitchFromPreset: current.PresetID,
		SetIDs: setIDs, ContentSHA256: contentHash, ExpectedStateHash: request.ExpectedStateHash,
	}
	var diff *importer.Diff
	if apply {
		diff, err = importer.Apply(ctx, d, log, target, opts)
	} else {
		diff, err = importer.Preview(ctx, d, target, opts)
	}
	if err != nil {
		return nil, err
	}
	suggested := SuggestedReplacementCodes(currentSetIDs, setIDs)
	for i := range diff.Collisions {
		collision := &diff.Collisions[i]
		collision.SuggestedReplace = suggested[collision.Code] && !collision.Replace
		if err := d.Read.QueryRowContext(ctx, `
			SELECT COUNT(CASE WHEN d.id IS NOT NULL AND d.trashed_at IS NULL THEN 1 END),
			       COUNT(CASE WHEN d.id IS NOT NULL AND d.trashed_at IS NOT NULL THEN 1 END)
			FROM jd_categories c LEFT JOIN documents d ON d.jd_category_id=c.id
			WHERE c.system_id=? AND c.code=?`, request.SystemID, collision.Code).
			Scan(&collision.LiveDocuments, &collision.TrashedDocuments); err != nil {
			return nil, err
		}
	}
	return &PresetChange{Diff: *diff, FromPresetID: current.PresetID, CurrentSetIDs: currentSetIDs, SetIDs: setIDs}, nil
}

func presetChangeTarget(request PresetChangeRequest) (*presetfile.PresetFile, []string, string, error) {
	if request.PresetID != "" && request.SetIDs != nil {
		return nil, nil, "", errors.New("choose either a preset or filing sets")
	}
	var (
		pf     *presetfile.PresetFile
		setIDs []string
		err    error
	)
	switch {
	case request.PresetID != "":
		if _, ok := PresetByID(request.PresetID); !ok {
			return nil, nil, "", fmt.Errorf("unknown preset %q", request.PresetID)
		}
		pf, err = loadPresetFile(request.PresetID)
		setIDs = PresetSetIDs(request.PresetID)
	case request.SetIDs != nil:
		setIDs, err = normalizeSetIDs(request.SetIDs)
		if err == nil {
			pf, err = ComposePreset(setIDs)
		}
	default:
		return nil, nil, "", errors.New("preset_id or set_ids is required")
	}
	if err != nil {
		return nil, nil, "", err
	}
	content, err := json.Marshal(pf)
	if err != nil {
		return nil, nil, "", err
	}
	hash := sha256.Sum256(content)
	return pf, setIDs, hex.EncodeToString(hash[:]), nil
}

func currentPresetSetIDs(system systems.System) []string {
	if ids, exists := presetSetRecipes[system.PresetID]; exists {
		return append([]string(nil), ids...)
	}
	var authoring struct {
		SetIDs []string `json:"set_ids"`
	}
	if json.Unmarshal([]byte(system.AuthoringJSON), &authoring) != nil {
		return []string{}
	}
	ids, err := normalizeSetIDs(authoring.SetIDs)
	if err != nil {
		return []string{}
	}
	return ids
}

func automaticReplacements(fromSetIDs, toSetIDs []string) map[int]bool {
	from := SemanticCategoryKeys(fromSetIDs)
	to := SemanticCategoryKeys(toSetIDs)
	replacements := map[int]bool{}
	for code, key := range to {
		if from[code] == key {
			replacements[code] = true
		}
	}
	return replacements
}
