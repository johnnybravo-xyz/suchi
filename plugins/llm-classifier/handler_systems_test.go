package llmclassifier

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestClassifierOffersOnlyDocumentSystemWithoutCreatingVocabulary(t *testing.T) {
	ctx := context.Background()
	d, first := openHandlerDocument(t, "Invoice", "invoice supplier payment total 120")
	setHandlerAutoApply(t, d, false)
	if _, err := d.Write.Exec(`
		UPDATE jd_systems SET code='S01' WHERE id=1;
		INSERT OR IGNORE INTO jd_areas(system_id,code_start,code_end,name,position) VALUES (1,10,19,'First',0);
		INSERT INTO jd_categories(id,system_id,area_start,code,name) VALUES (899,1,10,13,'FirstOnlyCategory') ON CONFLICT(system_id,code) DO UPDATE SET name='FirstOnlyCategory';
		INSERT INTO jd_systems(id,code,name,taxonomy,created_at,updated_at) VALUES (2,'S02','Second','jd',0,0);
		INSERT INTO jd_areas(system_id,code_start,code_end,name,position) VALUES (2,10,19,'Second',0),(2,40,49,'System',1);
		INSERT INTO jd_categories(id,system_id,area_start,code,name,system) VALUES (900,2,10,13,'SecondOnlyCategory',0),(901,2,40,49,'Inbox',1);
		UPDATE jd_systems SET inbox_category_id=901 WHERE id=2;
		INSERT INTO documents(id,system_id,owner_id,original_blob,original_size,title,content,jd_category_id,created_at,updated_at)
		VALUES (900,2,1,'foreign-invoice',1,'ForeignEvidenceTitle','invoice supplier payment total 120',901,0,0);
		INSERT INTO tags(id,system_id,name,slug,created_at,updated_at) VALUES (900,2,'shared','shared',0,0);
		INSERT INTO correspondents(id,system_id,name,slug,created_at,updated_at) VALUES (900,2,'shared','shared',0,0);
	`); err != nil {
		t.Fatal(err)
	}
	requests := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		requests <- string(body)
		content := `{"correspondent":"shared","tags":["shared"],"jd_category":13,"confidence":0.99}`
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
	}))
	defer srv.Close()
	p, err := New(Config{EndpointURL: srv.URL, Model: "test"}, silentLog())
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(p, d, silentLog())
	if err := h.Handle(ctx, pluginapi.Event{Kind: Kind, DocID: first, SystemID: 1}); err != nil {
		t.Fatal(err)
	}
	prompt := <-requests
	if !strings.Contains(prompt, "FirstOnlyCategory") || strings.Contains(prompt, "SecondOnlyCategory") || strings.Contains(prompt, "ForeignEvidenceTitle") {
		t.Fatalf("foreign prompt context: %s", prompt)
	}
	var categorySystem, guessedVocabulary int64
	if err := d.Read.QueryRow(`SELECT c.system_id, (SELECT COUNT(*) FROM correspondents WHERE system_id=1 AND name='shared') + (SELECT COUNT(*) FROM tags WHERE system_id=1 AND name='shared') FROM documents d JOIN jd_categories c ON c.id=d.jd_category_id WHERE d.id=?`, first).Scan(&categorySystem, &guessedVocabulary); err != nil {
		t.Fatal(err)
	}
	if categorySystem != 1 || guessedVocabulary != 0 {
		t.Fatalf("system=%d unreviewed vocabulary=%d", categorySystem, guessedVocabulary)
	}
	if err := h.Handle(ctx, pluginapi.Event{Kind: Kind, DocID: 900, SystemID: 2}); err != nil {
		t.Fatal(err)
	}
	prompt = <-requests
	if !strings.Contains(prompt, "SecondOnlyCategory") || strings.Contains(prompt, "FirstOnlyCategory") {
		t.Fatalf("wrong second-system offerings: %s", prompt)
	}
	var categoryID, correspondentID, inferredTags int64
	if err := d.Read.QueryRow(`SELECT d.jd_category_id,COALESCE(d.correspondent_id,0),(SELECT COUNT(*) FROM document_tags WHERE document_id=d.id AND tag_id=900) FROM documents d WHERE d.id=900`).Scan(&categoryID, &correspondentID, &inferredTags); err != nil {
		t.Fatal(err)
	}
	if categoryID != 901 || correspondentID != 0 || inferredTags != 0 {
		t.Fatalf("unreviewed S02 metadata category=%d correspondent=%d tags=%d", categoryID, correspondentID, inferredTags)
	}
	if err := h.Handle(ctx, pluginapi.Event{Kind: Kind, DocID: first, SystemID: 2}); err == nil {
		t.Fatal("accepted mismatched event namespace")
	}
}
