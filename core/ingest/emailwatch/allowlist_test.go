package emailwatch_test

import (
	"testing"

	"github.com/johnnybravo-xyz/suchi/core/ingest/emailwatch"
)

func TestMatchAddressCriteria(t *testing.T) {
	tests := []struct {
		address, criteria string
		want              bool
	}{
		{"alice@example.com", "", true},
		{"", "alice@example.com", false},
		{"Alice <ALICE@example.com>", "alice@example.com", true},
		{"bills@bescom.co.in", "@BESCOM.CO.IN", true},
		{"a@sub.example.com", "@example.com", false},
		{"bob@x.io", "alice@example.com\nbob@x.io", true},
		{"random garbage", "Random Garbage", true},
		{"eve@evil.test", ",,alice@example.com,,", false},
	}
	for _, tt := range tests {
		if got := emailwatch.MatchAddressCriteria(tt.address, tt.criteria); got != tt.want {
			t.Fatalf("MatchAddressCriteria(%q, %q)=%v want %v", tt.address, tt.criteria, got, tt.want)
		}
	}
}
