// /api/admin/refile — one-shot admin action that re-runs the
// deterministic rules classifier against every live doc and enqueues
// a render/move job for every live doc. See docs/refile.mdx for the
// full walkthrough.
//
// The selling point: "you can always come back to change this."
// Swap Suchi Presets, edit storage-path templates, tune rules — then
// hit this to make the change stick across the existing corpus.

package api

import (
	"encoding/json"
	"net/http"

	"github.com/johnnybravo-xyz/suchi/core/refile"
)

// Refile — POST /api/admin/refile. Admin-only. Body is a JSON blob
// mirroring refile.Options.
func (s *Server) Refile(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	var body struct {
		SkipRules  bool  `json:"skip_rules,omitempty"`
		SkipRender bool  `json:"skip_render,omitempty"`
		OwnerID    int64 `json:"owner_id,omitempty"`
	}
	// Empty body is valid — it means "run both passes across all owners".
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON")
			return
		}
	}
	stats, err := refile.All(r.Context(), s.DB, s.Log, refile.Options{
		SkipRules:  body.SkipRules,
		SkipRender: body.SkipRender,
		OwnerID:    body.OwnerID,
	})
	if err != nil {
		s.serverErr(w, "refile.all", err)
		return
	}
	if s.Jobs != nil {
		s.Jobs.Nudge()
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"docs_scanned":    stats.DocsScanned,
		"rules_applied":   stats.RulesApplied,
		"render_enqueued": stats.RenderEnqueued,
		"errors":          stats.Errors,
		"elapsed_ms":      stats.Elapsed.Milliseconds(),
	})
}
