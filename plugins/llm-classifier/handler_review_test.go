package llmclassifier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/db"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestHandlerClearsOnlyItsOwnReviewTag(t *testing.T) {
	for _, tc := range []struct {
		name        string
		preexisting bool
		customSlug  bool
		customName  bool
		reassert    bool
		confidence  float64
		tags        []string
		status      int
		invalid     bool
		wantReview  bool
		wantOwned   bool
	}{
		{name: "confident rescan", confidence: 0.9},
		{name: "custom canonical review slug", customSlug: true, confidence: 0.9},
		{name: "model requests canonical review name", customName: true, confidence: 0.9, tags: []string{"review-myself"}, wantReview: true, wantOwned: true},
		{name: "human or legacy tag", preexisting: true, confidence: 0.9, wantReview: true},
		{name: "human reasserts during model call", reassert: true, confidence: 0.9, wantReview: true},
		{name: "good title but low confidence", confidence: 0.6, wantReview: true, wantOwned: true},
		{name: "model still requests review", confidence: 0.9, tags: []string{"needs-review"}, wantReview: true, wantOwned: true},
		{name: "equivalent review tag spelling", confidence: 0.9, tags: []string{"Needs review"}, wantReview: true, wantOwned: true},
		{name: "provider failure", status: http.StatusBadGateway, wantReview: true, wantOwned: true},
		{name: "invalid result", invalid: true, wantReview: true, wantOwned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			d, docID := openHandlerDocument(t, "camera.jpg", "Invoice total 120.00")
			if tc.preexisting || tc.customSlug || tc.customName {
				tagName, tagSlug := "needs-review", "needs-review"
				if tc.customSlug {
					tagSlug = "my-review-tag"
				}
				if tc.customName {
					tagName = "review-myself"
				}
				if _, err := d.Write.ExecContext(ctx, `
					INSERT INTO tags(id, name, slug, created_at, updated_at)
					VALUES (99, ?, ?, 0, 0)
				`, tagName, tagSlug); err != nil {
					t.Fatal(err)
				}
			}
			if tc.preexisting {
				if _, err := d.Write.ExecContext(ctx,
					`INSERT INTO document_tags(document_id, tag_id) VALUES (?, 99)`, docID); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				result := Result{Title: "Well titled invoice", Confidence: 0.3}
				if calls > 2 {
					if tc.reassert {
						if _, err := d.Write.ExecContext(ctx, `
							UPDATE document_tags SET classifier_owned = 0 WHERE document_id = ?
						`, docID); err != nil {
							t.Error(err)
							w.WriteHeader(http.StatusInternalServerError)
							return
						}
					}
					if tc.status != 0 {
						w.WriteHeader(tc.status)
						return
					}
					result.Confidence = tc.confidence
					result.Tags = append([]string{"invoice"}, tc.tags...)
					if tc.invalid {
						result.Confidence = 1.5
					}
				}
				body, err := json.Marshal(result)
				if err != nil {
					t.Error(err)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []any{map[string]any{"message": map[string]string{"content": string(body)}}},
				})
			}))
			defer server.Close()
			p, err := New(Config{EndpointURL: server.URL, Model: "test", ConfidenceThreshold: 0.7}, silentLog())
			if err != nil {
				t.Fatal(err)
			}
			h := NewHandler(p, d, silentLog())
			event := pluginapi.Event{Kind: Kind, DocID: docID}
			for range 2 {
				if err := h.Handle(ctx, event); err != nil {
					t.Fatal(err)
				}
				assertReviewTag(t, d, docID, true, !tc.preexisting)
			}
			err = h.Handle(ctx, event)
			if wantError := tc.status != 0 || tc.invalid; (err != nil) != wantError {
				t.Fatalf("rescan error = %v, want error %v", err, wantError)
			}
			assertReviewTag(t, d, docID, tc.wantReview, tc.wantOwned)
			var title string
			var version, count int
			if err := d.Read.QueryRowContext(ctx, `
				SELECT title, pipeline_version_llm,
				       (SELECT COUNT(*) FROM document_tags WHERE document_id = documents.id)
				FROM documents WHERE id = ?
			`, docID).Scan(&title, &version, &count); err != nil {
				t.Fatal(err)
			}
			wantTitle, wantCount := "camera.jpg", 1
			if tc.status == 0 && !tc.invalid && tc.confidence >= 0.7 {
				wantTitle = "Well titled invoice"
				if tc.wantReview {
					wantCount++
				}
			}
			if title != wantTitle || version != PipelineVersionLLM || count != wantCount {
				t.Fatalf("title=%q version=%d tags=%d, want %q/%d/%d", title, version, count, wantTitle, PipelineVersionLLM, wantCount)
			}
		})
	}
}

func TestConfidentModelReviewTagCanClearOnLaterRescan(t *testing.T) {
	ctx := context.Background()
	d, docID := openHandlerDocument(t, "Invoice", "Invoice total 120.00")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		result := Result{Title: "Invoice", Confidence: 0.9}
		if calls == 1 {
			result.Tags = []string{"needs-review"}
		}
		body, _ := json.Marshal(result)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"content": string(body)}}},
		})
	}))
	defer server.Close()
	p, err := New(Config{EndpointURL: server.URL, Model: "test"}, silentLog())
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(p, d, silentLog())
	event := pluginapi.Event{Kind: Kind, DocID: docID}
	if err := h.Handle(ctx, event); err != nil {
		t.Fatal(err)
	}
	assertReviewTag(t, d, docID, true, true)
	if err := h.Handle(ctx, event); err != nil {
		t.Fatal(err)
	}
	assertReviewTag(t, d, docID, false, false)
}

func assertReviewTag(t *testing.T, d *db.DB, docID int64, exists, owned bool) {
	t.Helper()
	var count, ownedCount int
	if err := d.Read.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(dt.classifier_owned), 0)
		FROM document_tags dt JOIN tags t ON t.id = dt.tag_id
		WHERE dt.document_id = ? AND (t.name = 'needs-review' OR t.slug = 'needs-review')
	`, docID).Scan(&count, &ownedCount); err != nil {
		t.Fatal(err)
	}
	if (count == 1) != exists || (ownedCount == 1) != owned || count > 1 {
		t.Fatalf("review tags=%d owned=%d, want exists=%v owned=%v", count, ownedCount, exists, owned)
	}
}
