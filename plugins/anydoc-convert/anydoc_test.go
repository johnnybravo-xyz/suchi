package anydocconvert

import "testing"

// Guard the MIME allowlist against typo drift. Every entry the
// follow-up PR wires depends on this table; a bad row here means
// silently no coverage.
func TestHandles(t *testing.T) {
	cases := []struct {
		mime string
		want bool
	}{
		// Word
		{"application/msword", true},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", true},
		// PowerPoint
		{"application/vnd.ms-powerpoint", true},
		{"application/vnd.openxmlformats-officedocument.presentationml.presentation", true},
		// Excel
		{"application/vnd.ms-excel", true},
		{"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", true},
		// OpenDocument
		{"application/vnd.oasis.opendocument.text", true},
		{"application/vnd.oasis.opendocument.spreadsheet", true},
		{"application/vnd.oasis.opendocument.presentation", true},
		// RTF
		{"application/rtf", true},
		{"text/rtf", true},
		{"text/csv", true},
		// Deliberately out of scope
		{"application/pdf", false},      // suchi has its own PDF pipeline
		{"image/heic", false},           // imagemagick path
		{"application/epub+zip", false}, // existing epub plugin owns this
		{"application/octet-stream", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := Handles(tc.mime); got != tc.want {
			t.Errorf("Handles(%q) = %v, want %v", tc.mime, got, tc.want)
		}
	}
}
