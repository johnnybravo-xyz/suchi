package api

// Admin taxonomy import + export — the two endpoints backing the
// wizard's "Import a file" tab and Admin > Taxonomy > Import/Export.
//
// Import accepts a `suchi-taxonomy/v1` file (HuML/TOML/YAML), parses
// it, and applies via the same core/jd/importer that powers the
// built-in preset picker. Replace mode fires on an empty archive;
// merge mode fires when documents already exist (additive-only per
// spec §3 — no rename, no delete). Dry-run is the default: apply
// only when the caller sets `apply: true`.
//
// Export dumps the current tree + preset-owned seeds back into a
// valid `suchi-taxonomy/v1` file, format selectable via query.

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
	"gopkg.in/yaml.v3"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/jd/importer"
	"github.com/johnnybravo-xyz/suchi/core/jd/presetfile"
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
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin required")
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

	pf, err := presetfile.Parse([]byte(req.Content), presetfile.SerFormat(req.Format))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "parse", err.Error())
		return
	}

	// Compute the diff. Mode = replace when the archive has no non-
	// inbox docs; merge otherwise.
	mode, err := chooseImportMode(r.Context(), s)
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
	p := auth.FromContext(r.Context())
	if p == nil || p.Role != "admin" {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	format := presetfile.SerFormat(strings.ToLower(r.URL.Query().Get("format")))
	if format == "" {
		format = presetfile.FormatHuML
	}

	pf, err := buildExportPresetFile(r.Context(), s)
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
	case presetfile.FormatYAML:
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(pf); err != nil {
			s.writeError(w, http.StatusInternalServerError, "encode", err.Error())
			return
		}
		_ = enc.Close()
		body, suffix = buf.Bytes(), "yaml"
	case presetfile.FormatTOML:
		var buf bytes.Buffer
		if err := toml.NewEncoder(&buf).Encode(pf); err != nil {
			s.writeError(w, http.StatusInternalServerError, "encode", err.Error())
			return
		}
		body, suffix = buf.Bytes(), "toml"
	default:
		s.writeError(w, http.StatusBadRequest, "bad_format", "format must be huml/toml/yaml")
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="taxonomy.%s"`, suffix))
	_, _ = io.Copy(w, bytes.NewReader(body))
}

// chooseImportMode returns "replace" if the archive is empty of
// non-inbox docs, else "merge".
func chooseImportMode(ctx context.Context, s *Server) (string, error) {
	var stray int
	err := s.DB.Read.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM documents d
		JOIN jd_categories c ON c.id = d.jd_category_id
		WHERE d.trashed_at IS NULL AND c.system = 0
	`).Scan(&stray)
	if err != nil {
		return "", err
	}
	if stray == 0 {
		return "replace", nil
	}
	return "merge", nil
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

// buildExportPresetFile reads jd_areas + jd_categories + preset-owned
// rules + preset-owned automations and produces a *PresetFile ready
// for encoding. Keywords are reconstructed from content_contains rules
// whose then_kind is set_jd_category — that's a stable projection of
// the seeded shape.
func buildExportPresetFile(ctx context.Context, s *Server) (*presetfile.PresetFile, error) {
	pf := &presetfile.PresetFile{
		Format:  presetfile.Format,
		ID:      "exported",
		Version: 1,
		Name:    "Exported taxonomy",
		Story:   "Round-tripped from the running instance. Review before sharing.",
	}
	rows, err := s.DB.Read.QueryContext(ctx, `
		SELECT code_start, name FROM jd_areas ORDER BY position, code_start
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var code int
		var name string
		if err := rows.Scan(&code, &name); err != nil {
			return nil, err
		}
		pf.Areas = append(pf.Areas, presetfile.Area{Code: code, Name: name})
	}

	// Attach categories to areas.
	for i := range pf.Areas {
		a := &pf.Areas[i]
		crows, err := s.DB.Read.QueryContext(ctx, `
			SELECT code, name, COALESCE(description, ''), system
			FROM jd_categories WHERE area_start = ? ORDER BY code
		`, a.Code)
		if err != nil {
			return nil, err
		}
		for crows.Next() {
			var c presetfile.Category
			var sys int
			if err := crows.Scan(&c.Code, &c.Name, &c.Description, &sys); err != nil {
				crows.Close()
				return nil, err
			}
			if sys == 1 {
				pf.Inbox = c.Code
			}
			// Reconstruct keywords: content_contains rules with
			// then_value == this category code and preset_slug set.
			kwRows, err := s.DB.Read.QueryContext(ctx, `
				SELECT if_value FROM rules
				WHERE if_kind = 'content_contains'
				  AND then_kind = 'set_jd_category'
				  AND then_value = ?
				  AND preset_slug IS NOT NULL
				ORDER BY if_value
			`, fmt.Sprintf("%d", c.Code))
			if err != nil {
				crows.Close()
				return nil, err
			}
			for kwRows.Next() {
				var kw string
				if err := kwRows.Scan(&kw); err != nil {
					kwRows.Close()
					crows.Close()
					return nil, err
				}
				c.Keywords = append(c.Keywords, kw)
			}
			kwRows.Close()
			a.Categories = append(a.Categories, c)
		}
		crows.Close()
	}

	// TODO: include preset-owned seeds.automations in export. Not
	// wired yet — seeding round-trip works one-way (import → apply)
	// today. The next revision fills in the export side.

	return pf, nil
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
