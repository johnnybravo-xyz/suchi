package llmclassifier

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/approvals"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/documentstate"
	"github.com/johnnybravo-xyz/suchi/core/lang"
	"github.com/johnnybravo-xyz/suchi/core/settings"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestOptedOutMetadataRemainsReviewOnly(t *testing.T) {
	for _, score := range []float64{0.2, 1} {
		t.Run(itoaScore(score), func(t *testing.T) {
			ctx := context.Background()
			d, docID := openHandlerDocument(t, "Original", "Invoice 2026-08-06")
			setHandlerAutoApply(t, d, false)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				body, _ := json.Marshal(Result{Title: "Model title", Correspondent: "Model sender", Tags: []string{"model-tag"}, Language: "de", Confidence: score})
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(body)}}}})
			}))
			defer server.Close()
			p, err := New(Config{EndpointURL: server.URL, Model: "test", ConfidenceThreshold: 0.7}, silentLog())
			if err != nil {
				t.Fatal(err)
			}
			if err := NewHandler(p, d, silentLog()).Handle(ctx, pluginapi.Event{SystemID: 1, Kind: Kind, DocID: docID}); err != nil {
				t.Fatal(err)
			}
			var title, language string
			var correspondent, vocabulary int
			if err := d.Read.QueryRow(`SELECT title, COALESCE(languages, ''), COALESCE(correspondent_id, 0) FROM documents WHERE id=?`, docID).Scan(&title, &language, &correspondent); err != nil {
				t.Fatal(err)
			}
			if err := d.Read.QueryRow(`SELECT (SELECT COUNT(*) FROM correspondents WHERE name='Model sender') + (SELECT COUNT(*) FROM tags WHERE name='model-tag')`).Scan(&vocabulary); err != nil {
				t.Fatal(err)
			}
			if title != "Original" || language != "" || correspondent != 0 || vocabulary != 0 {
				t.Fatalf("inference mutated metadata: title=%q language=%q correspondent=%d vocabulary=%d", title, language, correspondent, vocabulary)
			}
			var proposals, explained int
			reason := "review_first"
			if score < 0.7 {
				reason = "low_confidence"
			}
			if err := d.Read.QueryRow(`SELECT COUNT(*), COUNT(*) FILTER (WHERE json_extract(vars_json, '$.reason')=?) FROM approval_runs WHERE doc_id=? AND state='running'`, reason, docID).Scan(&proposals, &explained); err != nil {
				t.Fatal(err)
			}
			if proposals != 4 || explained != 4 {
				t.Fatalf("proposals=%d explained=%d want four review suggestions", proposals, explained)
			}
		})
	}
}

func setHandlerAutoApply(t *testing.T, d *db.DB, enabled bool) {
	t.Helper()
	if err := settings.Set(context.Background(), d, settings.KeyClassificationAutoApply, enabled); err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticMetadataUsesThresholdWithoutLockingLanguage(t *testing.T) {
	ctx := context.Background()
	d, docID := openHandlerDocument(t, "Original", "Due 2026-08-06; issued 2026-08-01")
	var categoryID int64
	var categoryCode int
	if err := d.Read.QueryRow(`SELECT id,code FROM jd_categories WHERE system_id=1 AND system=0 ORDER BY code LIMIT 1`).Scan(&categoryID, &categoryCode); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(Result{
			Title: "Model title", Correspondent: "Model sender", JDCategory: categoryCode,
			Tags: []string{"first-tag", "second-tag"}, Language: "de", Confidence: 0.7,
			Dates: []DateCandidate{
				{Role: "due", Value: "2026-08-06", Precision: "day", RawText: "2026-08-06", Evidence: "Due 2026-08-06", Confidence: 0.7},
				{Role: "issued", Value: "2026-08-01", Precision: "day", RawText: "2026-08-01", Evidence: "issued 2026-08-01", Confidence: 0.69},
			},
		})
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(body)}}}})
	}))
	defer server.Close()
	p, err := New(Config{EndpointURL: server.URL, Model: "test", ConfidenceThreshold: 0.7}, silentLog())
	if err != nil {
		t.Fatal(err)
	}
	if err := NewHandler(p, d, silentLog()).Handle(ctx, pluginapi.Event{SystemID: 1, Kind: Kind, DocID: docID}); err != nil {
		t.Fatal(err)
	}
	var title, language, correspondent string
	var category int64
	var locked bool
	if err := d.Read.QueryRow(`SELECT d.title,d.languages,d.languages_locked,c.name,d.jd_category_id
		FROM documents d JOIN correspondents c ON c.id=d.correspondent_id WHERE d.id=?`, docID).Scan(&title, &language, &locked, &correspondent, &category); err != nil {
		t.Fatal(err)
	}
	if title != "Model title" || lang.Primary(language) != "de" || locked || correspondent != "Model sender" || category != categoryID {
		t.Fatalf("automatic metadata title=%q language=%q locked=%t correspondent=%q category=%d", title, language, locked, correspondent, category)
	}
	var tags, proposals, accepted, pending, fabricatedReviews int
	if err := d.Read.QueryRow(`SELECT
		(SELECT COUNT(*) FROM document_tags dt JOIN tags t ON t.id=dt.tag_id WHERE dt.document_id=? AND t.name IN ('first-tag','second-tag')),
		(SELECT COUNT(*) FROM approval_runs WHERE doc_id=?),
		(SELECT COUNT(*) FROM document_intelligence WHERE document_id=? AND status='accepted' AND sort_value='2026-08-06' AND gate_policy_version=?),
		(SELECT COUNT(*) FROM document_intelligence WHERE document_id=? AND status='pending' AND sort_value='2026-08-01'),
		(SELECT COUNT(*) FROM document_intelligence WHERE document_id=? AND (reviewed_by IS NOT NULL OR reviewed_at IS NOT NULL))`,
		docID, docID, docID, approvals.AutomaticPolicyVersion, docID, docID).Scan(&tags, &proposals, &accepted, &pending, &fabricatedReviews); err != nil {
		t.Fatal(err)
	}
	if tags != 2 || proposals != 0 || accepted != 1 || pending != 1 || fabricatedReviews != 0 {
		t.Fatalf("tags=%d proposals=%d accepted=%d pending=%d fabricated reviews=%d", tags, proposals, accepted, pending, fabricatedReviews)
	}
}

func TestApplicationModeDisabledDuringProviderCallQueuesReview(t *testing.T) {
	ctx := context.Background()
	d, docID := openHandlerDocument(t, "Original", "Due 2026-08-06")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := settings.Set(ctx, d, settings.KeyClassificationAutoApply, false); err != nil {
			t.Error(err)
			return
		}
		body, _ := json.Marshal(Result{
			Title: "Model title", Tags: []string{"model-tag"}, Confidence: 1,
			Dates: []DateCandidate{{Role: "due", Value: "2026-08-06", Precision: "day", RawText: "2026-08-06", Evidence: "Due 2026-08-06", Confidence: 1}},
		})
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(body)}}}})
	}))
	defer server.Close()
	p, err := New(Config{EndpointURL: server.URL, Model: "test"}, silentLog())
	if err != nil {
		t.Fatal(err)
	}
	if err := NewHandler(p, d, silentLog()).Handle(ctx, pluginapi.Event{SystemID: 1, Kind: Kind, DocID: docID}); err != nil {
		t.Fatal(err)
	}
	var title, status string
	var proposals, tags, reviewed int
	if err := d.Read.QueryRow(`SELECT title,
		(SELECT status FROM document_intelligence WHERE document_id=documents.id),
		(SELECT COUNT(*) FROM approval_runs WHERE doc_id=documents.id AND state='running'),
		(SELECT COUNT(*) FROM document_tags dt JOIN tags t ON t.id=dt.tag_id WHERE dt.document_id=documents.id AND t.name='model-tag'),
		(SELECT COUNT(*) FROM document_intelligence WHERE document_id=documents.id AND (reviewed_by IS NOT NULL OR reviewed_at IS NOT NULL))
		FROM documents WHERE id=?`, docID).Scan(&title, &status, &proposals, &tags, &reviewed); err != nil {
		t.Fatal(err)
	}
	if title != "Original" || status != "pending" || proposals != 2 || tags != 0 || reviewed != 0 {
		t.Fatalf("stale application mode: title=%q date=%q proposals=%d tags=%d reviews=%d", title, status, proposals, tags, reviewed)
	}
}

func TestAutomaticDatesOnlyClearClassifierOwnedResolvedReviewMarkers(t *testing.T) {
	for _, tc := range []struct {
		name          string
		humanOwned    bool
		reassert      bool
		modelReview   bool
		lowScore      bool
		pendingDate   bool
		pendingReview bool
		autoTags      bool
		wantMarker    int
	}{
		{name: "automatic date", wantMarker: 0},
		{name: "own tag mutations", autoTags: true, wantMarker: 0},
		{name: "human owned", humanOwned: true, wantMarker: 1},
		{name: "human tag reassertion", reassert: true, wantMarker: 1},
		{name: "model requested review", modelReview: true, wantMarker: 1},
		{name: "low overall confidence", lowScore: true, wantMarker: 1},
		{name: "pending date", pendingDate: true, wantMarker: 1},
		{name: "outstanding metadata review", pendingReview: true, wantMarker: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			d, docID := openHandlerDocument(t, "Original", "Due 2026-08-06")
			if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
				_, err := upsertTagAndAttach(ctx, tx, "needs-review", docID, 0, !tc.humanOwned)
				if err != nil {
					return err
				}
				if tc.reassert {
					if _, err := upsertTagAndAttach(ctx, tx, "human-tag", docID, 0, false); err != nil {
						return err
					}
				}
				if tc.pendingReview {
					baseline, err := documentstate.Load(ctx, tx, docID)
					if err != nil {
						return err
					}
					return approvals.ProposeDocumentChangeInTx(ctx, tx, docID, approvals.DocumentChange{
						Field: "title", Value: "Waiting title", Confidence: 0.2, Source: "llm", Baseline: &baseline,
					})
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.reassert {
					if _, err := d.Write.Exec(`UPDATE document_tags SET classifier_owned=0 WHERE document_id=? AND tag_id IN (SELECT id FROM tags WHERE name='human-tag')`, docID); err != nil {
						t.Error(err)
						return
					}
				}
				result := Result{Confidence: 1, Dates: []DateCandidate{{Role: "due", Value: "2026-08-06", Precision: "day", RawText: "2026-08-06", Evidence: "Due 2026-08-06", Confidence: 1}}}
				if tc.modelReview {
					result.Tags = []string{"needs-review"}
				}
				if tc.autoTags {
					result.Tags = []string{"first-tag", "second-tag"}
				}
				if tc.lowScore {
					result.Confidence = 0.69
				}
				if tc.pendingDate {
					result.Dates[0].Confidence = 0.69
				}
				body, _ := json.Marshal(result)
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(body)}}}})
			}))
			defer server.Close()
			p, err := New(Config{EndpointURL: server.URL, Model: "test", ConfidenceThreshold: 0.7}, silentLog())
			if err != nil {
				t.Fatal(err)
			}
			if err := NewHandler(p, d, silentLog()).Handle(ctx, pluginapi.Event{SystemID: 1, Kind: Kind, DocID: docID}); err != nil {
				t.Fatal(err)
			}
			var markers int
			var status string
			if err := d.Read.QueryRow(`SELECT
				(SELECT COUNT(*) FROM document_tags dt JOIN tags t ON t.id=dt.tag_id WHERE dt.document_id=? AND t.slug='needs-review'),
				(SELECT status FROM document_intelligence WHERE document_id=?)`, docID, docID).Scan(&markers, &status); err != nil {
				t.Fatal(err)
			}
			wantStatus := "accepted"
			if tc.pendingDate {
				wantStatus = "pending"
			}
			if markers != tc.wantMarker || status != wantStatus {
				t.Fatalf("review markers=%d date=%q want %d/%s", markers, status, tc.wantMarker, wantStatus)
			}
		})
	}
}

func itoaScore(score float64) string {
	if score == 1 {
		return "maximum score"
	}
	return "low score"
}

func TestClassifierDiscardsConfigAndSourceRaces(t *testing.T) {
	for _, race := range []string{"disable", "config", "source", "source ABA"} {
		t.Run(race, func(t *testing.T) {
			ctx := context.Background()
			d, docID := openHandlerDocument(t, "Original", "Due 2026-08-06")
			var p *Plugin
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				switch race {
				case "disable":
					p.Disable()
				case "config":
					cfg := p.Config()
					cfg.ConfidenceThreshold = 0.9
					if err := p.SetConfig(cfg); err != nil {
						t.Error(err)
					}
				case "source", "source ABA":
					if _, err := d.Write.Exec(`UPDATE documents SET content='Changed' WHERE id=?`, docID); err != nil {
						t.Error(err)
					}
					if race == "source ABA" {
						if _, err := d.Write.Exec(`UPDATE documents SET content='Due 2026-08-06' WHERE id=?`, docID); err != nil {
							t.Error(err)
						}
					}
				}
				body, _ := json.Marshal(Result{Title: "Model title", Confidence: 1, Dates: []DateCandidate{{Role: "due", Value: "2026-08-06", Precision: "day", RawText: "2026-08-06", Evidence: "Due 2026-08-06", Confidence: 1}}})
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(body)}}}})
			}))
			defer server.Close()
			var err error
			p, err = New(Config{EndpointURL: server.URL, Model: "test"}, silentLog())
			if err != nil {
				t.Fatal(err)
			}
			if err := NewHandler(p, d, silentLog()).Handle(ctx, pluginapi.Event{SystemID: 1, Kind: Kind, DocID: docID}); err == nil {
				t.Fatal("stale request succeeded")
			}
			var title string
			var proposals, facts int
			if err := d.Read.QueryRow(`SELECT title, (SELECT COUNT(*) FROM approval_runs WHERE doc_id=documents.id), (SELECT COUNT(*) FROM document_intelligence WHERE document_id=documents.id) FROM documents WHERE id=?`, docID).Scan(&title, &proposals, &facts); err != nil {
				t.Fatal(err)
			}
			if title != "Original" || proposals != 0 || facts != 0 {
				t.Fatalf("stale work persisted: title=%q proposals=%d facts=%d", title, proposals, facts)
			}
		})
	}
}

func TestClassifierHumanReassertionsWinDuringRequest(t *testing.T) {
	ctx := context.Background()
	d, docID := openHandlerDocument(t, "Original", "Invoice")
	if _, err := d.Write.Exec(`INSERT INTO tags(system_id,id,name,slug,created_at,updated_at) VALUES(1,99,'human','human',0,0); INSERT INTO document_tags(document_id,tag_id) VALUES(?,99)`, docID); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := d.Write.Exec(`UPDATE documents SET title=title, languages='en', languages_locked=1 WHERE id=?; UPDATE document_tags SET classifier_owned=0 WHERE document_id=?`, docID, docID); err != nil {
			t.Error(err)
		}
		body, _ := json.Marshal(Result{Title: "Model title", Language: "de", Tags: []string{"model-tag"}, Confidence: 1})
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(body)}}}})
	}))
	defer server.Close()
	p, err := New(Config{EndpointURL: server.URL, Model: "test"}, silentLog())
	if err != nil {
		t.Fatal(err)
	}
	if err := NewHandler(p, d, silentLog()).Handle(ctx, pluginapi.Event{SystemID: 1, Kind: Kind, DocID: docID}); err != nil {
		t.Fatal(err)
	}
	var title, language string
	var proposals, humanTags int
	if err := d.Read.QueryRow(`SELECT title,languages,(SELECT COUNT(*) FROM approval_runs WHERE doc_id=documents.id),(SELECT COUNT(*) FROM document_tags WHERE document_id=documents.id AND tag_id=99 AND classifier_owned=0) FROM documents WHERE id=?`, docID).Scan(&title, &language, &proposals, &humanTags); err != nil {
		t.Fatal(err)
	}
	if title != "Original" || language != "en" || proposals != 0 || humanTags != 1 {
		t.Fatalf("human state lost: %q %q proposals=%d tags=%d", title, language, proposals, humanTags)
	}
}

func TestClassifierRechecksRuntimeAfterWaitingForWriter(t *testing.T) {
	d, docID := openHandlerDocument(t, "Original", "Invoice")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"title\":\"Stale model title\",\"confidence\":1}"}}]}`))
	}))
	defer server.Close()
	p, err := New(Config{EndpointURL: server.URL, Model: "test"}, silentLog())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	held, err := d.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback()
	waitCount := d.Write.Stats().WaitCount
	done := make(chan error, 1)
	go func() {
		done <- NewHandler(p, d, silentLog()).Handle(ctx, pluginapi.Event{SystemID: 1, Kind: Kind, DocID: docID})
	}()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for d.Write.Stats().WaitCount == waitCount {
		select {
		case err := <-done:
			t.Fatalf("handler stopped before waiting for writer: %v", err)
		case <-ctx.Done():
			t.Fatal("handler did not reach writer")
		case <-ticker.C:
		}
	}
	p.Disable()
	if err := held.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("queued stale runtime persisted a proposal")
	}
	var proposals int
	if err := d.Read.QueryRow(`SELECT COUNT(*) FROM approval_runs WHERE doc_id=?`, docID).Scan(&proposals); err != nil {
		t.Fatal(err)
	}
	if proposals != 0 {
		t.Fatalf("disabled runtime persisted %d proposals", proposals)
	}
}

func seedNamingContext(t *testing.T, d *db.DB) {
	t.Helper()
	if _, err := d.Write.Exec(`
		INSERT INTO users(id,email,display_name,role,created_at,updated_at)
		VALUES(2,'naming@example.com','Naming owner','member',0,0);
		INSERT INTO documents(id,system_id,owner_id,original_blob,original_size,title,content,jd_category_id,created_at,updated_at)
		VALUES(20,1,2,'naming-source',1,'Invoice supplier naming example','Invoice supplier',(SELECT inbox_category_id FROM jd_systems WHERE id=1),0,0),
		      (21,1,2,'private-source',1,'Private naming example','Invoice supplier',(SELECT inbox_category_id FROM jd_systems WHERE id=1),0,0);
		INSERT INTO object_acls(object_kind,object_id,principal_kind,principal_id,perm_bits,created_at)
		VALUES('document',20,'user',1,1,0);
	`); err != nil {
		t.Fatal(err)
	}
}

func TestClassifierDiscardsNamingContextRaces(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutation string
		dateOnly bool
	}{
		{name: "title", mutation: `UPDATE documents SET title='Changed naming example' WHERE id=20`},
		{name: "source", mutation: `UPDATE documents SET content='Changed source' WHERE id=20`},
		{name: "trash", mutation: `UPDATE documents SET trashed_at=1 WHERE id=20`},
		{name: "visibility", mutation: `DELETE FROM object_acls WHERE object_kind='document' AND object_id=20`},
		{name: "visibility with date only", mutation: `DELETE FROM object_acls WHERE object_kind='document' AND object_id=20`, dateOnly: true},
		{name: "source owner disabled", mutation: `UPDATE users SET disabled=1 WHERE id=2`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			d, docID := openHandlerDocument(t, "Original invoice", "Invoice supplier Due 2026-08-06")
			seedNamingContext(t, d)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				request, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				if !strings.Contains(string(request), "Invoice supplier naming example") || strings.Contains(string(request), "Private naming example") {
					t.Error("provider did not receive only the authorized naming context")
				}
				if _, err := d.Write.Exec(tc.mutation); err != nil {
					t.Error(err)
					return
				}
				result := Result{Confidence: 1, Dates: []DateCandidate{{Role: "due", Value: "2026-08-06", Precision: "day", RawText: "2026-08-06", Evidence: "Due 2026-08-06", Confidence: 1}}}
				if !tc.dateOnly {
					result.Title = "Model title"
				}
				body, _ := json.Marshal(result)
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(body)}}}})
			}))
			defer server.Close()
			p, err := New(Config{EndpointURL: server.URL, Model: "test"}, silentLog())
			if err != nil {
				t.Fatal(err)
			}
			if err := NewHandler(p, d, silentLog()).Handle(ctx, pluginapi.Event{SystemID: 1, Kind: Kind, DocID: docID}); err == nil {
				t.Fatal("stale naming context persisted a result")
			}
			var title string
			var proposals, facts, markers, version int
			if err := d.Read.QueryRow(`SELECT title,pipeline_version_llm,
				(SELECT COUNT(*) FROM approval_runs WHERE doc_id=documents.id),
				(SELECT COUNT(*) FROM document_intelligence WHERE document_id=documents.id),
				(SELECT COUNT(*) FROM document_tags WHERE document_id=documents.id)
				FROM documents WHERE id=?`, docID).Scan(&title, &version, &proposals, &facts, &markers); err != nil {
				t.Fatal(err)
			}
			if title != "Original invoice" || proposals != 0 || facts != 0 || markers != 0 || version != 0 {
				t.Fatalf("stale result persisted: title=%q proposals=%d facts=%d markers=%d version=%d", title, proposals, facts, markers, version)
			}
		})
	}
}

func TestClassifierNamingProvenanceSurvivesUntilApproval(t *testing.T) {
	for _, afterReview := range []string{"unchanged", "title changed", "visibility revoked", "admin visibility revoked"} {
		t.Run(afterReview, func(t *testing.T) {
			ctx := context.Background()
			d, docID := openHandlerDocument(t, "Original invoice", "Invoice supplier")
			setHandlerAutoApply(t, d, false)
			seedNamingContext(t, d)
			// The owner-level naming visibility ceiling also survives review
			// when both the target owner and reviewer are administrators.
			if afterReview != "admin visibility revoked" {
				if _, err := d.Write.Exec(`UPDATE users SET role='member' WHERE id=1`); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := documentstate.Load(ctx, d.Read, 20)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				request, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				if !strings.Contains(string(request), "Invoice supplier naming example") || strings.Contains(string(request), "Private naming example") {
					t.Error("provider did not receive only the authorized naming context")
				}
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"title\":\"Reviewed invoice\",\"confidence\":1}"}}]}`))
			}))
			defer server.Close()
			p, err := New(Config{EndpointURL: server.URL, Model: "test"}, silentLog())
			if err != nil {
				t.Fatal(err)
			}
			if err := NewHandler(p, d, silentLog()).Handle(ctx, pluginapi.Event{SystemID: 1, Kind: Kind, DocID: docID}); err != nil {
				t.Fatal(err)
			}
			var runID int64
			var raw string
			if err := d.Read.QueryRow(`SELECT id,vars_json FROM approval_runs WHERE doc_id=?`, docID).Scan(&runID, &raw); err != nil {
				t.Fatal(err)
			}
			var proposal approvals.DocumentChange
			if err := json.Unmarshal([]byte(raw), &proposal); err != nil {
				t.Fatal(err)
			}
			if len(proposal.Supporters) != 1 || proposal.Supporters[0] != (documentstate.Reference{DocumentID: 20, Snapshot: snapshot}) || len(proposal.BasedOn) != 1 || proposal.BasedOn[0] != 20 {
				t.Fatalf("proposal lost its checked naming source: %+v", proposal.Supporters)
			}
			engine := approvals.New(d, silentLog())
			if err := engine.Advance(ctx, runID, ""); err != nil {
				t.Fatal(err)
			}
			_, tasks, err := engine.GetRun(ctx, runID)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("review tasks=%v err=%v", tasks, err)
			}
			if _, err := d.Write.Exec(`INSERT INTO sessions(id,user_id,created_at,expires_at,last_seen_at) VALUES('naming-review',1,0,4102444800,0)`); err != nil {
				t.Fatal(err)
			}
			actor := &pluginapi.Principal{Kind: "user", UserID: 1, AuthNBy: "local-auth", SessionID: "naming-review", AuthExpiresAt: 4102444800}
			if err := engine.Resolve(ctx, tasks[0].ID, "apply", actor); err != nil {
				t.Fatal(err)
			}
			if err := engine.Advance(ctx, runID, "apply"); err != nil {
				t.Fatal(err)
			}
			var wantErr error
			switch afterReview {
			case "title changed":
				if _, err := d.Write.Exec(`UPDATE documents SET title='Changed naming example' WHERE id=20`); err != nil {
					t.Fatal(err)
				}
				wantErr = approvals.ErrStaleProposal
			case "visibility revoked", "admin visibility revoked":
				if _, err := d.Write.Exec(`DELETE FROM object_acls WHERE object_kind='document' AND object_id=20`); err != nil {
					t.Fatal(err)
				}
				wantErr = approvals.ErrForbidden
			}
			if err := engine.Advance(ctx, runID, ""); !errors.Is(err, wantErr) {
				t.Fatalf("approval effect error=%v want=%v", err, wantErr)
			}
			wantTitle := "Reviewed invoice"
			if wantErr != nil {
				wantTitle = "Original invoice"
			}
			var title string
			if err := d.Read.QueryRow(`SELECT title FROM documents WHERE id=?`, docID).Scan(&title); err != nil {
				t.Fatal(err)
			}
			if title != wantTitle {
				t.Fatalf("title=%q want=%q", title, wantTitle)
			}
		})
	}
}
