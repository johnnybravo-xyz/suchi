// /api/jd/categories/ — read-only listing over the Johnny.Decimal
// taxonomy tree. Every JSON consumer (mobile app, MCP tool, agent,
// SPA) needs this to render a filing chip or drive a "File under…"
// picker; without it, the wire surface exposes only bare
// jd_category_id ints with no way to look up what they mean.
//
// Read-only on purpose. Categories are created via preset swap
// (POST /api/admin/setup/preset), not per-row insert. Individual
// mutation of an existing row's name/description is admin config
// territory not yet wired.

package api

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/jd"
)

// presetsView projects jd.Presets() into the wire shape. Kept out
// of the handler so a unit test can pin the projection without a
// server harness.
func presetsView() []PresetRow {
	src := jd.Presets()
	out := make([]PresetRow, 0, len(src))
	for _, p := range src {
		row := PresetRow{
			ID:          p.ID,
			Name:        p.Label,
			Description: p.Description,
			Blank:       p.Blank,
			Areas:       make([]PresetArea, 0, len(p.Tree.Areas)),
		}
		for _, a := range p.Tree.Areas {
			row.Areas = append(row.Areas, PresetArea{
				Code:          a.Start,
				Name:          a.Name,
				CategoryCount: len(a.Categories),
			})
		}
		out = append(out, row)
	}
	return out
}

// JDCategory is one row in /api/jd/categories/. Denormalized with the
// area info so pickers don't need a second round-trip. area_code is
// the numeric start of the area range (e.g. 20 for "Money", covering
// codes 20–29); the category's own code encodes the area implicitly
// (22 falls in the 20–29 range) so no jd_area_code duplication on
// DocumentDetail.
type JDCategory struct {
	ID          int64  `json:"id"`
	Code        int64  `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	AreaCode    int64  `json:"area_code"`
	AreaName    string `json:"area_name"`
	System      bool   `json:"system,omitempty"`
}

// ListJDCategories — GET /api/jd/categories/.
//
//	?q=<prefix>   case-insensitive prefix match on category code OR
//	              name. "22" and "tax" both match "22 Tax".
//	?area=<code>  scope to the range [area, area+9]. e.g. area=20
//	              returns categories 20–29.
//
// DRF envelope. Read is open to any authed user — categories are
// shared vocabulary; grants on the taxonomy layer would cover write
// operations if we ever add them.
func (s *Server) ListJDCategories(w http.ResponseWriter, r *http.Request) {
	if s.requireAuth(w, r) == nil {
		return
	}

	var (
		where []string
		args  []any
	)

	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		// Match code prefix OR name prefix. LIKE 'x%' is index-safe
		// (SQLite uses the primary key on jd_categories.id for scan,
		// but the row count is small enough that a filesort is fine).
		like := escapeLike(strings.ToLower(q)) + "%"
		// Cast code to TEXT for the prefix match so "22" matches
		// category 22 regardless of the underlying int type.
		where = append(where, "(lower(jc.name) LIKE ? ESCAPE '\\' OR CAST(jc.code AS TEXT) LIKE ? ESCAPE '\\')")
		args = append(args, like, like)
	}

	if a := strings.TrimSpace(r.URL.Query().Get("area")); a != "" {
		areaStart, err := strconv.ParseInt(a, 10, 64)
		if err != nil || areaStart < 0 {
			s.writeError(w, http.StatusBadRequest, "bad_area",
				"area must be a non-negative integer (the area's start code, e.g. 20)")
			return
		}
		where = append(where, "jc.area_start = ?")
		args = append(args, areaStart)
	}

	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}

	// COUNT first for the envelope. Small table; a scan is cheap.
	var total int
	if err := s.DB.Read.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM jd_categories jc"+whereSQL, args...).Scan(&total); err != nil {
		s.serverErr(w, "jd.count", err)
		return
	}

	p := ParsePageParams(r, 100, 500)
	queryArgs := append([]any{}, args...)
	queryArgs = append(queryArgs, p.PageSize, p.Offset())

	rows, err := s.DB.Read.QueryContext(r.Context(), `
		SELECT jc.id, jc.code, jc.name, COALESCE(jc.description, ''),
		       ja.code_start, ja.name, jc.system
		FROM jd_categories jc
		JOIN jd_areas ja ON ja.code_start = jc.area_start
	`+whereSQL+`
		ORDER BY jc.code
		LIMIT ? OFFSET ?
	`, queryArgs...)
	if err != nil {
		s.serverErr(w, "jd.list", err)
		return
	}
	defer rows.Close()

	out := []JDCategory{}
	for rows.Next() {
		var (
			c      JDCategory
			desc   string
			system int
		)
		if err := rows.Scan(&c.ID, &c.Code, &c.Name, &desc,
			&c.AreaCode, &c.AreaName, &system); err != nil {
			s.serverErr(w, "jd.scan", err)
			return
		}
		c.Description = desc
		c.System = system == 1
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		s.serverErr(w, "jd.rows", err)
		return
	}
	s.writeJSON(w, http.StatusOK, BuildEnvelope(r, total, p, out))
}

// Compile-time reference — sql import is used via QueryContext error
// checks; without this the linter complains on files with a
// conditional import chain.
var _ = sql.ErrNoRows

// PresetRow is one row of the /api/presets/ listing. The wizard
// renders the areas + category counts as a tree preview so the
// operator sees what a Suchi Preset actually looks like before
// committing to it.
type PresetRow struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	Blank       bool         `json:"blank,omitempty"`
	Areas       []PresetArea `json:"areas"`
}

// PresetArea is the area-level summary the wizard needs — code
// range for the header, name for the label, category_count so it
// can show "12 categories" without hydrating the whole tree.
type PresetArea struct {
	Code          int    `json:"code"`
	Name          string `json:"name"`
	CategoryCount int    `json:"category_count"`
}

// ListPresets — GET /api/presets/. Admin only. Sourced from
// core/jd.Presets() so a new preset appears in the wizard with no
// SPA release. No envelope — the population is bounded (currently
// five rows) and pagination would just add noise.
func (s *Server) ListPresets(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		s.writeError(w, http.StatusUnauthorized, "unauthorized", "auth required")
		return
	}
	if p.Role != "admin" {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin only")
		return
	}
	s.writeJSON(w, http.StatusOK, presetsView())
}
