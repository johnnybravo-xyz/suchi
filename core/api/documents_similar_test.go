package api

// FTS5 more-like-this "similar documents" endpoint. Load-bearing
// checks:
//   - Anonymous → 401.
//   - Non-existent id → 404.
//   - Docs with overlapping vocab surface; the source doc is
//     excluded from its own results.
//   - Empty content produces an empty result (not a 500).
//   - Tokenizer drops trivial words (length < 4 and the tiny
//     stopword list).

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/similar"
)

func newSimilarServer(t *testing.T) *Server {
	t.Helper()
	d := openTestDB(t)
	return &Server{
		DB:    d,
		Log:   slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Authz: authz.ACLAuthorizer{DB: d},
	}
}

func seedSimilarDoc(t *testing.T, s *Server, ownerID int64, title, content string) int64 {
	t.Helper()
	seedUser(t, s.DB, ownerID)
	inbox := seedStatsJDInbox(t, s.DB)
	res, err := s.DB.Write.ExecContext(context.Background(), `
		INSERT INTO documents(owner_id, original_blob, original_size, title, content,
		                     jd_category_id, mime_type, created_at, updated_at)
		VALUES (?, ?, 0, ?, ?, ?, 'application/pdf', 0, 0)
	`, ownerID, "sha_"+title, title, content, inbox)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func doSimilar(t *testing.T, s *Server, docID int64, p *pluginapi.Principal) (int, SimilarResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx := context.Background()
	if p != nil {
		ctx = auth.WithPrincipal(ctx, p)
	}
	r := httptest.NewRequest("GET", "/api/documents/"+strconv.FormatInt(docID, 10)+"/similar", nil).WithContext(ctx)
	r.SetPathValue("id", strconv.FormatInt(docID, 10))
	s.GetSimilarDocuments(rec, r)
	var body SimilarResponse
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("json: %v; body=%s", err, rec.Body.String())
		}
	}
	return rec.Code, body
}

func TestSimilarDocuments_Anonymous(t *testing.T) {
	s := newSimilarServer(t)
	code, _ := doSimilar(t, s, 1, nil)
	if code != 401 {
		t.Fatalf("anonymous status=%d, want 401", code)
	}
}

func TestSimilarDocuments_NotFound(t *testing.T) {
	s := newSimilarServer(t)
	seedUser(t, s.DB, 1)
	code, _ := doSimilar(t, s, 9999, adminPrincipal(1))
	if code != 404 {
		t.Fatalf("missing doc status=%d, want 404", code)
	}
}

func TestSimilarDocuments_FindsOverlappingDocs(t *testing.T) {
	s := newSimilarServer(t)
	src := seedSimilarDoc(t, s, 1, "March electricity bill",
		"Electricity utility charge for March 2026 covering residential consumption at 220kWh")
	near := seedSimilarDoc(t, s, 1, "April electricity bill",
		"Electricity utility charge for April 2026 covering residential consumption at 240kWh")
	far := seedSimilarDoc(t, s, 1, "Insurance renewal",
		"Comprehensive auto insurance policy renewal for vehicle registration 2026")

	code, body := doSimilar(t, s, src, adminPrincipal(1))
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	if body.Method != "fts" {
		t.Errorf("method = %q, want fts", body.Method)
	}
	// Source doc must never appear in its own results.
	for _, r := range body.Results {
		if r.ID == src {
			t.Errorf("source doc leaked into results: %+v", r)
		}
	}
	// The overlapping electricity doc must land above the insurance
	// doc — same domain vocab.
	var nearRank, farRank int = -1, -1
	for i, r := range body.Results {
		if r.ID == near {
			nearRank = i
		}
		if r.ID == far {
			farRank = i
		}
	}
	if nearRank == -1 {
		t.Errorf("near-vocab doc not returned: results=%+v", body.Results)
	}
	if nearRank != -1 && farRank != -1 && nearRank > farRank {
		t.Errorf("near-vocab doc ranked below far-vocab doc: near=%d far=%d", nearRank, farRank)
	}
}

func TestSimilarDocuments_EmptyContentReturnsEmpty(t *testing.T) {
	s := newSimilarServer(t)
	// Zero-content doc — tokenizer returns nothing → empty result.
	src := seedSimilarDoc(t, s, 1, "", "")
	code, body := doSimilar(t, s, src, adminPrincipal(1))
	if code != 200 {
		t.Fatalf("status=%d", code)
	}
	if len(body.Results) != 0 {
		t.Errorf("empty-content doc returned %d results, want 0", len(body.Results))
	}
}

func TestTopTokens(t *testing.T) {
	cases := []struct {
		in   string
		k    int
		want []string // must be a subset match — order sensitive to frequency
	}{
		// Repeated word wins.
		{"invoice invoice invoice payment", 2, []string{"invoice", "payment"}},
		// Short words + trivial stopwords drop.
		{"the and for is a b c to it me", 5, nil},
		// Case-fold.
		{"Bescom BESCOM bescom Insurance", 2, []string{"bescom", "insurance"}},
	}
	for _, tc := range cases {
		got := similar.TopTokens(tc.in, tc.k)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("TopTokens(%q, %d) = %v, want %v", tc.in, tc.k, got, tc.want)
		}
	}
}
