package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/suchi-dms/suchi/core/auth"
	"github.com/suchi-dms/suchi/core/mailsetup"
)

// WithMailSetup wires the mail wizard's runtime config. Leave it
// unwired to keep the endpoint disabled — POSTs then get 404.
func (s *Server) WithMailSetup(opts mailsetup.Options) *Server {
	s.mailSetup = opts
	return s
}

// MailSetupApply handles POST /api/admin/mail-setup. Admin-only.
//
// Body is a mailsetup.Request JSON payload. Success returns the
// mailsetup.Response with the .env path and (best-effort) restart
// outcome. The docker restart step failing does NOT fail the whole
// request — the file was still written and the operator can restart
// manually; the failure lives in resp.RestartError so the UI can show
// it as a warning banner.
func (s *Server) MailSetupApply(w http.ResponseWriter, r *http.Request) {
	if !s.mailSetup.Enabled() {
		http.NotFound(w, r)
		return
	}
	// Admin-only. Sessions with role=admin pass; API tokens need
	// an explicit admin:mail scope (which no token has by default),
	// so a stolen documents:write token can't rewrite creds.
	p := auth.FromContext(r.Context())
	if p == nil || (p.Role != "admin" && !auth.HasScope(p, "admin:mail")) {
		s.writeError(w, http.StatusForbidden, "forbidden", "admin required")
		return
	}
	var req mailsetup.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	resp, err := mailsetup.Apply(r.Context(), s.mailSetup, req)
	if err != nil {
		if errors.Is(err, mailsetup.ErrDisabled) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, mailsetup.ErrInvalidInput) {
			s.writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
			return
		}
		s.Log.Error("api.mailsetup.apply", "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "apply_failed", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}
