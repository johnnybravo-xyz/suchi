package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/httpx"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
	localauth "github.com/johnnybravo-xyz/suchi/plugins/local-auth"
)

func TestBlobRouteFamiliesEnforceSystemAndDocumentBoundaries(t *testing.T) {
	for _, headless := range []bool{false, true} {
		t.Run(fmt.Sprintf("headless=%v", headless), func(t *testing.T) {
			d := newDemoTestDB(t)
			ctx := t.Context()
			log := testLogger()
			_, err := d.ExecWrite(ctx, `INSERT INTO users(id,email,display_name,role,created_at,updated_at) VALUES(2,'anita@test','Anita','member',0,0),(3,'partner@test','Partner','member',0,0);
  UPDATE jd_systems SET code='S01',name='Audit' WHERE id=1;
  INSERT INTO jd_systems(id,code,name,taxonomy,created_at,updated_at) VALUES(2,'S02','Advisory','jd',0,0);
  INSERT INTO jd_system_members(system_id,user_id,created_at) VALUES(2,3,0);
  INSERT INTO jd_areas(system_id,code_start,code_end,name,position) VALUES(1,10,19,'Records',0),(2,10,19,'Records',0);
  INSERT INTO jd_categories(system_id,id,area_start,code,name,system) VALUES(1,1,10,13,'Tax',0),(2,2,10,13,'Tax',0);`)
			if err != nil {
				t.Fatal(err)
			}
			cas, err := blob.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ref, err := cas.Put(strings.NewReader("%PDF-1.4\nSYSTEM_BLOB_MARKER"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = d.ExecWrite(ctx, `INSERT INTO documents(system_id,id,owner_id,original_blob,original_size,title,mime_type,jd_category_id,created_at,updated_at) VALUES(1,1,2,?,32,'Audit file','application/pdf',1,0,0),(2,2,2,?,32,'Advisory file','application/pdf',2,0,0);
  INSERT INTO object_acls(object_kind,object_id,principal_kind,principal_id,perm_bits,created_at,created_by) VALUES('document',2,'user',3,1,0,1),('document',2,'user',2,1,0,1);`, ref.SHA256, ref.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			local, err := localauth.New(ctx, d, log, false, false)
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{UIDisabled: headless, BodyLimit: 1024}
			mux := http.NewServeMux()
			if err := registerBaseRoutes(mux, cfg, d, cas, httpx.NewMetrics(), local, nil, 3, log); err != nil {
				t.Fatal(err)
			}
			handler := buildHTTPHandler(mux, cfg, &auth.Chain{}, nil, httpx.NewMetrics(), log)
			for _, endpoint := range []string{"/preview/%d", "/download/%d", "/api/documents/%d/preview", "/api/documents/%d/download"} {
				for _, tc := range []struct {
					name       string
					user       int64
					role, kind string
					bound      int64
					doc        int
					query      string
					status     int
				}{
					{"intrinsic owner", 2, "member", "user", 0, 1, "", 200},
					{"owner and direct grant do not admit foreign system", 2, "member", "user", 0, 2, "", 404},
					{"partner both member and ACL", 3, "member", "user", 0, 2, "", 200},
					{"partner membership without ACL", 3, "member", "user", 0, 1, "", 404},
					{"session admin intrinsic", 1, "admin", "user", 0, 2, "", 200},
					{"session admin explicit mismatch", 1, "admin", "user", 0, 2, "?system=S01", 404},
					{"bound admin token intrinsic mismatch", 1, "admin", "token", 1, 2, "", 404},
					{"bound admin token explicit escape", 1, "admin", "token", 1, 2, "?system=S02", 404},
					{"bound admin token own system", 1, "admin", "token", 2, 2, "?system=S02", 200},
					{"external zero binding remains original", 1, "admin", "token", 0, 2, "", 404},
					{"stale explicit system", 1, "admin", "user", 0, 1, "?system=S99", 404},
					{"malformed explicit system", 1, "admin", "user", 0, 1, "?system=s01", 404},
					{"missing object admin", 1, "admin", "user", 0, 999, "", 404},
				} {
					t.Run(endpoint+"/"+tc.name, func(t *testing.T) {
						req := httptest.NewRequest("GET", fmt.Sprintf(endpoint, tc.doc)+tc.query, nil)
						req = req.WithContext(auth.WithPrincipal(ctx, &pluginapi.Principal{UserID: tc.user, Role: tc.role, Kind: tc.kind, TokenSystemID: tc.bound, Scopes: []string{auth.ScopeDocumentsRead}}))
						rec := httptest.NewRecorder()
						handler.ServeHTTP(rec, req)
						if rec.Code != tc.status {
							t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.status, rec.Body.String())
						}
						if strings.Contains(rec.Body.String(), "SYSTEM_BLOB_MARKER") != (tc.status == 200) {
							t.Fatal("incorrect blob disclosure", rec.Body.String())
						}
					})
				}
			}
			// A membership loss invalidates an otherwise valid document ACL on every mirror.
			if _, err := d.ExecWrite(ctx, `DELETE FROM jd_system_members WHERE user_id=3 AND system_id=2`); err != nil {
				t.Fatal(err)
			}
			for _, endpoint := range []string{"/preview/2", "/download/2", "/api/documents/2/preview", "/api/documents/2/download"} {
				req := httptest.NewRequest("GET", endpoint, nil)
				req = req.WithContext(auth.WithPrincipal(ctx, &pluginapi.Principal{UserID: 3, Role: "member", Kind: "user"}))
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != 404 || strings.Contains(rec.Body.String(), "SYSTEM_BLOB_MARKER") {
					t.Fatalf("revoked membership leaked: %d %s", rec.Code, rec.Body.String())
				}
			}
			if _, err := d.ExecWrite(ctx, `UPDATE users SET disabled=1 WHERE id=2`); err != nil {
				t.Fatal(err)
			}
			for _, endpoint := range []string{"/preview/1", "/download/1", "/api/documents/1/preview", "/api/documents/1/download"} {
				req := httptest.NewRequest("GET", endpoint, nil)
				req = req.WithContext(auth.WithPrincipal(ctx, &pluginapi.Principal{UserID: 2, Role: "member", Kind: "user"}))
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != 404 || strings.Contains(rec.Body.String(), "SYSTEM_BLOB_MARKER") {
					t.Fatalf("disabled owner received bytes: %d %s", rec.Code, rec.Body.String())
				}
			}
		})
	}
}
