package emailwatch_test

import (
	"encoding/json"
	"testing"

	"github.com/emersion/go-imap"
	"github.com/johnnybravo-xyz/suchi/core/ingest/emailwatch"
)

// decode unmarshals the produced JSON into a map so the tests can
// assert on presence/absence semantics (omitempty vs. explicit false)
// without re-deriving the field names from the struct.
func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func TestBuildPostIngestPayload_AllFieldsPresent(t *testing.T) {
	env := &imap.Envelope{
		Subject: "Invoice #42",
		From: []*imap.Address{
			{PersonalName: "Alice", MailboxName: "alice", HostName: "example.com"},
		},
	}
	raw, err := emailwatch.BuildPostIngestPayload(
		"deadbeef", 1234, "message/rfc822", "invoice.eml", "INBOX", env, true, false,
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	m := decode(t, raw)
	if m["sha256"] != "deadbeef" {
		t.Errorf("sha256: got %v", m["sha256"])
	}
	if m["size"].(float64) != 1234 {
		t.Errorf("size: got %v", m["size"])
	}
	if m["mime_type"] != "message/rfc822" {
		t.Errorf("mime_type: got %v", m["mime_type"])
	}
	if m["filename"] != "invoice.eml" {
		t.Errorf("filename: got %v", m["filename"])
	}
	if m["email_from"] != "alice@example.com" {
		t.Errorf("email_from: got %v", m["email_from"])
	}
	if m["email_subject"] != "Invoice #42" {
		t.Errorf("email_subject: got %v", m["email_subject"])
	}
	if m["email_folder"] != "INBOX" {
		t.Errorf("email_folder: got %v", m["email_folder"])
	}
	if m["email_has_attachment"] != true {
		t.Errorf("email_has_attachment: got %v", m["email_has_attachment"])
	}
}

func TestBuildPostIngestPayload_FirstFromAddressWins(t *testing.T) {
	env := &imap.Envelope{
		From: []*imap.Address{
			{MailboxName: "first", HostName: "a.example"},
			{MailboxName: "second", HostName: "b.example"},
		},
	}
	raw, err := emailwatch.BuildPostIngestPayload("s", 1, "m", "f", "INBOX", env, false, false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	m := decode(t, raw)
	if m["email_from"] != "first@a.example" {
		t.Errorf("want first@a.example, got %v", m["email_from"])
	}
}

func TestBuildPostIngestPayload_EmptyFromStaysEmpty(t *testing.T) {
	// Envelope present but no From addresses — the field must stay
	// empty (and therefore omitted by omitempty) so matchers don't
	// see a stray "@" or partial string.
	env := &imap.Envelope{Subject: "hello", From: nil}
	raw, err := emailwatch.BuildPostIngestPayload("s", 1, "m", "f", "INBOX", env, false, false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	m := decode(t, raw)
	if _, ok := m["email_from"]; ok {
		t.Errorf("email_from should be omitted when From is empty, got %v", m["email_from"])
	}
	if m["email_subject"] != "hello" {
		t.Errorf("email_subject: got %v", m["email_subject"])
	}
}

func TestBuildPostIngestPayload_NilEnvelope(t *testing.T) {
	raw, err := emailwatch.BuildPostIngestPayload(
		"cafef00d", 42, "message/rfc822", "orphan.eml", "Archive", nil, true, false,
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	m := decode(t, raw)
	if _, ok := m["email_from"]; ok {
		t.Errorf("email_from should be omitted when envelope is nil")
	}
	if _, ok := m["email_subject"]; ok {
		t.Errorf("email_subject should be omitted when envelope is nil")
	}
	if m["email_folder"] != "Archive" {
		t.Errorf("email_folder: got %v", m["email_folder"])
	}
	if m["email_has_attachment"] != true {
		t.Errorf("email_has_attachment: got %v", m["email_has_attachment"])
	}
	if m["sha256"] != "cafef00d" {
		t.Errorf("sha256: got %v", m["sha256"])
	}
}

func TestBuildPostIngestPayload_HasAttachmentFalseIsEmitted(t *testing.T) {
	// The bool must be present in JSON even when false — matchers
	// need to distinguish "false" from "absent". This is why the
	// struct tag omits `omitempty` for that one field only.
	raw, err := emailwatch.BuildPostIngestPayload("s", 1, "m", "f", "INBOX", nil, false, false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	m := decode(t, raw)
	v, ok := m["email_has_attachment"]
	if !ok {
		t.Fatal("email_has_attachment key must be present even when false")
	}
	if v != false {
		t.Errorf("want false, got %v", v)
	}
}

func TestBuildPostIngestPayload_SubjectPassthrough(t *testing.T) {
	// The helper does not decode / unfold — that stays a matcher
	// concern. Assert a subject with encoded-word syntax survives
	// verbatim.
	subj := "=?utf-8?B?SGVsbG8gd29ybGQ=?="
	env := &imap.Envelope{Subject: subj}
	raw, err := emailwatch.BuildPostIngestPayload("s", 1, "m", "f", "INBOX", env, false, false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	m := decode(t, raw)
	if m["email_subject"] != subj {
		t.Errorf("subject not passed through verbatim: got %v", m["email_subject"])
	}
}

func TestBuildPostIngestPayload_NilFromEntry(t *testing.T) {
	// Defensive: some senders' envelopes come back with a nil entry
	// in the From slice. The helper must not panic.
	env := &imap.Envelope{From: []*imap.Address{nil}}
	raw, err := emailwatch.BuildPostIngestPayload("s", 1, "m", "f", "INBOX", env, false, false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	m := decode(t, raw)
	if _, ok := m["email_from"]; ok {
		t.Errorf("email_from should be omitted when From[0] is nil")
	}
}
