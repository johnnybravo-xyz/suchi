// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import "net/http"

const (
	handshakeAPIVersion    = 1
	handshakeMinAppVersion = "0.1.0"
)

type handshakeResponse struct {
	Product       string `json:"product"`
	APIVersion    int    `json:"api_version"`
	MinAppVersion string `json:"min_app_version"`
}

// GetHandshake reports the public compatibility contract used before a client
// sends credentials. It intentionally exposes no deployment-specific state.
func (s *Server) GetHandshake(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, handshakeResponse{
		Product:       "suchi",
		APIVersion:    handshakeAPIVersion,
		MinAppVersion: handshakeMinAppVersion,
	})
}
