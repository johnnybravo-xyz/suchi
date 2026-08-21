package emailwatch

import (
	"bytes"
	"errors"
	"testing"

	"github.com/emersion/go-imap"
)

func TestMaterializeRejectsTruncation(t *testing.T) {
	section := &imap.BodySectionName{Peek: true}
	responseSection := &imap.BodySectionName{}
	w := &Watcher{maxAttach: 4}
	limit := w.maxAttach*2 + 8*1024
	msg := &imap.Message{
		Uid: 7,
		Body: map[*imap.BodySectionName]imap.Literal{
			responseSection: bytes.NewReader(bytes.Repeat([]byte("x"), int(limit+1))),
		},
	}
	if _, _, err := w.materialize(msg, section); !errors.Is(err, errMessageTooLarge) {
		t.Fatalf("oversized message error = %v", err)
	}
}

func TestMaterializeAcceptsMessageAtLimit(t *testing.T) {
	section := &imap.BodySectionName{Peek: true}
	responseSection := &imap.BodySectionName{}
	w := &Watcher{maxAttach: 4}
	limit := w.maxAttach*2 + 8*1024
	msg := &imap.Message{
		Uid:      7,
		Envelope: &imap.Envelope{MessageId: "message@example.com"},
		Body: map[*imap.BodySectionName]imap.Literal{
			responseSection: bytes.NewReader(bytes.Repeat([]byte("x"), int(limit))),
		},
	}
	raw, msgID, err := w.materialize(msg, section)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(raw)) != limit || msgID != "<message@example.com>" {
		t.Fatalf("len=%d msgID=%q", len(raw), msgID)
	}
}
