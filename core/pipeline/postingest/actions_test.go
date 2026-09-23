package postingest

import (
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/automations"
)

func testActions(t *testing.T) *automations.Registry {
	t.Helper()
	registry, err := automations.NewRegistry(automations.BuiltinActions())
	if err != nil {
		t.Fatal(err)
	}
	return registry
}
