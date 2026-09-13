package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/jd/importer"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
	"github.com/johnnybravo-xyz/suchi/core/jd/systems"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy"
)

type TaxonomyImportReq struct {
	Content            string         `json:"content"`
	Format             string         `json:"format,omitempty"`
	Apply              bool           `json:"apply,omitempty"`
	SkipSeeds          bool           `json:"skip_seeds,omitempty"`
	Remaps             map[string]int `json:"remaps,omitempty"`
	ExpectedStateHash  string         `json:"expected_state_hash,omitempty"`
	TargetSystem       string         `json:"target_system,omitempty"`
	ExistingSystemCode string         `json:"existing_system_code,omitempty"`
}

// ImportTaxonomy retains the existing session-admin route and shared error
// vocabulary. The browser only renders the domain preview and its diagnostics.
func (s *Server) ImportTaxonomy(w http.ResponseWriter, r *http.Request) {
	actor := s.requireAdmin(w, r)
	if actor == nil {
		return
	}
	var req TaxonomyImportReq
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, 400, "bad_body", err.Error())
		return
	}
	format, ok := taxonomyAuthoringFormat(req.Format, presetfile.FormatHuML)
	if !ok {
		s.writeError(w, 400, "bad_format", "format must be huml or toml")
		return
	}
	pf, err := presetfile.Parse([]byte(req.Content), format)
	if err != nil {
		var issues presetfile.Errors
		if errors.As(err, &issues) {
			s.writeJSON(w, 400, map[string]any{"code": "parse", "error": err.Error(), "issues": issues})
		} else {
			s.writeError(w, 400, "parse", err.Error())
		}
		return
	}
	remaps, err := decodeRemaps(req.Remaps)
	if err != nil {
		s.writeError(w, 400, "bad_remaps", err.Error())
		return
	}
	target := req.TargetSystem
	if target != "" && pf.System != "" && target != pf.System {
		s.writeError(w, http.StatusBadRequest, "system_conflict", "file and explicit target system must agree")
		return
	}
	if pf.System != "" {
		target = pf.System
	} else if target == "" {
		systemID, ok := s.requireSystem(w, r, actor)
		if !ok {
			return
		}
		system, err := systems.Get(r.Context(), s.DB.Read, systemID)
		if err != nil {
			s.serverErr(w, "taxonomy.target", err)
			return
		}
		target = system.Code
	}
	opts := importer.Options{SkipSeeds: req.SkipSeeds, Remaps: remaps, ContentSHA256: sha256hex(req.Content), ExpectedStateHash: req.ExpectedStateHash,
		TargetSystem: target, ExistingSystemCode: req.ExistingSystemCode, ActorID: actor.UserID}
	var diff *importer.Diff
	if req.Apply {
		diff, err = importer.Apply(r.Context(), s.DB, s.Log, pf, opts)
	} else {
		diff, err = importer.Preview(r.Context(), s.DB, pf, opts)
	}
	if err != nil {
		var collision *importer.UnresolvedCollisionsError
		switch {
		case errors.Is(err, importer.ErrStalePreview):
			s.writeError(w, 409, "stale_preview", err.Error())
		case errors.As(err, &collision):
			s.writeJSON(w, 409, map[string]any{"code": "collisions", "error": err.Error(), "collisions": collision.Items})
		default:
			s.writeError(w, 400, "taxonomy_import", err.Error())
		}
		return
	}
	if req.Apply {
		systemID := systems.DefaultID
		if target != "" {
			system, err := systems.ByCode(r.Context(), s.DB.Read, target)
			if err != nil {
				s.serverErr(w, "taxonomy.applied_system", err)
				return
			}
			systemID = system.ID
		}
		if s.Jobs != nil {
			s.Jobs.Nudge()
		}
		audit.Log(r.Context(), s.DB, s.Log, audit.Event{Actor: actor, SystemID: systemID, Action: "taxonomy.import", ObjectKind: "taxonomy", After: map[string]any{
			"preset_id": pf.ID, "preset_version": pf.Version, "content_sha256": diff.ContentSHA256, "mode": diff.Mode,
		}})
	}
	s.writeJSON(w, http.StatusOK, diff)
}

func (s *Server) ExportTaxonomy(w http.ResponseWriter, r *http.Request) {
	actor := s.requireAdmin(w, r)
	if actor == nil {
		return
	}
	systemID, ok := s.requireSystem(w, r, actor)
	if !ok {
		return
	}
	format, ok := taxonomyAuthoringFormat(r.URL.Query().Get("format"), presetfile.FormatHuML)
	if !ok {
		s.writeError(w, 400, "bad_format", "format must be huml or toml")
		return
	}
	skipSeeds := false
	if value := r.URL.Query().Get("skip_seeds"); value != "" {
		var err error
		skipSeeds, err = strconv.ParseBool(value)
		if err != nil {
			s.writeError(w, 400, "bad_skip_seeds", "skip_seeds must be true or false")
			return
		}
	}
	pf, err := taxonomy.BuildExport(r.Context(), s.DB, systemID, skipSeeds)
	if err != nil {
		s.writeError(w, 409, "taxonomy_export", err.Error())
		return
	}
	body, err := presetfile.Marshal(pf, format)
	if err != nil {
		s.writeError(w, 409, "taxonomy_export", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	filename := "archive." + string(format)
	if pf.System != "" {
		filename = pf.System + ".taxonomy." + string(format)
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	_, _ = w.Write(body)
}

func taxonomyAuthoringFormat(raw string, fallback presetfile.SerFormat) (presetfile.SerFormat, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return fallback, true
	case "huml":
		return presetfile.FormatHuML, true
	case "toml":
		return presetfile.FormatTOML, true
	default:
		return "", false
	}
}
func sha256hex(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func decodeRemaps(input map[string]int) (map[int]int, error) {
	out := make(map[int]int, len(input))
	for key, value := range input {
		code, err := strconv.Atoi(key)
		if err != nil || strconv.Itoa(code) != key {
			return nil, fmt.Errorf("remaps.%s: expected a canonical integer category code", key)
		}
		out[code] = value
	}
	return out, nil
}
