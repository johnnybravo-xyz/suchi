// SPDX-License-Identifier: AGPL-3.0-or-later

package tessocr

import (
	"strings"
	"testing"
)

func TestClassifyLanguagesReportsConfiguredPacks(t *testing.T) {
	output := []byte("List of available languages in /usr/share/tessdata (4):\nosd\neng\nscript/Devanagari\neng\n")
	available, missing, err := classifyLanguages(output, []string{"eng", "nep", "deu", "nep"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(available, ","); got != "eng,osd,script/Devanagari" {
		t.Fatalf("available = %q", got)
	}
	if got := strings.Join(missing, ","); got != "deu,nep" {
		t.Fatalf("missing = %q", got)
	}
}

func TestClassifyLanguagesRejectsEmptyListing(t *testing.T) {
	if _, _, err := classifyLanguages([]byte("List of available languages (0):\n"), []string{"eng"}); err == nil {
		t.Fatal("empty language listing succeeded")
	}
}
