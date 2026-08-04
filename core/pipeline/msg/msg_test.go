package msg_test

import (
	"testing"

	"github.com/suchi-dms/suchi/core/pipeline/msg"
)

func TestRecognized(t *testing.T) {
	cases := []struct {
		mime string
		want bool
	}{
		{"application/vnd.ms-outlook", true},
		{"application/x-outlook-msg", true},
		{"application/ms-outlook", true},
		{"APPLICATION/VND.MS-OUTLOOK", true},
		{"message/rfc822", false},
		{"application/pdf", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := msg.Recognized(tc.mime); got != tc.want {
			t.Errorf("Recognized(%q) = %v, want %v", tc.mime, got, tc.want)
		}
	}
}

// Convert() end-to-end would need msgconvert on PATH. libemail-outlook-message-perl
// isn't packaged on the CI host, so the round-trip is exercised in the docker
// full-image smoke test rather than here.
