package emailwatch_test

import (
	"testing"
	"time"

	"github.com/johnnybravo-xyz/suchi/core/ingest/emailwatch"
)

func TestParseURL(t *testing.T) {
	cases := []struct {
		in         string
		host, user string
		folder     string
		useTLS     bool
		wantErr    bool
	}{
		{"imaps://alice@mail.example.com/INBOX", "mail.example.com", "alice", "INBOX", true, false},
		{"imap://bob@localhost:143/Archive", "localhost:143", "bob", "Archive", false, false},
		{"imaps://alice@mail.example.com", "mail.example.com", "alice", "INBOX", true, false},
		{"ftp://alice@mail.example.com", "", "", "", false, true},
		{"imaps://mail.example.com/INBOX", "", "", "", false, true},
		{"", "", "", "", false, true},
	}
	for _, c := range cases {
		host, user, folder, useTLS, err := emailwatch.ParseURL(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: want error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if host != c.host || user != c.user || folder != c.folder || useTLS != c.useTLS {
			t.Errorf("%q: host=%q user=%q folder=%q tls=%v", c.in, host, user, folder, useTLS)
		}
	}
}

func TestRouteFromPlusAddress(t *testing.T) {
	cases := map[string]int{
		"archive+22@example.com":    22,
		"archive+31@example.com":    31,
		"archive@example.com":       0, // no plus
		"archive+flat@example.com":  0, // explicit flat → inbox
		"archive+notnumeric@ex.com": 0,
		"":                          0,
	}
	for in, want := range cases {
		if got := emailwatch.RouteFromPlusAddress(in); got != want {
			t.Errorf("%q: got %d, want %d", in, got, want)
		}
	}
}

func TestSidecarFromMessage(t *testing.T) {
	s := emailwatch.SidecarFromMessage(emailwatch.MessageHeader{
		From:        "BESCOM <billing@bescom.co.in>",
		Subject:     "Electricity bill March 2026",
		Date:        time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC),
		DeliveredTo: "archive+31@example.com",
	})
	if s.Title != "Electricity bill March 2026" {
		t.Errorf("Title=%q", s.Title)
	}
	if len(s.Correspondents) != 1 || s.Correspondents[0].Role != "sender" {
		t.Errorf("Correspondents=%+v", s.Correspondents)
	}
	if s.JDCategory != 31 {
		t.Errorf("JDCategory=%d, want 31 (from plus-address)", s.JDCategory)
	}
	if s.Created != "2026-03-02" {
		t.Errorf("Created=%q", s.Created)
	}
	if len(s.Tags) != 1 || s.Tags[0] != "source:email" {
		t.Errorf("Tags=%v", s.Tags)
	}
}
