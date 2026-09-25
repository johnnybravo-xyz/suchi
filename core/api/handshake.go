// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import "net/http"

const handshakeAPIVersion = 1

type handshakeResponse struct {
	Product         string   `json:"product"`
	APIVersion      int      `json:"api_version"`
	MobileContracts []string `json:"mobile_contracts"`
}

// GetHandshake reports the public compatibility contract used before a client
// sends credentials. It intentionally exposes no deployment-specific state.
func (s *Server) GetHandshake(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, handshakeResponse{
		Product:         "suchi",
		APIVersion:      handshakeAPIVersion,
		MobileContracts: []string{"suchi-companion-v1"},
	})
}
