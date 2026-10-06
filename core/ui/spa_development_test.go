// SPDX-License-Identifier: AGPL-3.0-or-later
//go:build !embedded_ui

package ui

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestDevelopmentSPAPanicsWithoutFreshBundle(t *testing.T) {
	t.Setenv(uiDevRootEnv, t.TempDir())
	defer func() {
		failure := recover()
		if failure == nil {
			t.Fatal("RegisterSPA did not panic")
		}
		if message := fmt.Sprint(failure); !strings.Contains(message, "run `make ui`") {
			t.Fatalf("panic = %q, want make ui guidance", message)
		}
	}()
	new(Server).RegisterSPA(http.NewServeMux())
}
