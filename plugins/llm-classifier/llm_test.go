package llmclassifier

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/db"
	migrations "github.com/johnnybravo-xyz/suchi/core/db/migrations"
	"github.com/johnnybravo-xyz/suchi/core/jd"
	"github.com/johnnybravo-xyz/suchi/core/jobs"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func openHandlerDocument(t *testing.T, title, content string) (*db.DB, int64) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	migs, err := db.LoadMigrations(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, d, migs, silentLog()); err != nil {
		t.Fatal(err)
	}
	if err := jd.EnsureTree(ctx, d, silentLog(), jd.ModeJD); err != nil {
		t.Fatal(err)
	}
	res, err := d.Write.ExecContext(ctx, `
		INSERT INTO users(email, display_name, role, created_at, updated_at)
		VALUES ('owner@example.com', 'Owner', 'admin', 0, 0)
	`)
	if err != nil {
		t.Fatal(err)
	}
	ownerID, _ := res.LastInsertId()
	inboxID, err := jd.InboxCategoryID(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	res, err = d.Write.ExecContext(ctx, `
		INSERT INTO documents(owner_id, original_blob, original_size, title, content,
		                      jd_category_id, created_at, updated_at)
		VALUES (?, 'handler-test-sha', 10, ?, ?, ?, 0, 0)
	`, ownerID, title, content, inboxID)
	if err != nil {
		t.Fatal(err)
	}
	docID, _ := res.LastInsertId()
	return d, docID
}

func TestNewRejectsNonLocalWithoutAck(t *testing.T) {
	// Non-local endpoint without ack → disabled (not an error).
	p, err := New(Config{
		EndpointURL: "https://api.openai.com/v1",
		Model:       "gpt-4o-mini",
		EgressAck:   false,
	}, silentLog())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p != nil {
		t.Error("plugin should be nil when non-local endpoint lacks ack")
	}
}

func TestNewAcceptsLocalWithoutAck(t *testing.T) {
	p, err := New(Config{
		EndpointURL: "http://localhost:11434/v1",
		Model:       "llama3",
	}, silentLog())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p == nil {
		t.Fatal("plugin should be enabled for localhost")
	}
	if !p.rt.Load().local {
		t.Error("local flag not set for localhost endpoint")
	}
}

func TestNewAcceptsNonLocalWithAck(t *testing.T) {
	p, err := New(Config{
		EndpointURL: "https://api.openai.com/v1",
		Model:       "gpt-4o-mini",
		EgressAck:   true,
	}, silentLog())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p == nil {
		t.Fatal("plugin should be enabled when non-local + ack")
	}
	if p.rt.Load().local {
		t.Error("api.openai.com should not be local")
	}
}

func TestProviderLogsOmitEndpointPathsAndQueries(t *testing.T) {
	const (
		host        = "models.example.test"
		pathSecret  = "tenant-path-secret"
		querySecret = "query-api-secret"
	)
	newLogger := func(output *bytes.Buffer) *slog.Logger {
		return slog.New(slog.NewTextHandler(output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	assertRedacted := func(t *testing.T, output string) {
		t.Helper()
		if !strings.Contains(output, "host="+host) {
			t.Fatalf("log omitted parsed host: %s", output)
		}
		if strings.Contains(output, pathSecret) || strings.Contains(output, querySecret) {
			t.Fatalf("log exposed endpoint path or query: %s", output)
		}
	}

	t.Run("startup", func(t *testing.T) {
		var output bytes.Buffer
		_, err := New(Config{
			EndpointURL: "https://" + host + "/v1/" + pathSecret,
			Model:       "test-model",
			EgressAck:   true,
		}, newLogger(&output))
		if err != nil {
			t.Fatal(err)
		}
		assertRedacted(t, output.String())
	})

	t.Run("reload", func(t *testing.T) {
		var output bytes.Buffer
		plugin := NewDisabled(newLogger(&output))
		if err := plugin.SetConfig(Config{
			EndpointURL: "https://" + host + "/v1/" + pathSecret,
			Model:       "test-model",
			EgressAck:   true,
		}); err != nil {
			t.Fatal(err)
		}
		assertRedacted(t, output.String())
	})

	t.Run("egress acknowledgement", func(t *testing.T) {
		var output bytes.Buffer
		plugin, err := New(Config{
			EndpointURL: "https://" + host + "/v1/" + pathSecret,
			Model:       "test-model",
		}, newLogger(&output))
		if err != nil {
			t.Fatal(err)
		}
		if plugin != nil {
			t.Fatal("unacknowledged hosted endpoint enabled the plugin")
		}
		assertRedacted(t, output.String())
	})

	t.Run("invalid query", func(t *testing.T) {
		var output bytes.Buffer
		_, err := New(Config{
			EndpointURL: "https://" + host + "/v1?api-key=" + querySecret,
			Model:       "test-model",
			EgressAck:   true,
		}, newLogger(&output))
		if err == nil {
			t.Fatal("endpoint query was accepted")
		}
		if strings.Contains(output.String(), querySecret) {
			t.Fatalf("log exposed rejected endpoint query: %s", output.String())
		}
	})
}

func TestNewDisabledOnEmptyURL(t *testing.T) {
	p, _ := New(Config{}, silentLog())
	if p != nil {
		t.Error("empty URL should disable")
	}
}

func TestNewRequiresModel(t *testing.T) {
	if _, err := New(Config{EndpointURL: "http://localhost/v1"}, silentLog()); err == nil {
		t.Error("missing model should error")
	}
}

func TestNewRejectsMalformedEndpointBases(t *testing.T) {
	for _, endpoint := range []string{
		"ftp://localhost/v1",
		"http://user:secret@localhost/v1",
		"http://localhost/v1?api-key=secret",
		"http://localhost/v1#fragment",
		"/v1",
	} {
		t.Run(endpoint, func(t *testing.T) {
			if _, err := New(Config{EndpointURL: endpoint, Model: "x"}, silentLog()); err == nil {
				t.Fatal("expected invalid endpoint to fail")
			}
		})
	}
}

// TestClassifyHappyPath uses an httptest server that mimics a
// /v1/chat/completions endpoint. Verifies the request shape (Bearer
// auth, JSON body with system+user messages) and that a well-formed
// response is parsed into a Result.
func TestClassifyHappyPath(t *testing.T) {
	fakeResp := `{
		"choices": [{
			"message": {
				"role": "assistant",
				"content": "{\"title\": \"Electricity bill March\", \"correspondent\": \"BESCOM\", \"tags\": [\"utilities\"], \"jd_category\": 31, \"confidence\": 0.92}"
			}
		}]
	}`
	var gotAuth, gotContentType string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fakeResp))
	}))
	defer srv.Close()

	p, err := New(Config{
		EndpointURL: srv.URL,
		Model:       "gpt-4o-mini",
		APIKey:      "test-key",
	}, silentLog())
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Classify(context.Background(),
		"March invoice", "total due 4523 rupees",
		[]JDCat{{Code: 31, Name: "Utilities"}, {Code: 22, Name: "Tax"}},
		nil)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if res.Correspondent != "BESCOM" {
		t.Errorf("Correspondent=%q", res.Correspondent)
	}
	if res.JDCategory != 31 {
		t.Errorf("JDCategory=%d", res.JDCategory)
	}
	if res.Confidence < 0.9 {
		t.Errorf("Confidence=%f", res.Confidence)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization=%q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type=%q", gotContentType)
	}
	if gotBody["model"] != "gpt-4o-mini" {
		t.Errorf("model=%v", gotBody["model"])
	}
}

func TestCompleteSharesOpenAITransportAndBoundsOutput(t *testing.T) {
	var got struct {
		Model          string              `json:"model"`
		Messages       []CompletionMessage `json:"messages"`
		MaxTokens      int                 `json:"max_tokens"`
		ResponseFormat map[string]string   `json:"response_format"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("request path/auth = %s / %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Grounded answer [1]."}}]}`))
	}))
	defer srv.Close()
	p, err := New(Config{EndpointURL: srv.URL, Model: "model-x", APIKey: "secret"}, silentLog())
	if err != nil {
		t.Fatal(err)
	}
	answer, err := p.Complete(context.Background(), "trusted system", []CompletionMessage{
		{Role: "user", Content: "question"}, {Role: "assistant", Content: "prior answer"},
	}, 900)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "Grounded answer [1]." || got.Model != "model-x" || got.MaxTokens != 700 {
		t.Fatalf("answer=%q model=%q max=%d", answer, got.Model, got.MaxTokens)
	}
	if len(got.Messages) != 3 || got.Messages[0].Role != "system" || got.Messages[0].Content != "trusted system" {
		t.Fatalf("messages=%+v", got.Messages)
	}
	if got.ResponseFormat != nil {
		t.Fatalf("plain completion response_format=%v", got.ResponseFormat)
	}
	if _, err := p.CompleteJSON(context.Background(), "trusted system",
		[]CompletionMessage{{Role: "user", Content: "question"}}, 100); err != nil {
		t.Fatal(err)
	}
	if got.ResponseFormat["type"] != "json_object" {
		t.Fatalf("JSON completion response_format=%v", got.ResponseFormat)
	}
}

func TestHandlerUsesConfiguredConfidenceForMetadataAndDates(t *testing.T) {
	ctx := context.Background()
	d, docID := openHandlerDocument(t, "Original title", "Boarding pass Date 06 Aug 2026; issued August 1, 2026")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"title\":\"Changed title\",\"correspondent\":\"Guess\",\"tags\":[\"guess\"],\"jd_category\":0,\"confidence\":0.2,\"dates\":[{\"role\":\"service\",\"value\":\"2026-08-06\",\"precision\":\"day\",\"raw_text\":\"06 Aug 2026\",\"evidence\":\"Date 06 Aug 2026\",\"confidence\":0.91},{\"role\":\"issued\",\"value\":\"2026-08-01\",\"precision\":\"day\",\"raw_text\":\"August 1, 2026\",\"evidence\":\"issued August 1, 2026\",\"confidence\":0.69}]}"}}]}`))
	}))
	defer srv.Close()
	p, err := New(Config{EndpointURL: srv.URL, Model: "test", ConfidenceThreshold: 0.7, DateAutoApply: true}, silentLog())
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(p, d, silentLog())
	if err := h.Handle(ctx, pluginapi.Event{Kind: Kind, DocID: docID}); err != nil {
		t.Fatal(err)
	}

	var title string
	var version int
	if err := d.Read.QueryRowContext(ctx,
		`SELECT title, pipeline_version_llm FROM documents WHERE id = ?`, docID).
		Scan(&title, &version); err != nil {
		t.Fatal(err)
	}
	if title != "Original title" || version != PipelineVersionLLM {
		t.Fatalf("low-confidence doc = title:%q version:%d", title, version)
	}
	var reviewTags int
	if err := d.Read.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM document_tags dt
		JOIN tags t ON t.id = dt.tag_id
		WHERE dt.document_id = ? AND t.name = 'needs-review'
	`, docID).Scan(&reviewTags); err != nil {
		t.Fatal(err)
	}
	if reviewTags != 1 {
		t.Fatalf("needs-review tags = %d, want 1", reviewTags)
	}
	for value, wantStatus := range map[string]string{
		"2026-08-06": "accepted",
		"2026-08-01": "pending",
	} {
		var status string
		if err := d.Read.QueryRowContext(ctx, `
			SELECT status FROM document_intelligence
			WHERE document_id = ? AND intelligence_type = 'date' AND sort_value = ?
		`, docID, value).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != wantStatus {
			t.Fatalf("date %s status=%q want=%q", value, status, wantStatus)
		}
	}
}

func TestDateAutoApplyCanBeDisabled(t *testing.T) {
	ctx := context.Background()
	d, docID := openHandlerDocument(t, "Boarding pass", "Date 06 Aug 2026")
	dates, err := prepareDateCandidates("Date 06 Aug 2026", []DateCandidate{{
		Role: "service", Value: "2026-08-06", Precision: "day",
		RawText: "06 Aug 2026", Evidence: "Date 06 Aug 2026", Confidence: 0.95,
	}}, false, 0.7)
	if err != nil {
		t.Fatal(err)
	}
	err = d.WriteTx(ctx, func(tx *sql.Tx) error {
		return replaceDateCandidatesInTx(ctx, tx, docID, "handler-test-sha", dates, time.Now().Unix())
	})
	if err != nil {
		t.Fatal(err)
	}
	var status string
	var evidenceStart sql.NullInt64
	if err := d.Read.QueryRowContext(ctx, `
		SELECT status, evidence_start FROM document_intelligence
		WHERE document_id = ? AND intelligence_type = 'date'
	`, docID).Scan(&status, &evidenceStart); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("date status=%q want=pending", status)
	}
	if !evidenceStart.Valid || evidenceStart.Int64 != 5 {
		t.Fatalf("evidence_start=%v want=5", evidenceStart)
	}
}

func TestPrepareDateCandidatesNormalizesAndComputesOffsets(t *testing.T) {
	dates, err := prepareDateCandidates("Préface — DATE 06 AUG 2026 total", []DateCandidate{{
		Role: " Service ", Value: " 2026-08-06 ", Precision: " DAY ",
		RawText: " 06   Aug 2026 ", Evidence: " DATE\n06 AUG 2026 ", Confidence: 0.9,
	}}, true, 0.8)
	if err != nil {
		t.Fatal(err)
	}
	if len(dates) != 1 {
		t.Fatalf("prepared dates=%d want=1", len(dates))
	}
	got := dates[0]
	if got.Role != "service" || got.SortValue != "2026-08-06" || got.RawText != "06 Aug 2026" || got.EvidenceText != "DATE 06 AUG 2026" {
		t.Fatalf("prepared date=%+v", got)
	}
	if got.ValueJSON != `{"date":"2026-08-06","precision":"day"}` {
		t.Fatalf("value_json=%q", got.ValueJSON)
	}
	if got.status != "accepted" || !got.evidenceStart.Valid || got.evidenceStart.Int64 != 18 {
		t.Fatalf("status=%q evidence_start=%v", got.status, got.evidenceStart)
	}
}

func TestPrepareDateCandidatesValidatesBeforePersistence(t *testing.T) {
	_, err := prepareDateCandidates("Due February 30", []DateCandidate{{
		Role: "due", Value: "2026-02-30", Precision: "day",
		RawText: "February 30", Evidence: "Due February 30", Confidence: 0.9,
	}}, true, 0.8)
	if err == nil {
		t.Fatal("invalid date was prepared")
	}
}

func TestSequentialDateClassificationsReplaceAutomaticFactsAndPreserveReviews(t *testing.T) {
	ctx := context.Background()
	d, docID := openHandlerDocument(t, "Schedule", "Old Aug 6 Reviewed Aug 8 Rejected Aug 9 New Aug 7")
	classify := func(now int64, dates []DateCandidate) {
		t.Helper()
		prepared, err := prepareDateCandidates("Old Aug 6 Reviewed Aug 8 Rejected Aug 9 New Aug 7", dates, true, 0.7)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
			return replaceDateCandidatesInTx(ctx, tx, docID, "handler-test-sha", prepared, now)
		}); err != nil {
			t.Fatal(err)
		}
	}
	classify(1, []DateCandidate{
		{Role: "service", Value: "2026-08-06", Precision: "day", RawText: "Aug 6", Evidence: "Old Aug 6", Confidence: 0.95},
		{Role: "renewal", Value: "2026-08-08", Precision: "day", RawText: "Aug 8", Evidence: "Reviewed Aug 8", Confidence: 0.5},
		{Role: "due", Value: "2026-08-09", Precision: "day", RawText: "Aug 9", Evidence: "Rejected Aug 9", Confidence: 0.5},
	})
	if _, err := d.Write.ExecContext(ctx, `
		UPDATE document_intelligence SET status = 'accepted', reviewed_at = 10
		WHERE document_id = ? AND sort_value = '2026-08-08';
		UPDATE document_intelligence SET status = 'rejected', reviewed_at = 11
		WHERE document_id = ? AND sort_value = '2026-08-09'
	`, docID, docID); err != nil {
		t.Fatal(err)
	}

	// The next extraction changes the automatic date and repeats both
	// reviewed facts exactly. Exact-match conflicts must retain the human
	// decision rather than reviving the model's proposed status.
	classify(2, []DateCandidate{
		{Role: "service", Value: "2026-08-07", Precision: "day", RawText: "Aug 7", Evidence: "New Aug 7", Confidence: 0.96},
		{Role: "renewal", Value: "2026-08-08", Precision: "day", RawText: "Aug 8", Evidence: "Reviewed Aug 8", Confidence: 0.99},
		{Role: "due", Value: "2026-08-09", Precision: "day", RawText: "Aug 9", Evidence: "Rejected Aug 9", Confidence: 0.99},
	})
	rows, err := d.Read.QueryContext(ctx, `
		SELECT sort_value, status, reviewed_at, confidence
		FROM document_intelligence WHERE document_id = ? ORDER BY sort_value
	`, docID)
	if err != nil {
		t.Fatal(err)
	}
	type factState struct {
		status     string
		reviewedAt sql.NullInt64
		confidence float64
	}
	got := map[string]factState{}
	for rows.Next() {
		var value string
		var state factState
		if err := rows.Scan(&value, &state.status, &state.reviewedAt, &state.confidence); err != nil {
			t.Fatal(err)
		}
		got[value] = state
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if _, exists := got["2026-08-06"]; exists {
		t.Fatal("stale automatically accepted date survived changed extraction")
	}
	if state := got["2026-08-07"]; state.status != "accepted" || state.reviewedAt.Valid {
		t.Fatalf("new automatic fact=%+v", state)
	}
	if state := got["2026-08-08"]; state.status != "accepted" || !state.reviewedAt.Valid || state.confidence != 0.5 {
		t.Fatalf("human-accepted fact was overwritten: %+v", state)
	}
	if state := got["2026-08-09"]; state.status != "rejected" || !state.reviewedAt.Valid || state.confidence != 0.5 {
		t.Fatalf("human-rejected fact was overwritten: %+v", state)
	}

	classify(3, nil)
	var automatic, reviewed int
	if err := d.Read.QueryRowContext(ctx, `
		SELECT COUNT(*) FILTER (WHERE reviewed_at IS NULL),
		       COUNT(*) FILTER (WHERE reviewed_at IS NOT NULL)
		FROM document_intelligence WHERE document_id = ?
	`, docID).Scan(&automatic, &reviewed); err != nil {
		t.Fatal(err)
	}
	if automatic != 0 || reviewed != 2 {
		t.Fatalf("after disappeared extraction automatic=%d reviewed=%d", automatic, reviewed)
	}
}

func TestSequentialDateClassificationsRecomputeAutomaticStatusFromSettings(t *testing.T) {
	ctx := context.Background()
	d, docID := openHandlerDocument(t, "Schedule", "Date Aug 6")
	candidate := []DateCandidate{{
		Role: "service", Value: "2026-08-06", Precision: "day",
		RawText: "Aug 6", Evidence: "Date Aug 6", Confidence: 0.95,
	}}
	classify := func(autoApply bool, threshold float64, want string) {
		t.Helper()
		prepared, err := prepareDateCandidates("Date Aug 6", candidate, autoApply, threshold)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.WriteTx(ctx, func(tx *sql.Tx) error {
			return replaceDateCandidatesInTx(ctx, tx, docID, "handler-test-sha", prepared, time.Now().Unix())
		}); err != nil {
			t.Fatal(err)
		}
		var status string
		if err := d.Read.QueryRowContext(ctx, `SELECT status FROM document_intelligence WHERE document_id = ?`, docID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != want {
			t.Fatalf("autoApply=%t threshold=%v status=%q want=%q", autoApply, threshold, status, want)
		}
	}
	classify(true, 0.7, "accepted")
	classify(false, 0.7, "pending")
	classify(true, 0.99, "pending")
	classify(true, 0.9, "accepted")
}

func TestHandlerDoesNotAddCompetingCorrespondent(t *testing.T) {
	ctx := context.Background()
	d, docID := openHandlerDocument(t, "Statement", "credit card statement")
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO correspondents(id, name, slug, created_at, updated_at)
		VALUES (1, 'Header Sender', 'header-sender', 0, 0);
		UPDATE documents SET correspondent_id = 1 WHERE id = ?;
		INSERT INTO document_correspondents(document_id, correspondent_id, role)
		VALUES (?, 1, 'sender');
	`, docID, docID); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"correspondent\":\"Model Guess\",\"confidence\":0.9}"}}]}`))
	}))
	defer srv.Close()
	p, err := New(Config{EndpointURL: srv.URL, Model: "test", ConfidenceThreshold: 0.7}, silentLog())
	if err != nil {
		t.Fatal(err)
	}
	if err := NewHandler(p, d, silentLog()).Handle(ctx, pluginapi.Event{Kind: Kind, DocID: docID}); err != nil {
		t.Fatal(err)
	}

	var primary string
	if err := d.Read.QueryRowContext(ctx, `
		SELECT c.name FROM documents d
		JOIN correspondents c ON c.id = d.correspondent_id
		WHERE d.id = ?
	`, docID).Scan(&primary); err != nil {
		t.Fatal(err)
	}
	var attached, guesses int
	if err := d.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM document_correspondents WHERE document_id = ?`, docID,
	).Scan(&attached); err != nil {
		t.Fatal(err)
	}
	if err := d.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM correspondents WHERE name = 'Model Guess'`,
	).Scan(&guesses); err != nil {
		t.Fatal(err)
	}
	if primary != "Header Sender" || attached != 1 || guesses != 0 {
		t.Fatalf("primary=%q attached=%d model guesses=%d", primary, attached, guesses)
	}
}

func TestHandlerRepairsJunctionOnlySenderBeforeModelGuess(t *testing.T) {
	ctx := context.Background()
	d, docID := openHandlerDocument(t, "Invoice", "Example supplies invoice")
	if _, err := d.Write.ExecContext(ctx, `
		INSERT INTO correspondents(id, name, slug, created_at, updated_at)
		VALUES (1, 'EXAMPLE SUPPLIES PRIVATE LIMITED',
		        'example-supplies-private-limited', 0, 0);
		INSERT INTO document_correspondents(document_id, correspondent_id, role)
		VALUES (?, 1, 'sender');
	`, docID); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"correspondent\":\"Example Supplies Private Limited\",\"confidence\":0.9}"}}]}`))
	}))
	defer srv.Close()
	p, err := New(Config{EndpointURL: srv.URL, Model: "test", ConfidenceThreshold: 0.7}, silentLog())
	if err != nil {
		t.Fatal(err)
	}
	if err := NewHandler(p, d, silentLog()).Handle(ctx, pluginapi.Event{Kind: Kind, DocID: docID}); err != nil {
		t.Fatal(err)
	}

	var primaryID, attached, correspondents int64
	if err := d.Read.QueryRowContext(ctx,
		`SELECT correspondent_id FROM documents WHERE id = ?`, docID,
	).Scan(&primaryID); err != nil {
		t.Fatal(err)
	}
	if err := d.Read.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM document_correspondents
		WHERE document_id = ? AND correspondent_id = 1 AND role = 'sender'
	`, docID).Scan(&attached); err != nil {
		t.Fatal(err)
	}
	if err := d.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM correspondents`).Scan(&correspondents); err != nil {
		t.Fatal(err)
	}
	var canonicalUpdated int64
	if err := d.Read.QueryRowContext(ctx,
		`SELECT updated_at FROM correspondents WHERE id = 1`,
	).Scan(&canonicalUpdated); err != nil {
		t.Fatal(err)
	}
	if primaryID != 1 || attached != 1 || correspondents != 1 || canonicalUpdated != 0 {
		t.Fatalf("primary=%d attached=%d correspondents=%d canonical updated_at=%d",
			primaryID, attached, correspondents, canonicalUpdated)
	}
}

// TestClassifyInjectsJDCatsIntoUserMessage: the per-installation
// Johnny-Decimal categories must land in the user message so the
// model has real codes to pick from — a bare "10-99" hint produces
// confidently-wrong classifications (see the E2E finding).
func TestClassifyInjectsJDCatsIntoUserMessage(t *testing.T) {
	var gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, m := range body.Messages {
			if m.Role == "user" {
				gotUser = m.Content
			}
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"jd_category\":31,\"confidence\":0.9}"}}]}`))
	}))
	defer srv.Close()

	p, _ := New(Config{EndpointURL: srv.URL, Model: "x"}, silentLog())
	_, err := p.Classify(context.Background(), "invoice", "body",
		[]JDCat{{Code: 31, Name: "Utilities"}, {Code: 22, Name: "Tax"}},
		nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"31 – Utilities", "22 – Tax", "Available Johnny-Decimal"} {
		if !strings.Contains(gotUser, want) {
			t.Errorf("user message missing %q; got: %s", want, gotUser)
		}
	}
}

// Sibling titles land in the user message as few-shot examples so the
// model conforms to prior naming instead of drifting. Empty slice must
// omit the block cleanly.
func TestClassifyInjectsSiblingTitles(t *testing.T) {
	var gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role, Content string
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, m := range body.Messages {
			if m.Role == "user" {
				gotUser = m.Content
			}
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"jd_category\":31,\"confidence\":0.9}"}}]}`))
	}))
	defer srv.Close()

	p, _ := New(Config{EndpointURL: srv.URL, Model: "x"}, silentLog())
	_, err := p.Classify(context.Background(), "current", "body", nil,
		[]string{"Electricity bill - Jul 2026", "Electricity bill - Jun 2026"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Electricity bill - Jul 2026",
		"Electricity bill - Jun 2026",
		"Recent titles for similar documents",
	} {
		if !strings.Contains(gotUser, want) {
			t.Errorf("user message missing %q; got: %s", want, gotUser)
		}
	}
}

// Empty jdCats slice must not inject any category header — the model
// falls back to guessing rather than seeing an empty list.
func TestClassifyOmitsHeaderWhenJDCatsEmpty(t *testing.T) {
	var gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, m := range body.Messages {
			if m.Role == "user" {
				gotUser = m.Content
			}
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"jd_category\":0,\"confidence\":0.3}"}}]}`))
	}))
	defer srv.Close()

	p, _ := New(Config{EndpointURL: srv.URL, Model: "x"}, silentLog())
	if _, err := p.Classify(context.Background(), "t", "c", nil, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotUser, "Available Johnny-Decimal") {
		t.Errorf("empty jdCats must omit header; got: %s", gotUser)
	}
}

// TestParseChatCompletionStripsFence: many local models wrap JSON in
// ```json ... ``` despite the system prompt. Wrapper must tolerate.
func TestParseChatCompletionStripsFence(t *testing.T) {
	raw := `{
		"choices": [{"message": {"content": "` + "```" + `json\n{\"title\": \"x\", \"jd_category\": 42, \"confidence\": 0.5}\n` + "```" + `"}}]
	}`
	r, err := parseChatCompletion([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.Title != "x" || r.JDCategory != 42 {
		t.Errorf("result=%+v", r)
	}
}

func TestParseChatCompletionDoesNotEchoMalformedModelOutput(t *testing.T) {
	const sensitive = "private document text"
	_, err := parseChatCompletion([]byte(`{"choices":[{"message":{"content":"` + sensitive + `"}}]}`))
	if err == nil {
		t.Fatal("expected malformed model output to fail")
	}
	if strings.Contains(err.Error(), sensitive) {
		t.Fatalf("error exposed model output: %v", err)
	}
}

func TestParseChatCompletionValidatesAndNormalizesResult(t *testing.T) {
	content, err := json.Marshal(map[string]any{
		"title": "  March\n invoice  ", "correspondent": "  ACME   Corp ",
		"tags":        []string{" Utilities ", "utilities", " TAX "},
		"jd_category": 31, "confidence": 0.8, "language": "EN, de",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{"content": string(content)},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := parseChatCompletion(raw)
	if err != nil {
		t.Fatal(err)
	}
	if r.Title != "March invoice" || r.Correspondent != "ACME Corp" {
		t.Fatalf("text was not normalized: %#v", r)
	}
	if len(r.Tags) != 2 || r.Tags[0] != "utilities" || r.Tags[1] != "tax" {
		t.Fatalf("tags were not normalized and deduplicated: %#v", r.Tags)
	}
	if r.Language != "en,de" {
		t.Fatalf("language = %q, want en,de", r.Language)
	}
}

func TestParseChatCompletionAcceptsIntegerCategoryStrings(t *testing.T) {
	envelope := []byte(`{"choices":[{"message":{"content":"{\"jd_category\":\"31\",\"confidence\":0.8}"}}]}`)
	result, err := parseChatCompletion(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if result.JDCategory != 31 {
		t.Fatalf("jd_category=%d, want 31", result.JDCategory)
	}
}

func TestSelectClassificationContentKeepsDateWindows(t *testing.T) {
	content := strings.Repeat("introductory archive text ", 300) +
		"\nThe policy renewal date is September 14, 2026 and requires action.\n" +
		strings.Repeat("closing archive text ", 300)
	selected := selectClassificationContent(content, 900)
	if !strings.Contains(selected, "renewal date is September 14, 2026") {
		t.Fatalf("date-bearing middle excerpt was dropped: %s", selected)
	}
	if got := len([]rune(selected)); got > 900 {
		t.Fatalf("selected content has %d runes, want <= 900", got)
	}
}

func TestDateSignalCoversStandardFormatsWithoutTreatingNumbersAsDates(t *testing.T) {
	standard := []string{
		"2026-09-14", "14/09/2026", "09/14/26", "14 September 2026",
		"September 14, 2026", "Sept. 14 2026", "2026-09", "09/2026",
		"2026-09-14T10:30:00Z",
	}
	for _, value := range standard {
		if !documentDateSignal.MatchString(value) {
			t.Errorf("standard date format was not recognized: %q", value)
		}
	}
	for _, value := range []string{
		"invoice total 4523", "account 20260914", "serial 123456", "version 2.31",
	} {
		if documentDateSignal.MatchString(value) {
			t.Errorf("ordinary number was treated as a date signal: %q", value)
		}
	}
}

func TestDateCandidatesRequireValidGroundedEvidence(t *testing.T) {
	result := &Result{Confidence: 0.8, Dates: []DateCandidate{{
		Role: "renewal", Value: "2026-09-14", Precision: "day",
		RawText:    "September 14, 2026",
		Evidence:   "The policy renewal date is September 14, 2026.",
		Confidence: 0.94,
	}}}
	if err := validateResult(result); err != nil {
		t.Fatal(err)
	}
	grounded := groundedDateCandidates(result.Dates,
		"Terms. The policy renewal date is September 14, 2026. End.")
	if len(grounded) != 1 {
		t.Fatalf("grounded dates=%+v", grounded)
	}
	if got := groundedDateCandidates(result.Dates, "No matching date appears here."); len(got) != 0 {
		t.Fatalf("ungrounded date survived: %+v", got)
	}
}

func TestParseChatCompletionRejectsUnsafeResultShapes(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "confidence", content: `{"confidence":1.1}`},
		{name: "category", content: `{"confidence":0.8,"jd_category":1000}`},
		{name: "too many tags", content: `{"confidence":0.8,"tags":["a","b","c","d","e","f"]}`},
		{name: "bad language", content: `{"confidence":0.8,"language":"not-a-language"}`},
		{name: "invalid date", content: `{"confidence":0.8,"dates":[{"role":"due","value":"2026-02-30","precision":"day","raw_text":"February 30","evidence":"Due February 30","confidence":0.9}]}`},
		{name: "invalid date role", content: `{"confidence":0.8,"dates":[{"role":"birthday","value":"2026-02-01","precision":"day","raw_text":"February 1","evidence":"Birthday February 1","confidence":0.9}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			envelope, err := json.Marshal(map[string]any{
				"choices": []any{map[string]any{
					"message": map[string]any{"content": tt.content},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseChatCompletion(envelope); err == nil {
				t.Fatal("expected invalid model result to fail")
			}
		})
	}
}

func TestDisableStopsClassifyUntilSetConfig(t *testing.T) {
	p, err := New(Config{EndpointURL: "http://localhost:11434/v1", Model: "x"}, silentLog())
	if err != nil {
		t.Fatal(err)
	}
	p.Disable()
	if p.Enabled() {
		t.Fatal("plugin remained enabled")
	}
	if _, err := p.Classify(context.Background(), "title", "content", nil, nil); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Classify error = %v, want ErrDisabled", err)
	}
	if err := p.SetConfig(Config{EndpointURL: "http://localhost:11434/v1", Model: "x"}); err != nil {
		t.Fatal(err)
	}
	if !p.Enabled() {
		t.Fatal("SetConfig did not re-enable plugin")
	}
	if got := p.rt.Load().client.Timeout; got != 60*time.Second {
		t.Fatalf("default timeout after live activation = %s, want 60s", got)
	}
	if err := p.SetConfig(Config{
		EndpointURL: "http://localhost:11434/v1", Model: "x", Timeout: 17 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	if got := p.rt.Load().client.Timeout; got != 17*time.Second {
		t.Fatalf("live timeout = %s, want 17s", got)
	}
}

func TestDisabledHandlerIsNoOp(t *testing.T) {
	p := NewDisabled(silentLog())
	h := NewHandler(p, nil, silentLog())
	if err := h.Handle(context.Background(), pluginapi.Event{DocID: 42}); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyPropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"private upstream response"}`))
	}))
	defer srv.Close()

	p, _ := New(Config{EndpointURL: srv.URL, Model: "x"}, silentLog())
	_, err := p.Classify(context.Background(), "t", "c", nil, nil)
	if err == nil {
		t.Fatal("500 should propagate as error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err missing 500: %v", err)
	}
	if strings.Contains(err.Error(), "private upstream response") {
		t.Fatalf("error exposed upstream response: %v", err)
	}
	if errors.Is(err, jobs.ErrTerminal) {
		t.Error("500 must remain retryable; ErrTerminal is 4xx-only")
	}
}

func TestClassifyWrapsHTTP4xxAsTerminal(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				w.Write([]byte(`{"error":"nope"}`))
			}))
			defer srv.Close()

			p, _ := New(Config{EndpointURL: srv.URL, Model: "x"}, silentLog())
			_, err := p.Classify(context.Background(), "t", "c", nil, nil)
			if err == nil {
				t.Fatalf("HTTP %d should propagate as error", code)
			}
			if !errors.Is(err, jobs.ErrTerminal) {
				t.Errorf("HTTP %d must wrap jobs.ErrTerminal; got %v", code, err)
			}
		})
	}
}

// 429 rate-limit is retryable — the outbox backoff waits it out. Assert
// it does NOT wrap ErrTerminal.
func TestClassify429StaysRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"rate limit exceeded"}`))
	}))
	defer srv.Close()

	p, _ := New(Config{EndpointURL: srv.URL, Model: "x"}, silentLog())
	_, err := p.Classify(context.Background(), "t", "c", nil, nil)
	if err == nil {
		t.Fatal("429 should propagate as error")
	}
	if errors.Is(err, jobs.ErrTerminal) {
		t.Error("429 must stay retryable")
	}
}
