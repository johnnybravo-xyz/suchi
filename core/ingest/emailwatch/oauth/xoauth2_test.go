package oauth

import (
	"bytes"
	"testing"
)

func TestXOAUTH2ClientStart(t *testing.T) {
	cases := []struct {
		name    string
		user    string
		token   string
		wantMec string
		wantIR  []byte
		wantErr bool
	}{
		{
			name:    "canonical",
			user:    "alice@example.com",
			token:   "ya29.deadbeef",
			wantMec: "XOAUTH2",
			wantIR:  []byte("user=alice@example.com\x01auth=Bearer ya29.deadbeef\x01\x01"),
		},
		{
			name:    "empty user rejects",
			user:    "",
			token:   "t",
			wantErr: true,
		},
		{
			name:    "empty token rejects",
			user:    "u@x",
			token:   "",
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl := XOAUTH2Client(c.user, c.token)
			mech, ir, err := cl.Start()
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if mech != c.wantMec {
				t.Errorf("mech = %q, want %q", mech, c.wantMec)
			}
			if !bytes.Equal(ir, c.wantIR) {
				t.Errorf("ir = %q, want %q", ir, c.wantIR)
			}
		})
	}
}
