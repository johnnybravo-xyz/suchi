package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizeAPITrailingSlash(t *testing.T) {
	mux := http.NewServeMux()
	hits := map[string]int{}
	handler := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			hits[name]++
			w.WriteHeader(http.StatusOK)
		}
	}
	mux.HandleFunc("GET /api/tasks/", handler("tasks_list"))
	mux.HandleFunc("GET /api/documents/", handler("documents_list"))
	mux.HandleFunc("POST /api/documents/", handler("documents_upload"))
	mux.HandleFunc("GET /api/documents/{id}", handler("doc_get"))
	mux.HandleFunc("POST /api/documents/{id}/versions/", handler("versions_list"))
	mux.HandleFunc("POST /api/tasks/{id}/claim", handler("task_claim"))
	mux.HandleFunc("GET /login", handler("login"))
	mux.HandleFunc("GET /", handler("ui_root"))

	router := NormalizeAPITrailingSlash(mux)

	cases := []struct {
		name    string
		method  string
		path    string
		want    int
		hitName string
	}{
		{"collection with slash", "GET", "/api/tasks/", 200, "tasks_list"},
		{"documents collection", "GET", "/api/documents/", 200, "documents_list"},
		{"collection without slash", "POST", "/api/documents", 200, "documents_upload"},
		{"item no-slash native", "GET", "/api/documents/1", 200, "doc_get"},
		{"item with-slash normalized", "GET", "/api/documents/1/", 200, "doc_get"},
		{"nested collection with slash", "POST", "/api/documents/1/versions/", 200, "versions_list"},
		{"sub-action no-slash", "POST", "/api/tasks/9/claim", 200, "task_claim"},
		{"sub-action with-slash normalized", "POST", "/api/tasks/9/claim/", 200, "task_claim"},
		{"non-api untouched", "GET", "/login", 200, "login"},
		{"random API tail is rejected", "GET", "/api/documents/1/garbage/", 404, ""},
		{"random API leaf is rejected", "GET", "/api/does-not-exist", 404, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits = map[string]int{}
			req := httptest.NewRequest(tc.method, tc.path, nil)
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)
			if res.Code != tc.want {
				t.Errorf("status: got %d, want %d (hits=%v)", res.Code, tc.want, hits)
			}
			if tc.hitName != "" && hits[tc.hitName] != 1 {
				t.Errorf("handler %q hit %d times, want 1 (all hits=%v)",
					tc.hitName, hits[tc.hitName], hits)
			}
		})
	}
}
