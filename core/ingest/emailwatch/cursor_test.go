package emailwatch

import (
	"errors"
	"slices"
	"testing"
)

func TestNextUIDCheckpoint(t *testing.T) {
	tests := []struct {
		name               string
		last               uint32
		completed          []uint32
		failed             []uint32
		checkpointComplete bool
		want               uint32
	}{
		{name: "all complete", last: 4, completed: []uint32{5, 7, 6}, checkpointComplete: true, want: 7},
		{name: "earlier failure blocks later success", last: 4, completed: []uint32{5, 7}, failed: []uint32{6}, checkpointComplete: true, want: 5},
		{name: "first fetched message fails", last: 4, completed: []uint32{6, 7}, failed: []uint32{5}, checkpointComplete: true, want: 4},
		{name: "failure after completed messages", last: 4, completed: []uint32{5}, failed: []uint32{8}, checkpointComplete: true, want: 7},
		{name: "incomplete cycle keeps old cursor", last: 4, completed: []uint32{5, 6}, checkpointComplete: false, want: 4},
		{name: "empty result", last: 4, checkpointComplete: true, want: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextUIDCheckpoint(tt.last, tt.completed, tt.failed, tt.checkpointComplete); got != tt.want {
				t.Fatalf("checkpoint = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestConcurrentDeleteFetchError(t *testing.T) {
	if !isConcurrentDeleteFetchError(errors.New("Some of the requested messages no longer exist.")) {
		t.Fatal("Outlook concurrent deletion should be ignored")
	}
	if isConcurrentDeleteFetchError(errors.New("connection reset by peer")) {
		t.Fatal("network error must remain a mailbox failure")
	}
}

func TestMissingUIDs(t *testing.T) {
	got := missingUIDs(
		[]uint32{11, 12, 13, 14},
		[]uint32{11, 14},
		[]uint32{13},
	)
	if want := []uint32{12}; !slices.Equal(got, want) {
		t.Fatalf("missing UIDs = %v, want %v", got, want)
	}

	vanished, retry := partitionMissingUIDs(
		[]uint32{12, 15, 18},
		[]uint32{15, 16, 18},
	)
	if !slices.Equal(vanished, []uint32{12}) || !slices.Equal(retry, []uint32{15, 18}) {
		t.Fatalf("partitioned missing UIDs: vanished=%v retry=%v", vanished, retry)
	}
}
