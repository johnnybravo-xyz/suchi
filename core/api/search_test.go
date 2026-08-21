package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
)

func TestSearchMalformedFTSQueryReturnsBadRequest(t *testing.T) {
	s := &Server{
		DB:  openTestDB(t),
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	req := httptest.NewRequest(http.MethodGet, "/api/search/?q=%22", nil)
	req = req.WithContext(auth.WithPrincipal(context.Background(), adminPrincipal(1)))
	rec := httptest.NewRecorder()

	s.Search(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"bad_query"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
