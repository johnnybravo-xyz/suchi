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

func TestClassifierOffersAndWritesOnlyTheDocumentSystem(t *testing.T) {
	ctx := context.Background()
	d, first := openHandlerDocument(t, "Invoice", "invoice supplier payment total 120")
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
	var categorySystem, correspondentSystem, tagSystem int64
	if err := d.Read.QueryRow(`SELECT c.system_id,co.system_id,t.system_id FROM documents d JOIN jd_categories c ON c.id=d.jd_category_id JOIN correspondents co ON co.id=d.correspondent_id JOIN document_tags dt ON dt.document_id=d.id JOIN tags t ON t.id=dt.tag_id WHERE d.id=?`, first).Scan(&categorySystem, &correspondentSystem, &tagSystem); err != nil {
		t.Fatal(err)
	}
	if categorySystem != 1 || correspondentSystem != 1 || tagSystem != 1 {
		t.Fatalf("foreign metadata category=%d correspondent=%d tag=%d", categorySystem, correspondentSystem, tagSystem)
	}
	if err := h.Handle(ctx, pluginapi.Event{Kind: Kind, DocID: 900, SystemID: 2}); err != nil {
		t.Fatal(err)
	}
	prompt = <-requests
	if !strings.Contains(prompt, "SecondOnlyCategory") || strings.Contains(prompt, "FirstOnlyCategory") {
		t.Fatalf("wrong second-system offerings: %s", prompt)
	}
	var categoryID, correspondentID, tagID int64
	if err := d.Read.QueryRow(`SELECT d.jd_category_id,d.correspondent_id,dt.tag_id FROM documents d JOIN document_tags dt ON dt.document_id=d.id WHERE d.id=900`).Scan(&categoryID, &correspondentID, &tagID); err != nil {
		t.Fatal(err)
	}
	if categoryID != 900 || correspondentID != 900 || tagID != 900 {
		t.Fatalf("S02 metadata category=%d correspondent=%d tag=%d", categoryID, correspondentID, tagID)
	}
	if err := h.Handle(ctx, pluginapi.Event{Kind: Kind, DocID: first, SystemID: 2}); err == nil {
		t.Fatal("accepted mismatched event namespace")
	}
}
