// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	"github.com/johnnybravo-xyz/suchi/core/db"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func workflowSystemsFixture(t *testing.T) (*Server, *db.DB) {
	t.Helper()
	d := openTestDB(t)
	for _, id := range []int64{1, 5, 6} {
		seedUser(t, d, id)
	}
	if _, err := d.Write.ExecContext(context.Background(), `
		UPDATE users SET role = 'member' WHERE id IN (5,6);
		UPDATE jd_systems SET code = 'S01' WHERE id = 1;
		INSERT INTO jd_systems(id,code,name,taxonomy,created_at,updated_at) VALUES (2,'S02','Second','jd',0,0);
		INSERT INTO jd_system_members(system_id,user_id,created_at) VALUES (2,5,0),(2,6,0);
		INSERT INTO jd_areas(system_id,code_start,code_end,name,position) VALUES (1,10,19,'First',0),(2,10,19,'Second',0);
		INSERT INTO jd_categories(id,system_id,area_start,code,name) VALUES (101,1,10,13,'First filing'),(201,2,10,13,'Second filing');
		INSERT INTO documents(id,system_id,owner_id,original_blob,original_size,title,jd_category_id,created_at,updated_at)
		VALUES (101,1,5,'first',1,'First source',101,0,0),(102,1,6,'private-first',1,'Private source',101,0,0),(201,2,6,'second',1,'Private second',201,0,0);
		INSERT INTO custom_fields(id,system_id,name,data_type,created_at,updated_at) VALUES (101,1,'Related','documentlink',0,0),(201,2,'Related','documentlink',0,0);
	`); err != nil {
		t.Fatal(err)
	}
	s := &Server{Actions: testActions(t), DB: d, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)), Authz: authz.ACLAuthorizer{DB: d}}
	return s, d
}

func setWorkflowLink(s *Server, p *pluginapi.Principal, source, field, target int64, query string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPut, "/api/documents/"+strconv.FormatInt(source, 10)+"/custom_fields/"+strconv.FormatInt(field, 10)+query, strings.NewReader(`{"value":`+strconv.FormatInt(target, 10)+`}`))
	r.SetPathValue("id", strconv.FormatInt(source, 10))
	r.SetPathValue("field", strconv.FormatInt(field, 10))
	r = r.WithContext(auth.WithPrincipal(r.Context(), p))
	w := httptest.NewRecorder()
	s.SetCustomField(w, r)
	return w
}

func TestDocumentLinksRequireBothSystemsAndDocumentPermissions(t *testing.T) {
	for _, tc := range []struct {
		name               string
		source, field      int64
		grant, removeEntry bool
		token              bool
		query              string
		want               int
	}{
		{"target ACL required", 101, 101, false, false, false, "", 404},
		{"both permissions", 101, 101, true, false, false, "", 204},
		{"source change required", 102, 101, true, false, false, "", 404},
		{"target membership required", 101, 101, true, true, false, "", 404},
		{"foreign field rejected", 101, 201, true, false, false, "", 404},
		{"explicit source mismatch", 101, 101, true, false, false, "?system=S02", 404},
		{"admin token ceiling", 101, 101, true, false, true, "", 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, d := workflowSystemsFixture(t)
			if tc.grant {
				if _, err := d.Write.Exec(`INSERT INTO object_acls(object_kind,object_id,principal_kind,principal_id,perm_bits,created_at) VALUES ('document',201,'user',5,1,0)`); err != nil {
					t.Fatal(err)
				}
			}
			if tc.removeEntry {
				if _, err := d.Write.Exec(`DELETE FROM jd_system_members WHERE system_id=2 AND user_id=5`); err != nil {
					t.Fatal(err)
				}
			}
			p := memberPrincipal(5)
			if tc.token {
				p = adminPrincipal(1)
				p.Kind = "token"
				p.TokenSystemID = 1
				p.Scopes = []string{auth.ScopeDocumentsWrite}
			}
			w := setWorkflowLink(s, p, tc.source, tc.field, 201, tc.query)
			if w.Code != tc.want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			var count int
			if err := d.Read.QueryRow(`SELECT COUNT(*) FROM document_custom_field_values WHERE document_id=?`, tc.source).Scan(&count); err != nil {
				t.Fatal(err)
			}
			wantCount := 0
			if tc.want == 204 {
				wantCount = 1
			}
			if count != wantCount {
				t.Fatalf("stored links=%d want=%d", count, wantCount)
			}
		})
	}
}

type pausedWorkflowAuthorizer struct {
	base             authz.Authorizer
	once             sync.Once
	entered, release chan struct{}
}

func (p *pausedWorkflowAuthorizer) Can(ctx context.Context, actor authz.Principal, kind authz.Kind, id int64, want authz.Perm) error {
	err := p.base.Can(ctx, actor, kind, id, want)
	if id == 101 && want == authz.PermChange {
		p.once.Do(func() { close(p.entered); <-p.release })
	}
	return err
}

func TestDocumentLinkWriterRejectsRemovedAndReadmittedToken(t *testing.T) {
	s, d := workflowSystemsFixture(t)
	if _, err := d.Write.Exec(`INSERT INTO api_tokens(id,user_id,system_id,name,token_hash,scopes,created_at) VALUES (1,5,1,'Device','workflow-token','documents:write',0)`); err != nil {
		t.Fatal(err)
	}
	pause := &pausedWorkflowAuthorizer{base: s.Authz, entered: make(chan struct{}), release: make(chan struct{})}
	s.Authz = pause
	p := memberPrincipal(5)
	p.Kind = "token"
	p.TokenID = 1
	p.TokenSystemID = 1
	p.Scopes = []string{auth.ScopeDocumentsWrite}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- setWorkflowLink(s, p, 101, 101, 101, "") }()
	select {
	case <-pause.entered:
	case w := <-done:
		t.Fatalf("request ended before writer barrier: %d %s", w.Code, w.Body.String())
	}
	err := d.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			DELETE FROM jd_system_members WHERE system_id=1 AND user_id=5;
			INSERT INTO jd_system_members(system_id,user_id,created_at) VALUES (1,5,1);
			INSERT INTO api_tokens(user_id,system_id,name,token_hash,scopes,created_at)
			VALUES (5,1,'Replacement device','replacement-workflow-token','documents:read',1);
		`)
		return err
	})
	close(pause.release)
	w := <-done
	if err != nil {
		t.Fatal(err)
	}
	if w.Code != 404 {
		t.Fatalf("old token write status=%d body=%s", w.Code, w.Body.String())
	}
	var count int
	if err := d.Read.QueryRow(`SELECT COUNT(*) FROM document_custom_field_values WHERE document_id=101`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("revoked token wrote a document link")
	}
}

func TestApprovalTaskPagesAndCountsRespectSystemAndACL(t *testing.T) {
	s, d := workflowSystemsFixture(t)
	if _, err := d.Write.Exec(`
		INSERT INTO approval_defs(id,system_id,slug,version,spec_json,active,created_at) VALUES (101,1,'review',1,'{}',1,0),(201,2,'review',1,'{}',1,0);
		INSERT INTO approval_runs(id,system_id,def_id,doc_id,state,current_state,vars_json,state_entered_at,started_at)
		VALUES (101,1,101,101,'running','review','{}',0,0),(102,1,101,102,'running','review','{}',0,0),(103,1,101,NULL,'running','review','{}',0,0),(201,2,201,201,'running','review','{}',0,0);
		INSERT INTO approval_tasks(id,run_id,state_key,assignee,prompt,choices_json,status,created_at)
		VALUES (101,101,'review','user:5','First','["approve"]','open',1),(102,102,'review','user:5','Private','["approve"]','open',99),(103,103,'review','user:5','Documentless','["approve"]','open',2),(201,201,'review','user:5','Foreign','["approve"]','open',100);
		DELETE FROM jd_system_members WHERE system_id=2 AND user_id=5;
		INSERT INTO object_acls(object_kind,object_id,principal_kind,principal_id,perm_bits,created_at) VALUES ('document',201,'user',5,7,0);
	`); err != nil {
		t.Fatal(err)
	}
	request := func(query string, p *pluginapi.Principal) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/tasks/?include=approvals&limit=1"+query, nil)
		r = r.WithContext(auth.WithPrincipal(r.Context(), p))
		w := httptest.NewRecorder()
		s.ListTasks(w, r)
		return w
	}
	w := request("&system=S01", memberPrincipal(5))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var response TasksResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Counts["approvals_open"] != 2 || len(response.ApprovalTasks) != 1 || response.ApprovalTasks[0].ID != 103 {
		t.Fatalf("scoped page=%+v", response)
	}
	for _, query := range []string{"&system=S02", "&system=unknown", "&system=0", "&system=S01&system=S02"} {
		w = request(query, memberPrincipal(5))
		if w.Code != 404 {
			t.Fatalf("%s status=%d body=%s", query, w.Code, w.Body.String())
		}
	}
	p := adminPrincipal(1)
	p.Kind = "token"
	p.TokenSystemID = 1
	p.Scopes = []string{auth.ScopeDocumentsRead}
	w = request("&system=S02", p)
	if w.Code != 404 {
		t.Fatalf("token escaped system: %d %s", w.Code, w.Body.String())
	}
	if _, err := d.Write.Exec(`UPDATE users SET disabled=1 WHERE id=5`); err != nil {
		t.Fatal(err)
	}
	w = request("&system=S01", memberPrincipal(5))
	if w.Code != 404 {
		t.Fatalf("disabled actor status=%d", w.Code)
	}
}
