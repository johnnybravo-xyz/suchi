package api

// Admin import/export for suchi-taxonomy/v1 files.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	huml "github.com/huml-lang/go-huml"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/jd/importer"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
	"github.com/johnnybravo-xyz/suchi/core/taxonomy"
)

// TaxonomyImportReq is the POST body.
//
// Remaps resolves merge-mode collisions. Key = the incoming preset
// code (as a JSON string — JSON object keys can't be integers).
// Value = 0 to skip the category, or a fresh in-decade code to
// import under. Ignored in replace mode.
type TaxonomyImportReq struct {
	Content   string         `json:"content"`
	Format    string         `json:"format,omitempty"`
	Apply     bool           `json:"apply,omitempty"`
	SkipSeeds bool           `json:"skip_seeds,omitempty"`
	Remaps    map[string]int `json:"remaps,omitempty"`
}

// TaxonomyDiff is what dry-run returns (and apply=true echoes on
// success). Small on purpose so the SPA can render a tree preview
// without a second call.
type TaxonomyDiff struct {
	PresetID           string         `json:"preset_id"`
	PresetVersion      int            `json:"preset_version"`
	ContentSHA256      string         `json:"content_sha256"`
	Mode               string         `json:"mode"` // "replace" | "merge"
	AreasIncoming      int            `json:"areas_incoming"`
	CategoriesIncoming int            `json:"categories_incoming"`
	CategoriesToAdd    []int          `json:"categories_to_add,omitempty"`
	Collisions         []TaxonomyColl `json:"collisions,omitempty"`
	KeywordsToSeed     int            `json:"keywords_to_seed"`
	AutomationsToSeed  int            `json:"automations_to_seed"`
	Applied            bool           `json:"applied"`
}

// TaxonomyColl reports a merge-mode collision (same code, different
// name). The SPA renders these as skip/remap rows.
//
// ProposedCode is the next free code inside the incoming category's
// decade — a good default if the operator wants "just move it out
// of the way". 0 when no code in the decade is free (the operator
// must pick skip or pick a code in a different area — which the
// importer will refuse).
type TaxonomyColl struct {
	Code         int    `json:"code"`
	Existing     string `json:"existing"`
	Incoming     string `json:"incoming"`
	ProposedCode int    `json:"proposed_code,omitempty"`
}

// ImportTaxonomy — POST /api/admin/taxonomy/import.
func (s *Server) ImportTaxonomy(w http.ResponseWriter, r *http.Request) {
	p := s.requireAdmin(w, r)
	if p == nil {
		return
	}
	var req TaxonomyImportReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", err.Error())
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		s.writeError(w, http.StatusBadRequest, "empty_content", "content is required")
		return
	}

	format, ok := taxonomyAuthoringFormat(req.Format, "")
	if !ok {
		s.writeError(w, http.StatusBadRequest, "bad_format", "format must be huml or toml")
		return
	}
	pf, err := presetfile.Parse([]byte(req.Content), format)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "parse", err.Error())
		return
	}

	// Compute the diff. Mode = replace when the archive has no non-
	// inbox docs; merge otherwise.
	mode, err := taxonomy.ImportMode(r.Context(), s.DB)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}
	diff := &TaxonomyDiff{
		PresetID:      pf.ID,
		PresetVersion: pf.Version,
		ContentSHA256: sha256hex(req.Content),
		Mode:          mode,
	}
	for _, a := range pf.Areas {
		diff.AreasIncoming++
		for _, c := range a.Categories {
			diff.CategoriesIncoming++
			if pf.Seeds == nil {
				continue
			}
			_ = c
		}
	}
	if pf.Seeds != nil {
		diff.AutomationsToSeed = len(pf.Seeds.Automations)
	}
	for _, a := range pf.Areas {
		for _, c := range a.Categories {
			diff.KeywordsToSeed += len(c.Keywords)
		}
	}

	// Merge-mode collision detection — additive-only per spec §3. Any
	// same-code / different-name row surfaces as a Collision; caller
	// must resolve before applying (short-circuit for now).
	if mode == "merge" {
		coll, cats, err := detectMergeCollisions(r.Context(), s, pf)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
			return
		}
		diff.Collisions = coll
		diff.CategoriesToAdd = cats
	}

	if !req.Apply {
		s.writeJSON(w, http.StatusOK, diff)
		return
	}

	if mode == "merge" {
		remaps := decodeRemaps(req.Remaps)
		if err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
			if _, err := importer.ApplyMerge(r.Context(), tx, s.Log, pf, importer.Options{
				SkipSeeds: req.SkipSeeds,
				Remaps:    remaps,
			}); err != nil {
				return err
			}
			return importer.WriteImportProvenance(r.Context(), tx,
				pf.ID, pf.Version, diff.ContentSHA256)
		}); err != nil {
			var unresolved *importer.UnresolvedCollisionsError
			if errors.As(err, &unresolved) {
				// Preserve the proposed_code hints computed earlier;
				// index by incoming code to enrich the unresolved list.
				byCode := map[int]TaxonomyColl{}
				for _, c := range diff.Collisions {
					byCode[c.Code] = c
				}
				diff.Collisions = diff.Collisions[:0]
				for _, u := range unresolved.Items {
					c := TaxonomyColl{Code: u.Code, Existing: u.Existing, Incoming: u.Incoming}
					if prior, ok := byCode[u.Code]; ok {
						c.ProposedCode = prior.ProposedCode
					}
					diff.Collisions = append(diff.Collisions, c)
				}
				s.writeJSON(w, http.StatusConflict, diff)
				return
			}
			s.writeError(w, http.StatusInternalServerError, "db_write", err.Error())
			return
		}
	} else {
		// Replace path — mirrors applyPreset in core/jd/presets.go.
		if err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
			// Park docs on the current system category; delete + re-seed.
			if _, err := tx.ExecContext(r.Context(), `
				UPDATE documents SET jd_category_id = (
					SELECT id FROM jd_categories WHERE system = 1 LIMIT 1
				) WHERE trashed_at IS NULL
			`); err != nil {
				return fmt.Errorf("park docs: %w", err)
			}
			if _, err := tx.ExecContext(r.Context(), `DELETE FROM jd_categories`); err != nil {
				return err
			}
			if _, err := tx.ExecContext(r.Context(), `DELETE FROM jd_areas`); err != nil {
				return err
			}
			_, err := importer.ApplyReplace(r.Context(), tx, s.Log, pf, importer.Options{
				SkipSeeds: req.SkipSeeds,
			})
			if err != nil {
				return err
			}
			// Repoint parked docs to the new inbox.
			var newInbox int64
			if err := tx.QueryRowContext(r.Context(),
				`SELECT id FROM jd_categories WHERE system = 1 LIMIT 1`).Scan(&newInbox); err != nil {
				return err
			}
			if _, err := tx.ExecContext(r.Context(), `
				UPDATE documents SET jd_category_id = ? WHERE trashed_at IS NULL
			`, newInbox); err != nil {
				return err
			}
			return importer.WriteImportProvenance(r.Context(), tx,
				pf.ID, pf.Version, diff.ContentSHA256)
		}); err != nil {
			s.writeError(w, http.StatusInternalServerError, "db_write", err.Error())
			return
		}
	}
	diff.Applied = true
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		Actor: p, Action: "taxonomy.import",
		ObjectKind: "taxonomy", ObjectID: 0,
		After: map[string]any{
			"preset_id":      pf.ID,
			"preset_version": pf.Version,
			"content_sha256": diff.ContentSHA256,
			"mode":           mode,
		},
	})
	s.writeJSON(w, http.StatusOK, diff)
}

// ExportTaxonomy — GET /api/admin/taxonomy/export.
func (s *Server) ExportTaxonomy(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	format, ok := taxonomyAuthoringFormat(r.URL.Query().Get("format"), presetfile.FormatHuML)
	if !ok {
		s.writeError(w, http.StatusBadRequest, "bad_format", "format must be huml or toml")
		return
	}

	pf, err := taxonomy.BuildExport(r.Context(), s.DB)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "db_read", err.Error())
		return
	}
	var (
		body   []byte
		ct     = "text/plain; charset=utf-8"
		suffix = "toml"
	)
	switch format {
	case presetfile.FormatHuML:
		b, err := huml.Marshal(pf)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "encode", err.Error())
			return
		}
		body, suffix = b, "huml"
	case presetfile.FormatTOML:
		var buf bytes.Buffer
		if err := toml.NewEncoder(&buf).Encode(pf); err != nil {
			s.writeError(w, http.StatusInternalServerError, "encode", err.Error())
			return
		}
		body, suffix = buf.Bytes(), "toml"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="taxonomy.%s"`, suffix))
	_, _ = io.Copy(w, bytes.NewReader(body))
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

// detectMergeCollisions walks the incoming PresetFile against the
// current jd_categories rows and reports (colliding-code, cats-to-add).
func detectMergeCollisions(ctx context.Context, s *Server, pf *presetfile.PresetFile) ([]TaxonomyColl, []int, error) {
	rows, err := s.DB.Read.QueryContext(ctx, `SELECT code, name FROM jd_categories`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	existing := map[int]string{}
	for rows.Next() {
		var code int
		var name string
		if err := rows.Scan(&code, &name); err != nil {
			return nil, nil, err
		}
		existing[code] = name
	}
	// Reserve codes the incoming preset has already claimed for
	// non-colliding categories so the "propose next free" walk
	// doesn't hand two collisions the same target.
	reserved := map[int]bool{}
	for k := range existing {
		reserved[k] = true
	}
	var coll []TaxonomyColl
	var add []int
	for _, a := range pf.Areas {
		for _, c := range a.Categories {
			ex, present := existing[c.Code]
			if !present {
				add = append(add, c.Code)
				reserved[c.Code] = true
				continue
			}
			if ex != c.Name {
				proposed := nextFreeInDecade(c.Code, reserved)
				if proposed > 0 {
					reserved[proposed] = true
				}
				coll = append(coll, TaxonomyColl{
					Code: c.Code, Existing: ex, Incoming: c.Name,
					ProposedCode: proposed,
				})
			}
		}
	}
	return coll, add, nil
}

// nextFreeInDecade returns the smallest unused code inside the same
// JD decade as `code`. Returns 0 when every code in the decade is
// taken.
func nextFreeInDecade(code int, taken map[int]bool) int {
	decade := (code / 10) * 10
	for c := decade + 1; c <= decade+9; c++ {
		if c == code {
			continue
		}
		if !taken[c] {
			return c
		}
	}
	return 0
}

func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// decodeRemaps converts the wire form (string→int; keys are numeric
// strings because JSON object keys can't be integers) into the
// int→int shape ApplyMerge expects. Junk keys are silently
// dropped — importer surfaces unresolved collisions explicitly.
func decodeRemaps(in map[string]int) map[int]int {
	if len(in) == 0 {
		return nil
	}
	out := make(map[int]int, len(in))
	for k, v := range in {
		if code, err := strconv.Atoi(strings.TrimSpace(k)); err == nil {
			out[code] = v
		}
	}
	return out
}
