// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/audit"
	"github.com/johnnybravo-xyz/suchi/core/auth"
	"github.com/johnnybravo-xyz/suchi/core/authz"
	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

const maxDocumentNoteBytes = 16 << 10

var errNoteForbidden = errors.New("note belongs to another user")

// DocumentNote is an attributed, exact-revision note shown on Document Detail.
type DocumentNote struct {
	ID        int64  `json:"id"`
	UserID    *int64 `json:"user_id,omitempty"`
	Author    string `json:"author"`
	Note      string `json:"note"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	CanEdit   bool   `json:"can_edit"`
}

type documentNoteRequest struct {
	Note string `json:"note"`
}

func (s *Server) loadDocumentNotes(ctx context.Context, p *pluginapi.Principal, documentID int64) ([]DocumentNote, error) {
	rows, err := s.DB.Read.QueryContext(ctx, `
		SELECT n.id, n.user_id,
		       COALESCE(NULLIF(u.display_name, ''), u.email, 'Former user'),
		       n.note, n.created_at, COALESCE(n.updated_at, n.created_at)
		FROM notes n
		LEFT JOIN users u ON u.id = n.user_id
		WHERE n.document_id = ?
		ORDER BY n.created_at, n.id
	`, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]DocumentNote, 0)
	for rows.Next() {
		var (
			note   DocumentNote
			userID sql.NullInt64
		)
		if err := rows.Scan(&note.ID, &userID, &note.Author, &note.Note, &note.CreatedAt, &note.UpdatedAt); err != nil {
			return nil, err
		}
		if userID.Valid {
			id := userID.Int64
			note.UserID = &id
		}
		note.CanEdit = p != nil && (p.Role == "admin" || userID.Valid && userID.Int64 == p.UserID)
		out = append(out, note)
	}
	return out, rows.Err()
}

// CreateDocumentNote serves POST /api/documents/{id}/notes/.
func (s *Server) CreateDocumentNote(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	p := auth.FromContext(r.Context())
	documentID, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "invalid document id")
		return
	}
	if !s.authorize(w, r, p, authz.KindDocument, documentID, authz.PermChange) {
		return
	}
	var in documentNoteRequest
	if err := decodeJSON(r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON: "+err.Error())
		return
	}
	noteText, ok := s.validDocumentNote(w, in.Note)
	if !ok {
		return
	}

	now := time.Now().Unix()
	var (
		noteID     int64
		noteUserID int64
		noteAuthor string
	)
	err = s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		allowed, err := s.authorized(r.Context(), tx, p, authz.KindDocument, documentID, authz.PermChange)
		if err != nil {
			return err
		}
		if !allowed {
			return errNotFound
		}
		var systemID int64
		if err := tx.QueryRowContext(r.Context(), `SELECT system_id FROM documents WHERE id = ? AND trashed_at IS NULL`, documentID).Scan(&systemID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errNotFound
			}
			return err
		}
		current, err := s.currentWriterPrincipal(r.Context(), tx, p, systemID)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO notes(document_id, user_id, note, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
		`, documentID, current.UserID, noteText, now, now)
		if err != nil {
			return err
		}
		noteID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		noteUserID = current.UserID
		noteAuthor, err = noteAuthorFromQuery(r.Context(), tx, current.UserID)
		return err
	})
	if err != nil {
		if errors.Is(err, errNotFound) {
			s.writeError(w, http.StatusNotFound, "not_found", "document not found")
			return
		}
		s.serverErr(w, "document_notes.create", err)
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		SystemID: selectedSystemID(r.Context()), Actor: p,
		Action: "document.note.create", ObjectKind: "document", ObjectID: documentID,
		After: map[string]any{"note_id": noteID},
	})
	userID := noteUserID
	s.writeJSON(w, http.StatusCreated, DocumentNote{
		ID: noteID, UserID: &userID, Author: noteAuthor, Note: noteText,
		CreatedAt: now, UpdatedAt: now, CanEdit: true,
	})
}

// UpdateDocumentNote serves PATCH /api/documents/{id}/notes/{note}.
func (s *Server) UpdateDocumentNote(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	p := auth.FromContext(r.Context())
	documentID, noteID, ok := s.parseDocumentNotePath(w, r)
	if !ok {
		return
	}
	var in documentNoteRequest
	if err := decodeJSON(r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_body", "invalid JSON: "+err.Error())
		return
	}
	noteText, ok := s.validDocumentNote(w, in.Note)
	if !ok {
		return
	}
	now := time.Now().Unix()
	err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		if err := s.authorizeDocumentNoteWrite(r.Context(), tx, p, documentID, noteID); err != nil {
			return err
		}
		_, err := tx.ExecContext(r.Context(), `UPDATE notes SET note = ?, updated_at = ? WHERE id = ?`, noteText, now, noteID)
		return err
	})
	if writeDocumentNoteError(s, w, err) {
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		SystemID: selectedSystemID(r.Context()), Actor: p,
		Action: "document.note.update", ObjectKind: "document", ObjectID: documentID,
		After: map[string]any{"note_id": noteID},
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"id": noteID, "updated_at": now})
}

// DeleteDocumentNote serves DELETE /api/documents/{id}/notes/{note}.
func (s *Server) DeleteDocumentNote(w http.ResponseWriter, r *http.Request) {
	if !auth.RequireScope(w, r, auth.ScopeDocumentsWrite) {
		return
	}
	p := auth.FromContext(r.Context())
	documentID, noteID, ok := s.parseDocumentNotePath(w, r)
	if !ok {
		return
	}
	err := s.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		if err := s.authorizeDocumentNoteWrite(r.Context(), tx, p, documentID, noteID); err != nil {
			return err
		}
		_, err := tx.ExecContext(r.Context(), `DELETE FROM notes WHERE id = ?`, noteID)
		return err
	})
	if writeDocumentNoteError(s, w, err) {
		return
	}
	audit.Log(r.Context(), s.DB, s.Log, audit.Event{
		SystemID: selectedSystemID(r.Context()), Actor: p,
		Action: "document.note.delete", ObjectKind: "document", ObjectID: documentID,
		Before: map[string]any{"note_id": noteID},
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) authorizeDocumentNoteWrite(ctx context.Context, tx *sql.Tx, p *pluginapi.Principal, documentID, noteID int64) error {
	allowed, err := s.authorized(ctx, tx, p, authz.KindDocument, documentID, authz.PermChange)
	if err != nil {
		return err
	}
	if !allowed {
		return errNotFound
	}
	var (
		userID   sql.NullInt64
		systemID int64
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT n.user_id, d.system_id
		FROM notes n
		JOIN documents d ON d.id = n.document_id AND d.trashed_at IS NULL
		WHERE n.id = ? AND n.document_id = ?
	`, noteID, documentID).Scan(&userID, &systemID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		}
		return err
	}
	current, err := s.currentWriterPrincipal(ctx, tx, p, systemID)
	if err != nil {
		return err
	}
	if current.Role != "admin" && (!userID.Valid || userID.Int64 != current.UserID) {
		return errNoteForbidden
	}
	return nil
}

func (s *Server) parseDocumentNotePath(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	documentID, err := parseIDPath(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_id", "invalid document id")
		return 0, 0, false
	}
	noteID, err := strconv.ParseInt(r.PathValue("note"), 10, 64)
	if err != nil || noteID <= 0 {
		s.writeError(w, http.StatusBadRequest, "bad_note_id", "invalid note id")
		return 0, 0, false
	}
	return documentID, noteID, true
}

func (s *Server) validDocumentNote(w http.ResponseWriter, value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		s.writeError(w, http.StatusBadRequest, "empty_note", "note must not be empty")
		return "", false
	}
	if len(value) > maxDocumentNoteBytes {
		s.writeError(w, http.StatusRequestEntityTooLarge, "note_too_large", "note exceeds 16 KiB")
		return "", false
	}
	return value, true
}

func writeDocumentNoteError(s *Server, w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errNotFound):
		s.writeError(w, http.StatusNotFound, "not_found", "note not found")
	case errors.Is(err, errNoteForbidden):
		s.writeError(w, http.StatusForbidden, "forbidden", "only the note author or an administrator can change this note")
	default:
		s.serverErr(w, "document_notes.write", err)
	}
	return true
}

type documentNoteQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func noteAuthorFromQuery(ctx context.Context, q documentNoteQueryer, userID int64) (string, error) {
	var author string
	err := q.QueryRowContext(ctx, `
		SELECT COALESCE(NULLIF(display_name, ''), email)
		FROM users
		WHERE id = ?
	`, userID).Scan(&author)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(author) == "" {
		return "User #" + strconv.FormatInt(userID, 10), nil
	}
	return author, nil
}
