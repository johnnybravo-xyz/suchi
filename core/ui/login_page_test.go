// SPDX-License-Identifier: AGPL-3.0-or-later

package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoginPageMapsErrorCodesAndShowsCodeOnlyWhenAsked(t *testing.T) {
	s := newUISrv(t)
	mux := http.NewServeMux()
	s.Register(mux)
	for _, tc := range []struct {
		query, wantText string
		wantCodeField   bool
	}{
		{query: "", wantCodeField: false},
		{query: "?error=invalid_credentials", wantText: "Email or password did not match.", wantCodeField: false},
		{query: "?error=two_factor_required", wantText: "authenticator app", wantCodeField: true},
		{query: "?error=two_factor_invalid", wantText: "fresh code", wantCodeField: true},
		{query: "?error=two_factor_locked", wantText: "Ask an administrator", wantCodeField: false},
		{query: "?error=%3Cb%3Einjected%3C%2Fb%3E", wantText: "Could not sign in.", wantCodeField: false},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login"+tc.query, nil))
		body := rec.Body.String()
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status=%d", tc.query, rec.Code)
		}
		if tc.wantText != "" && !strings.Contains(body, tc.wantText) {
			t.Errorf("%s: body lacks %q", tc.query, tc.wantText)
		}
		if strings.Contains(body, "injected") {
			t.Errorf("%s: page echoed query text", tc.query)
		}
		if got := strings.Contains(body, `name="two_factor_code"`); got != tc.wantCodeField {
			t.Errorf("%s: code field shown=%t, want %t", tc.query, got, tc.wantCodeField)
		}
	}
}
