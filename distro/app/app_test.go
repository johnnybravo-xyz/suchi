// SPDX-License-Identifier: AGPL-3.0-or-later

package app_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/automations"
	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	"github.com/johnnybravo-xyz/suchi/core/pipeline/postingest"
	"github.com/johnnybravo-xyz/suchi/core/rescan"
	"github.com/johnnybravo-xyz/suchi/distro/app"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

const configuredJobKind = "assembly-test:configured"

type configuredSubscriber struct {
	handled chan<- pluginapi.Event
}

func (s configuredSubscriber) Kinds() []string { return []string{configuredJobKind} }

func (s configuredSubscriber) Handle(_ context.Context, event pluginapi.Event) error {
	s.handled <- event
	return nil
}

type runningApp struct {
	url    string
	client *http.Client
}

func options(t *testing.T) app.Options {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	dir := t.TempDir()
	return app.Options{Config: config.Config{PublicURL: "http://" + addr, ListenAddr: addr, DataDir: dir, DecryptKeyPath: filepath.Join(dir, "decrypt.key"), DevMode: true, BodyLimit: 1 << 20, OCRLanguages: []string{"eng"}}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), BuildVersion: "assembly-test", BuildRevision: "test-revision"}
}

func startApp(t *testing.T, opts app.Options) runningApp {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, opts) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(20 * time.Second):
			t.Error("app did not stop")
		}
	})
	jar, _ := cookiejar.New(nil)
	a := runningApp{opts.Config.PublicURL, &http.Client{Jar: jar, Timeout: 3 * time.Second}}
	until(t, func() bool {
		resp, err := a.client.Get(a.url + "/readyz")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == 200
	})
	a.request(t, "POST", "/api/login", `{"email":"dev@suchi.local","password":"devdevdev"}`, 204)
	return a
}

func until(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for application state")
}

func (a runningApp) request(t *testing.T, method, path, body string, status int) []byte {
	t.Helper()
	req, err := http.NewRequest(method, a.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != status {
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, resp.StatusCode, status, data)
	}
	return data
}

func TestCompanionV1RoutesReachAssembledHandlers(t *testing.T) {
	running := startApp(t, options(t))

	pairingBody := running.request(t, http.MethodPost, "/api/mobile/pairing", `{"name":"Route test"}`, http.StatusCreated)
	var pairing struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(pairingBody, &pairing); err != nil || pairing.Code == "" {
		t.Fatalf("decode pairing: code=%q err=%v", pairing.Code, err)
	}

	client := &http.Client{Timeout: 3 * time.Second}
	token := ""
	request := func(method, path string, body io.Reader, contentType string, want ...int) []byte {
		t.Helper()
		if body == nil {
			body = http.NoBody
		}
		req, err := http.NewRequest(method, running.url+path, body)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Token "+token)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, status := range want {
			if resp.StatusCode == status {
				return data
			}
		}
		t.Fatalf("%s %s: status=%d want=%v body=%s", method, path, resp.StatusCode, want, data)
		return nil
	}

	handshake := request(http.MethodGet, "/api/handshake", nil, "", http.StatusOK)
	if !bytes.Contains(handshake, []byte(`"suchi-companion-v1"`)) {
		t.Fatalf("handshake does not advertise companion v1: %s", handshake)
	}
	passwordExchange := request(http.MethodPost, "/api/token/",
		strings.NewReader(`{"email":"dev@suchi.local","password":"devdevdev"}`),
		"application/json", http.StatusOK)
	var passwordCredentials struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(passwordExchange, &passwordCredentials); err != nil || len(passwordCredentials.Token) != 64 {
		t.Fatalf("decode password credentials: token length=%d err=%v", len(passwordCredentials.Token), err)
	}
	exchange := request(http.MethodPost, "/api/mobile/pairing/exchange",
		strings.NewReader(fmt.Sprintf(`{"code":%q,"device_name":"Route test"}`, pairing.Code)),
		"application/json", http.StatusOK)
	var credentials struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(exchange, &credentials); err != nil || credentials.Token == "" {
		t.Fatalf("decode credentials: token=%q err=%v", credentials.Token, err)
	}
	token = credentials.Token

	request(http.MethodGet, "/api/whoami", nil, "", http.StatusOK)
	request(http.MethodGet, "/api/documents/?page=1&page_size=50&ordering=-created_at", nil, "", http.StatusOK)
	request(http.MethodGet, "/api/saved_views/?page=1&page_size=100", nil, "", http.StatusOK)
	savedViewBody := request(http.MethodPost, "/api/saved_views/",
		strings.NewReader(`{"name":"Route test","filter_json":"{}","display":"list","position":0,"shared":false}`),
		"application/json", http.StatusCreated)
	var savedView struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(savedViewBody, &savedView); err != nil || savedView.ID <= 0 {
		t.Fatalf("decode saved view: id=%d err=%v", savedView.ID, err)
	}
	request(http.MethodDelete, fmt.Sprintf("/api/saved_views/%d", savedView.ID), nil, "", http.StatusNoContent)

	tagBody := running.request(t, http.MethodPost, "/api/tags/", `{"name":"Route test"}`, http.StatusCreated)
	var tag struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(tagBody, &tag); err != nil || tag.ID <= 0 {
		t.Fatalf("decode tag: id=%d err=%v", tag.ID, err)
	}

	const documentBytes = "Companion route assembly marker."
	var uploadBody bytes.Buffer
	form := multipart.NewWriter(&uploadBody)
	file, err := form.CreateFormFile("document", "companion-route.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, documentBytes); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	uploadRequest, err := http.NewRequest(http.MethodPost, running.url+"/api/documents/", &uploadBody)
	if err != nil {
		t.Fatal(err)
	}
	uploadRequest.Header.Set("Authorization", "Token "+token)
	uploadRequest.Header.Set("Content-Type", form.FormDataContentType())
	uploadRequest.Header.Set("Idempotency-Key", "12345678-1234-4123-8123-123456789abc")
	uploadResponse, err := client.Do(uploadRequest)
	if err != nil {
		t.Fatal(err)
	}
	uploadedBody, readErr := io.ReadAll(uploadResponse.Body)
	_ = uploadResponse.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if uploadResponse.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/documents/: status=%d body=%s", uploadResponse.StatusCode, uploadedBody)
	}
	var uploaded struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(uploadedBody, &uploaded); err != nil || uploaded.ID <= 0 {
		t.Fatalf("decode upload: id=%d err=%v", uploaded.ID, err)
	}
	documentPath := fmt.Sprintf("/api/documents/%d", uploaded.ID)

	request(http.MethodGet, documentPath+"?include_content=0", nil, "", http.StatusOK)
	request(http.MethodGet, documentPath+"?include_content=1", nil, "", http.StatusOK)
	request(http.MethodGet, "/api/search/?q=marker&page=1&page_size=25", nil, "", http.StatusOK)
	request(http.MethodGet, "/api/jd/categories/?page=1&page_size=100", nil, "", http.StatusOK)
	request(http.MethodGet, "/api/tags/?page=1&page_size=500", nil, "", http.StatusOK)
	request(http.MethodPost, "/api/documents/bulk_edit",
		strings.NewReader(fmt.Sprintf(`{"documents":[%d],"method":"add_tag","parameters":{"tag_id":%d}}`, uploaded.ID, tag.ID)),
		"application/json", http.StatusOK)
	request(http.MethodGet, fmt.Sprintf("/api/tasks/?doc_id=%d&kind=post-ingest&include=jobs&limit=50", uploaded.ID), nil, "", http.StatusOK)
	request(http.MethodPatch, documentPath, strings.NewReader(`{"title":"Companion route document"}`), "application/json", http.StatusOK)
	request(http.MethodGet, documentPath+"/preview", nil, "", http.StatusOK)
	downloaded := request(http.MethodGet, documentPath+"/download", nil, "", http.StatusOK)
	if !bytes.Equal(downloaded, []byte(documentBytes)) {
		t.Fatalf("downloaded bytes=%q", downloaded)
	}
	request(http.MethodGet, documentPath+"/thumb?width=160", nil, "", http.StatusNotFound)
	request(http.MethodDelete, documentPath, nil, "", http.StatusNoContent)
	request(http.MethodPost, documentPath+"/restore", nil, "", http.StatusOK)
	request(http.MethodPost, "/api/logout", nil, "", http.StatusNoContent)
	request(http.MethodGet, "/api/whoami", nil, "", http.StatusUnauthorized)
}

func TestExternalAssemblyMigrationsActionsAndCommunityIsolation(t *testing.T) {
	opts := options(t)
	opts.MigrationSets = []db.MigrationSet{{Component: "example", Migrations: []db.Migration{{Version: 1, SQL: `CREATE TABLE example_receipts(doc_id INTEGER PRIMARY KEY REFERENCES documents(id),system_id INTEGER NOT NULL REFERENCES jd_systems(id));`}}}}
	executed := make(chan automations.ActionTarget, 1)
	opts.Actions = []automations.ActionDefinition{{Kind: "example_receipt",
		Validate: func(ctx context.Context, tx *sql.Tx, systemID int64, params map[string]any) error {
			if params["value"] != "ok" {
				return errors.New("value must be ok")
			}
			var count int
			return tx.QueryRowContext(ctx, `SELECT count(*) FROM example_receipts WHERE system_id=?`, systemID).Scan(&count)
		},
		Execute: func(ctx context.Context, tx *sql.Tx, target automations.ActionTarget, params map[string]any) error {
			if _, err := tx.ExecContext(ctx, `INSERT INTO example_receipts VALUES(?,?) ON CONFLICT(doc_id) DO NOTHING`, target.DocID, target.SystemID); err != nil {
				return err
			}
			select {
			case executed <- target:
			default:
			}
			return nil
		},
	}}
	configuredJobs := make(chan pluginapi.Event, 1)
	var configureCalls atomic.Int32
	opts.Configure = func(services *app.Services) error {
		configureCalls.Add(1)
		if services.DB == nil || services.Mux == nil || services.Jobs == nil || services.Approvals == nil || services.Actions == nil || services.Log == nil {
			return errors.New("incomplete assembly services")
		}
		services.Jobs.Register(configuredSubscriber{handled: configuredJobs})
		services.Mux.HandleFunc("POST /api/assembly-test/job", func(w http.ResponseWriter, r *http.Request) {
			err := services.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
				return jobs.Enqueue(r.Context(), tx, configuredJobKind, 0, 0, `{"source":"route"}`)
			})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			services.Jobs.Nudge()
			w.WriteHeader(http.StatusAccepted)
		})
		return nil
	}
	extended := startApp(t, opts)
	communityOpts := options(t)
	communityOpts.Configure = func(services *app.Services) error {
		services.Mux.HandleFunc("POST /api/assembly-test/unhandled", func(w http.ResponseWriter, r *http.Request) {
			err := services.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
				return jobs.Enqueue(r.Context(), tx, configuredJobKind, 0, 0, `{"source":"isolated"}`)
			})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			services.Jobs.Nudge()
			w.WriteHeader(http.StatusAccepted)
		})
		services.Mux.HandleFunc("GET /api/assembly-test/unhandled", func(w http.ResponseWriter, r *http.Request) {
			var state string
			if err := services.DB.Read.QueryRowContext(r.Context(), `SELECT state FROM jobs WHERE kind = ? ORDER BY id DESC LIMIT 1`, configuredJobKind).Scan(&state); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, state)
		})
		return nil
	}
	community := startApp(t, communityOpts)
	if got := configureCalls.Load(); got != 1 {
		t.Fatalf("Configure called %d times, want once", got)
	}
	// Configure routes run through the assembled middleware. A session can use
	// the route, while an API token is denied because the route is not listed in
	// the explicit token policy.
	tokenResponse := extended.request(t, "POST", "/api/token/", `{"email":"dev@suchi.local","password":"devdevdev"}`, 200)
	var issued struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(tokenResponse, &issued); err != nil || issued.Token == "" {
		t.Fatalf("decode token: token=%q err=%v", issued.Token, err)
	}
	tokenReq, _ := http.NewRequest("POST", extended.url+"/api/assembly-test/job", nil)
	tokenReq.Header.Set("Authorization", "Token "+issued.Token)
	tokenClient := &http.Client{Timeout: 3 * time.Second}
	tokenResp, err := tokenClient.Do(tokenReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = tokenResp.Body.Close()
	if tokenResp.StatusCode != http.StatusForbidden {
		t.Fatalf("configured route token status=%d want=%d", tokenResp.StatusCode, http.StatusForbidden)
	}
	extended.request(t, "POST", "/api/assembly-test/job", "", http.StatusAccepted)
	select {
	case event := <-configuredJobs:
		if event.Kind != configuredJobKind || event.DocID != 0 || event.SystemID != 0 {
			t.Fatalf("configured subscriber event=%+v", event)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("configured durable subscriber did not receive job")
	}
	communityReq, _ := http.NewRequest("POST", community.url+"/api/assembly-test/job", nil)
	communityResp, err := community.client.Do(communityReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = communityResp.Body.Close()
	if communityResp.StatusCode == http.StatusAccepted {
		t.Fatal("configured route leaked into community app")
	}
	community.request(t, "POST", "/api/assembly-test/unhandled", "", http.StatusAccepted)
	until(t, func() bool {
		return bytes.Equal(community.request(t, "GET", "/api/assembly-test/unhandled", "", http.StatusOK), []byte("dead"))
	})
	rule := `{"name":"Receipt","enabled":true,"triggers":[{"type":2}],"actions":[{"type":"assign_title","params":{"template":"Filed by shared assembly"}},{"type":"example_receipt","params":{"value":"ok"}}]}`
	extended.request(t, "POST", "/api/automations/", strings.Replace(rule, `"value":"ok"`, `"value":"bad"`, 1), 400)
	extended.request(t, "POST", "/api/automations/", rule, 201)
	denied := community.request(t, "POST", "/api/automations/", rule, 400)
	if !bytes.Contains(denied, []byte("unsupported kind")) {
		t.Fatalf("community response=%s", denied)
	}
	identity := extended.request(t, "GET", "/api/whoami", "", 200)
	if !bytes.Contains(identity, []byte(`"build_version":"assembly-test"`)) || !bytes.Contains(identity, []byte(`"build_revision":"test-revision"`)) {
		t.Fatalf("build identity=%s", identity)
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	file, err := form.CreateFormFile("document", "receipt.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("A local receipt for the assembly integration test."))
	_ = form.Close()
	req, _ := http.NewRequest("POST", extended.url+"/api/documents/", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	resp, err := extended.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	uploaded, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		t.Fatalf("upload=%d %s", resp.StatusCode, uploaded)
	}
	select {
	case target := <-executed:
		if target.SystemID != 1 || target.DocID <= 0 {
			t.Fatalf("target=%+v", target)
		}
		// Execute hooks run inside the automation transaction, so the custom
		// action can signal just before the built-in title change commits.
		until(t, func() bool {
			document := extended.request(t, "GET", fmt.Sprintf("/api/documents/%d", target.DocID), "", 200)
			return bytes.Contains(document, []byte("Filed by shared assembly"))
		})
	case <-time.After(15 * time.Second):
		t.Fatal("post-ingest did not execute supplied action")
	}
	// Both APIs must use their own approval engine even after another app boots.
	exerciseApprovalAPI(t, extended, "Extended review")
	exerciseApprovalAPI(t, community, "Community review")
	definition := extended.request(t, "GET", "/api/approvals/definitions/assembly-review", "", 200)
	if !bytes.Contains(definition, []byte("Extended review")) {
		t.Fatalf("approval definition crossed instances: %s", definition)
	}
}

func exerciseApprovalAPI(t *testing.T, a runningApp, prompt string) {
	t.Helper()
	body := fmt.Sprintf(`{"slug":"assembly-review","spec":{"start":"review","states":{"review":{"kind":"approve","assignee":"role:admin","prompt":%q,"choices":["approve","reject"],"on":{"approve":"done","reject":"done"}},"done":{"kind":"end"}}}}`, prompt)
	a.request(t, "POST", "/api/approvals/definitions", body, 201)
	start := func() int64 {
		response := a.request(t, "POST", "/api/approvals/definitions/assembly-review/start", `{}`, 201)
		var result struct {
			RunID int64 `json:"run_id"`
		}
		if err := json.Unmarshal(response, &result); err != nil {
			t.Fatal(err)
		}
		return result.RunID
	}
	runID := start()
	path := fmt.Sprintf("/api/approvals/runs/%d", runID)
	var taskID int64
	until(t, func() bool {
		data := a.request(t, "GET", path, "", 200)
		var result struct {
			Tasks []struct {
				ID     int64  `json:"id"`
				Prompt string `json:"prompt"`
			} `json:"tasks"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Tasks) == 0 {
			return false
		}
		if result.Tasks[0].Prompt != prompt {
			t.Fatalf("task came from another app: %s", data)
		}
		taskID = result.Tasks[0].ID
		return true
	})
	resolve := fmt.Sprintf("/api/approvals/tasks/%d/resolve", taskID)
	a.request(t, "POST", resolve, `{"choice":"invalid"}`, 400)
	a.request(t, "POST", resolve, `{"choice":"approve"}`, 204)
	a.request(t, "POST", resolve, `{"choice":"approve"}`, 409)
	until(t, func() bool { return bytes.Contains(a.request(t, "GET", path, "", 200), []byte(`"Status":"done"`)) })
	runID = start()
	path = fmt.Sprintf("/api/approvals/runs/%d", runID)
	a.request(t, "POST", path+"/cancel", `{"reason":"test cancel"}`, 204)
	if !bytes.Contains(a.request(t, "GET", path, "", 200), []byte(`"Status":"cancelled"`)) {
		t.Fatal("cancel was not retained")
	}
}

func TestAssemblyRejectsBootExtensionErrors(t *testing.T) {
	for _, test := range []string{"migration failure", "duplicate action", "duplicate component"} {
		t.Run(test, func(t *testing.T) {
			opts := options(t)
			opts.MigrationSets = []db.MigrationSet{{Component: "example", Migrations: []db.Migration{{Version: 1, SQL: `CREATE TABLE example(id INTEGER)`}}}}
			want := "duplicate action"
			switch test {
			case "migration failure":
				opts.MigrationSets[0].Migrations[0].SQL += `; INSERT INTO missing_table VALUES(1)`
				want = "migration component"
			case "duplicate action":
				opts.Actions = []automations.ActionDefinition{automations.BuiltinActions()[0]}
			case "duplicate component":
				opts.MigrationSets = append(opts.MigrationSets, opts.MigrationSets[0])
				want = "duplicate migration component"
			}
			err := app.Run(t.Context(), opts)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("boot error=%v want=%s", err, want)
			}
			d, err := db.Open(t.Context(), filepath.Join(opts.Config.DataDir, "suchi.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			var rows int
			if test == "duplicate action" {
				if err := d.Read.QueryRow(`SELECT count(*) FROM _suchi_extension_migrations WHERE component='example'`).Scan(&rows); err != nil || rows != 1 {
					t.Fatalf("migration before action registration: rows=%d err=%v", rows, err)
				}
			} else if test == "migration failure" {
				if err := d.Read.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='example'`).Scan(&rows); err != nil || rows != 0 {
					t.Fatalf("failed migration leaked schema: rows=%d err=%v", rows, err)
				}
			}
		})
	}
}

func TestPipelineProposalVersionsControlKindsIndependently(t *testing.T) {
	opts := options(t)
	stop := errors.New("stop after pipeline proposal inspection")
	opts.Configure = func(services *app.Services) error {
		_, err := services.DB.Write.ExecContext(t.Context(), `
			INSERT INTO documents(
				system_id, owner_id, original_blob, original_size, title, jd_category_id,
				added_at, created_at, updated_at, pipeline_version_ocr,
				pipeline_version_llm, pipeline_version_content
			)
			VALUES (
				1,
				(SELECT id FROM users WHERE email = 'dev@suchi.local'),
				'pipeline-proposal-stale', 1, 'Pipeline proposal fixture',
				(SELECT inbox_category_id FROM jd_systems WHERE id = 1),
				0, 0, 0, ?, 0, ?
			)
		`, postingest.PipelineVersionOCR-1, postingest.PipelineVersionContent-1)
		if err != nil {
			return err
		}
		return stop
	}
	if err := app.Run(t.Context(), opts); !errors.Is(err, stop) {
		t.Fatalf("seed archive: %v", err)
	}

	type proposalCounts struct {
		total   int
		ocr     int
		llm     int
		content int
	}
	bootAndCount := func(policy rescan.Versions) proposalCounts {
		t.Helper()
		bootOpts := opts
		bootOpts.PipelineProposalVersions = policy
		counts := proposalCounts{}
		bootOpts.Configure = func(services *app.Services) error {
			if err := services.DB.Read.QueryRowContext(t.Context(), `
				SELECT
					COUNT(*),
					COALESCE(SUM(json_extract(r.vars_json, '$.kind') = 'ocr'), 0),
					COALESCE(SUM(json_extract(r.vars_json, '$.kind') = 'llm'), 0),
					COALESCE(SUM(json_extract(r.vars_json, '$.kind') = 'content'), 0)
				FROM approval_runs r
				JOIN approval_defs def ON def.id = r.def_id
				WHERE def.slug = ?
			`, rescan.ProposalSlug).Scan(&counts.total, &counts.ocr, &counts.llm, &counts.content); err != nil {
				return err
			}
			return stop
		}
		if err := app.Run(t.Context(), bootOpts); !errors.Is(err, stop) {
			t.Fatalf("boot with pipeline proposal policy %+v: %v", policy, err)
		}
		return counts
	}

	if got := bootAndCount(rescan.Versions{}); got.total != 0 {
		t.Fatalf("zero proposal policy created %+v runs, want none", got)
	}
	ocrPolicy := rescan.Versions{OCR: postingest.PipelineVersionOCR}
	if got := bootAndCount(ocrPolicy); got != (proposalCounts{total: 1, ocr: 1}) {
		t.Fatalf("OCR-only proposal policy created %+v runs", got)
	}
	if got := bootAndCount(rescan.Versions{}); got != (proposalCounts{total: 1, ocr: 1}) {
		t.Fatalf("zero proposal policy changed existing runs to %+v", got)
	}
	contentPolicy := rescan.Versions{Content: postingest.PipelineVersionContent}
	if got := bootAndCount(contentPolicy); got != (proposalCounts{total: 2, ocr: 1, content: 1}) {
		t.Fatalf("content-only proposal policy created %+v runs", got)
	}
}

func TestPipelineProposalVersionsRejectFutureRevisions(t *testing.T) {
	opts := options(t)
	opts.PipelineProposalVersions = rescan.Versions{OCR: postingest.PipelineVersionOCR + 1}
	err := app.Run(t.Context(), opts)
	if err == nil || !strings.Contains(err.Error(), "ocr revision") || !strings.Contains(err.Error(), "exceeds current") {
		t.Fatalf("future proposal revision error = %v", err)
	}
}

func TestConfigureErrorStartsNoRuntime(t *testing.T) {
	opts := options(t)
	handled := make(chan pluginapi.Event, 1)
	wantErr := errors.New("configure rejected")
	opts.Configure = func(services *app.Services) error {
		services.Jobs.Register(configuredSubscriber{handled: handled})
		if err := services.DB.WriteTx(t.Context(), func(tx *sql.Tx) error {
			return jobs.Enqueue(t.Context(), tx, configuredJobKind, 0, 0, `{"source":"failed-configure"}`)
		}); err != nil {
			return err
		}
		services.Mux.HandleFunc("GET /api/never-started", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
		return wantErr
	}

	err := app.Run(t.Context(), opts)
	if !errors.Is(err, wantErr) {
		t.Fatalf("app.Run error=%v want=%v", err, wantErr)
	}
	select {
	case event := <-handled:
		t.Fatalf("subscriber ran after Configure failure: %+v", event)
	default:
	}

	listener, err := net.Listen("tcp", opts.Config.ListenAddr)
	if err != nil {
		t.Fatalf("HTTP listener started after Configure failure: %v", err)
	}
	_ = listener.Close()

	database, err := db.Open(t.Context(), filepath.Join(opts.Config.DataDir, "suchi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var state string
	if err := database.Read.QueryRowContext(t.Context(), `SELECT state FROM jobs WHERE kind = ?`, configuredJobKind).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "pending" {
		t.Fatalf("job state=%q want pending; a worker started after Configure failure", state)
	}
}
