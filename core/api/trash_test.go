package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/db"
	trashservice "github.com/johnnybravo-xyz/suchi/core/trash"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func newTrashAPIServer(t *testing.T) (*Server, *http.ServeMux, *blob.CAS) {
	t.Helper()
	database := openTestDB(t)
	dataDir := t.TempDir()
	cas, err := blob.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	renderRoot := filepath.Join(dataDir, "rendered")
	if err := os.MkdirAll(renderRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	retention, err := trashservice.New(database, renderRoot, log)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(database, cas, retention, log)
	if err != nil {
		t.Fatal(err)
	}
	seedStatsJDInbox(t, database)
	seedUser(t, database, 1)
	mux := http.NewServeMux()
	server.Register(mux)
	return server, mux, cas
}

func seedTrashAPIDocument(t *testing.T, server *Server, cas *blob.CAS, ownerID int64, title string, trashed bool, timestamp int64) int64 {
	t.Helper()
	ref, err := cas.Put(strings.NewReader(title))
	if err != nil {
		t.Fatal(err)
	}
	return seedStatsDoc(t, server.DB, ownerID, ref.SHA256, title, 1, trashed, timestamp)
}

func doTrashAPIRequest(t *testing.T, mux *http.ServeMux, method, path string, principal *pluginapi.Principal) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	if principal != nil {
		request = request.WithContext(auth.WithPrincipal(request.Context(), principal))
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder
}

func TestListTrashIncludesFixedDeletionDateAndOwnerScope(t *testing.T) {
	server, mux, cas := newTrashAPIServer(t)
	trashedAt := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC).Unix()
	ownID := seedTrashAPIDocument(t, server, cas, 1, "Own trash", true, trashedAt)
	seedTrashAPIDocument(t, server, cas, 2, "Other trash", true, trashedAt+1)

	response := doTrashAPIRequest(t, mux, http.MethodGet, "/api/trash/", memberPrincipal(1))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Count   int        `json:"count"`
		Results []TrashRow `json:"results"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Count != 1 || len(envelope.Results) != 1 || envelope.Results[0].ID != ownID {
		t.Fatalf("unexpected Trash scope: %+v", envelope)
	}
	wantDeletesAt := trashedAt + int64(trashservice.Retention/time.Second)
	if envelope.Results[0].DeletesAt != wantDeletesAt {
		t.Fatalf("deletes_at=%d, want %d", envelope.Results[0].DeletesAt, wantDeletesAt)
	}
}

func TestPurgeTrashDocumentRequiresTrashedStateAndDeletePermission(t *testing.T) {
	server, mux, cas := newTrashAPIServer(t)
	now := time.Now().Unix()
	liveID := seedTrashAPIDocument(t, server, cas, 1, "Live", false, now)
	trashedID := seedTrashAPIDocument(t, server, cas, 1, "Trashed", true, now)
	seedUser(t, server.DB, 2)

	response := doTrashAPIRequest(t, mux, http.MethodDelete,
		"/api/trash/"+itoa(trashedID), memberPrincipal(2))
	if response.Code != http.StatusForbidden {
		t.Fatalf("unauthorized status=%d, want 403; body=%s", response.Code, response.Body.String())
	}
	response = doTrashAPIRequest(t, mux, http.MethodDelete,
		"/api/trash/"+itoa(liveID), memberPrincipal(1))
	if response.Code != http.StatusNotFound {
		t.Fatalf("live status=%d, want 404; body=%s", response.Code, response.Body.String())
	}
	response = doTrashAPIRequest(t, mux, http.MethodDelete,
		"/api/trash/"+itoa(trashedID), memberPrincipal(1))
	if response.Code != http.StatusNoContent {
		t.Fatalf("permanent-delete status=%d, want 204; body=%s", response.Code, response.Body.String())
	}
	if rowExistsInAPI(t, server, trashedID) {
		t.Fatal("permanently deleted document row remains")
	}
}

func TestEmptyTrashUsesMemberAndAdminScopes(t *testing.T) {
	t.Run("member owner scope", func(t *testing.T) {
		server, mux, cas := newTrashAPIServer(t)
		now := time.Now().Unix()
		ownID := seedTrashAPIDocument(t, server, cas, 1, "Member trash", true, now)
		otherID := seedTrashAPIDocument(t, server, cas, 2, "Other trash", true, now)

		response := doTrashAPIRequest(t, mux, http.MethodDelete, "/api/trash/", memberPrincipal(1))
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var body struct {
			Purged int `json:"purged"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Purged != 1 || rowExistsInAPI(t, server, ownID) || !rowExistsInAPI(t, server, otherID) {
			t.Fatalf("member purge=%d own_exists=%t other_exists=%t",
				body.Purged, rowExistsInAPI(t, server, ownID), rowExistsInAPI(t, server, otherID))
		}
	})

	t.Run("administrator archive scope", func(t *testing.T) {
		server, mux, cas := newTrashAPIServer(t)
		now := time.Now().Unix()
		firstID := seedTrashAPIDocument(t, server, cas, 1, "Admin trash", true, now)
		secondID := seedTrashAPIDocument(t, server, cas, 2, "Member trash", true, now)

		response := doTrashAPIRequest(t, mux, http.MethodDelete, "/api/trash/", adminPrincipal(1))
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var body struct {
			Purged int `json:"purged"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Purged != 2 || rowExistsInAPI(t, server, firstID) || rowExistsInAPI(t, server, secondID) {
			t.Fatalf("admin purge=%d first_exists=%t second_exists=%t",
				body.Purged, rowExistsInAPI(t, server, firstID), rowExistsInAPI(t, server, secondID))
		}
	})
}

func TestRestoreDocumentStopsAtRetentionBoundary(t *testing.T) {
	server, mux, cas := newTrashAPIServer(t)
	now := time.Now()
	recoverableID := seedTrashAPIDocument(t, server, cas, 1, "Recoverable", true,
		now.Add(-29*24*time.Hour).Unix())
	expiredID := seedTrashAPIDocument(t, server, cas, 1, "Expired", true,
		now.Add(-trashservice.Retention-time.Second).Unix())

	response := doTrashAPIRequest(t, mux, http.MethodPost,
		"/api/documents/"+itoa(recoverableID)+"/restore", memberPrincipal(1))
	if response.Code != http.StatusOK {
		t.Fatalf("recoverable status=%d body=%s", response.Code, response.Body.String())
	}
	var trashedAt *int64
	if err := server.DB.Read.QueryRowContext(context.Background(),
		`SELECT trashed_at FROM documents WHERE id = ?`, recoverableID).Scan(&trashedAt); err != nil {
		t.Fatal(err)
	}
	if trashedAt != nil {
		t.Fatalf("recoverable trashed_at=%d, want NULL", *trashedAt)
	}

	response = doTrashAPIRequest(t, mux, http.MethodPost,
		"/api/documents/"+itoa(expiredID)+"/restore", memberPrincipal(1))
	if response.Code != http.StatusNotFound {
		t.Fatalf("expired status=%d, want 404; body=%s", response.Code, response.Body.String())
	}
	if !rowExistsInAPI(t, server, expiredID) {
		t.Fatal("expired row disappeared outside the purge service")
	}
}

func TestUploadDoesNotRestoreExpiredDuplicate(t *testing.T) {
	server, _, _ := newTrashAPIServer(t)
	principal := memberPrincipal(1)
	content := []byte("expired duplicate bytes")

	first := httptest.NewRecorder()
	server.UploadDocument(first, sourceUploadRequest(t, "old.pdf", content, principal))
	if first.Code != http.StatusCreated {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	var original UploadResponse
	if err := json.Unmarshal(first.Body.Bytes(), &original); err != nil {
		t.Fatal(err)
	}
	if _, err := server.DB.Write.ExecContext(context.Background(),
		`UPDATE documents SET trashed_at = ? WHERE id = ?`,
		time.Now().Add(-trashservice.Retention-time.Second).Unix(), original.ID); err != nil {
		t.Fatal(err)
	}

	second := httptest.NewRecorder()
	server.UploadDocument(second, sourceUploadRequest(t, "new.pdf", content, principal))
	if second.Code != http.StatusCreated {
		t.Fatalf("expired duplicate status=%d, want 201; body=%s", second.Code, second.Body.String())
	}
	var replacement UploadResponse
	if err := json.Unmarshal(second.Body.Bytes(), &replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.Restored || replacement.ID == original.ID {
		t.Fatalf("expired row was restored: original=%+v replacement=%+v", original, replacement)
	}
}

func TestUploadRetainsOriginalAcrossConcurrentPurge(t *testing.T) {
	server, mux, cas := newTrashAPIServer(t)
	content := []byte("shared original survives online purge")
	oldID := seedTrashAPIDocument(t, server, cas, 1, string(content), true, time.Now().Unix())
	seedUser(t, server.DB, 2)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	// Pause the real handler's Inbox lookup after CAS.Put but before its write.
	// Purge uses the ordinary read pool, so the interleaving is deterministic.
	readGate, err := sql.Open("sqlite", "file:"+server.DB.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer readGate.Close()
	readGate.SetMaxOpenConns(1)
	held, err := readGate.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	uploader := &Server{
		DB:  &db.DB{Read: readGate, Write: server.DB.Write, Path: server.DB.Path},
		CAS: cas, Log: server.Log, Authz: server.Authz,
	}
	request := sourceUploadRequest(t, "shared.txt", content, memberPrincipal(2))
	request = request.WithContext(auth.WithPrincipal(ctx, memberPrincipal(2)))
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		uploader.UploadDocument(response, request)
	}()
	defer func() {
		cancel()
		_ = held.Close()
		<-done
	}()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for readGate.Stats().WaitCount == 0 {
		select {
		case <-ticker.C:
		case <-done:
			t.Fatal("upload finished before the Inbox gate")
		case <-ctx.Done():
			t.Fatal("upload never reached the Inbox gate")
		}
	}
	purge := doTrashAPIRequest(t, mux, http.MethodDelete, "/api/trash/"+itoa(oldID), memberPrincipal(1))
	if purge.Code != http.StatusNoContent {
		t.Fatalf("purge status=%d body=%s", purge.Code, purge.Body.String())
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	if response.Code != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", response.Code, response.Body.String())
	}
	var uploaded UploadResponse
	if err := json.Unmarshal(response.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	var hash string
	if err := server.DB.Read.QueryRowContext(ctx,
		`SELECT original_blob FROM documents WHERE id = ? AND owner_id = 2`, uploaded.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	original, err := cas.Get(hash)
	if err != nil {
		t.Fatalf("successful upload lost its original: %v", err)
	}
	defer original.Close()
	got, err := io.ReadAll(original)
	if err != nil || string(got) != string(content) {
		t.Fatalf("original=%q err=%v", got, err)
	}
}

func rowExistsInAPI(t *testing.T, server *Server, id int64) bool {
	t.Helper()
	var count int
	if err := server.DB.Read.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM documents WHERE id = ?`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count == 1
}
