// SPDX-License-Identifier: AGPL-3.0-or-later

package fswatch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/mimeutil"
)

// Bank exports often save "Key : value" text under an .xls name. The header
// shape must not turn a named document format into an email, while extensionless
// Maildir names still reach the email parser.
func TestEmailSniffDoesNotOverrideNamedFormat(t *testing.T) {
	source := t.TempDir()
	watcher, database := newScannerTestWatcher(t, source, false)
	cases := map[string]struct {
		body string
		want string
	}{
		"statement.xls": {
			body: "Account Name       : Example Holder\nAddress            : Example Street\nDate               : 1 Apr 2018\nBranch             : Example\n\nTxn Date\tDescription\tAmount\n",
			want: mimeutil.ApplicationMSExcel,
		},
		"1700000000.M1P1.example:2,S": {
			body: "From: sender@example.com\nTo: owner@example.com\nSubject: Statement\n\nBody\n",
			want: mimeutil.MessageRFC822,
		},
	}
	for name, tc := range cases {
		path := filepath.Join(source, name)
		if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		id, _, err := watcher.ingest(t.Context(), path, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got string
		if err := database.Read.QueryRow(`SELECT mime_type FROM documents WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("%s mime_type=%q, want %q", name, got, tc.want)
		}
	}
}
