package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Table-driven test for NormalizeAPITrailingSlash. Registers a mux
// with the same mix of patterns suchi has today — some collection
// routes end in `/`, some item routes don't — and verifies both slash
// variants land on the same handler for item routes, without breaking
// collection routes or letting stray tails leak into a subtree match.
func TestNormalizeAPITrailingSlash(t *testing.T) {
	mux := http.NewServeMux()
	hits := map[string]int{}
	handler := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			hits[name]++
			w.WriteHeader(http.StatusOK)
		}
	}
	// A representative sample of the real routes.
	mux.HandleFunc("GET /api/tasks/", handler("tasks_list"))
	mux.HandleFunc("GET /api/documents/{id}", handler("doc_get"))
	mux.HandleFunc("POST /api/documents/{id}/versions/", handler("versions_list"))
	mux.HandleFunc("POST /api/tasks/{id}/claim", handler("task_claim"))
	// A non-API route so we can prove the middleware doesn't touch it.
	mux.HandleFunc("GET /login", handler("login"))
	// UI catch-all — the real suchi mux registers `GET /` for the doc
	// list. Without special-casing, mux.Handler for `/api/foo/N/`
	// would return this pattern (non-empty) and the middleware would
	// happily let it swallow API requests. Registered here so the
	// tests below exercise that path.
	mux.HandleFunc("GET /", handler("ui_root"))

	router := NormalizeAPITrailingSlash(mux)
	srv := httptest.NewServer(router)
	defer srv.Close()

	cases := []struct {
		name    string
		method  string
		path    string
		want    int
		hitName string
	}{
		// Collection route registered with trailing slash — both forms
		// work already thanks to Go 1.22 subtree matching.
		{"collection with slash", "GET", "/api/tasks/", 200, "tasks_list"},

		// Item route registered without slash — bare form works
		// unchanged.
		{"item no-slash native", "GET", "/api/documents/1", 200, "doc_get"},
		// ...and the trailing-slash form now works too via the retry.
		{"item with-slash normalized", "GET", "/api/documents/1/", 200, "doc_get"},

		// Nested collection registered with slash — subtree matcher
		// covers both forms in stdlib.
		{"nested collection with slash", "POST", "/api/documents/1/versions/", 200, "versions_list"},

		// Sub-action route without slash — bare form works.
		{"sub-action no-slash", "POST", "/api/tasks/9/claim", 200, "task_claim"},
		// ...and the trailing-slash form is normalized to it.
		{"sub-action with-slash normalized", "POST", "/api/tasks/9/claim/", 200, "task_claim"},

		// Middleware only touches /api/; non-api routes pass through.
		{"non-api untouched", "GET", "/login", 200, "login"},

		// Stray tails aren't rerouted into more specific handlers by
		// the middleware — the catch-all UI handler picks them up,
		// same as it does without the middleware. Not the middleware's
		// job to make random paths 404.
		{"random api tail falls to catch-all", "GET", "/api/documents/1/garbage/", 200, "ui_root"},
		{"random api leaf falls to catch-all", "GET", "/api/does-not-exist", 200, "ui_root"},
	}

	client := srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits = map[string]int{}
			req, _ := http.NewRequest(tc.method, srv.URL+tc.path, nil)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status: got %d, want %d (hits=%v)", resp.StatusCode, tc.want, hits)
			}
			if tc.hitName != "" && hits[tc.hitName] != 1 {
				t.Errorf("handler %q hit %d times, want 1 (all hits=%v)",
					tc.hitName, hits[tc.hitName], hits)
			}
		})
	}
}
