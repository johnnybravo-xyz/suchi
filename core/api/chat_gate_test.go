package api

import (
	"testing"
	"time"
)

func TestChatGateBoundsRateAndProviderConcurrency(t *testing.T) {
	now := time.Unix(100, 0)
	gate := newChatGate()
	gate.now = func() time.Time { return now }

	releaseOne, _, ok := gate.acquireProvider()
	if !ok {
		t.Fatal("first provider call was refused")
	}
	releaseTwo, _, ok := gate.acquireProvider()
	if !ok {
		t.Fatal("second provider call was refused")
	}
	if _, retry, ok := gate.acquireProvider(); ok || retry <= 0 {
		t.Fatalf("third provider call ok=%t retry=%s", ok, retry)
	}
	releaseOne()
	releaseTwo()

	for i := range chatRequestBurst {
		_, _, ok := gate.admit(9)
		if !ok {
			t.Fatalf("burst request %d was refused", i)
		}
	}
	if _, retry, ok := gate.admit(9); ok || retry <= 0 {
		t.Fatalf("request beyond burst ok=%t retry=%s", ok, retry)
	}
	now = now.Add(10 * time.Second)
	_, _, ok = gate.admit(9)
	if !ok {
		t.Fatal("refill did not admit a request")
	}
}

func TestChatGateBoundsRetrievalsAndRefundsReservation(t *testing.T) {
	now := time.Unix(200, 0)
	gate := newChatGate()
	gate.now = func() time.Time { return now }

	releaseOne, _, ok := gate.acquireRetrieval()
	if !ok {
		t.Fatal("first retrieval was refused")
	}
	releaseTwo, _, ok := gate.acquireRetrieval()
	if !ok {
		t.Fatal("second retrieval was refused")
	}
	if _, retry, ok := gate.acquireRetrieval(); ok || retry <= 0 {
		t.Fatalf("third retrieval ok=%t retry=%s", ok, retry)
	}
	releaseOne()
	releaseTwo()

	for i := 0; i < chatRequestBurst+2; i++ {
		refund, _, ok := gate.admit(11)
		if !ok {
			t.Fatalf("refunded reservation %d was refused", i)
		}
		refund()
		refund() // refunds are idempotent
	}
}

func TestChatGatePrunesOnlyFullyRefilledBuckets(t *testing.T) {
	now := time.Unix(300, 0)
	gate := newChatGate()
	gate.now = func() time.Time { return now }

	staleRefund, _, ok := gate.admit(1)
	if !ok {
		t.Fatal("stale bucket reservation was refused")
	}
	if _, _, ok := gate.admit(2); !ok {
		t.Fatal("active bucket reservation was refused")
	}

	now = now.Add(chatBucketPruneEvery - 10*time.Second)
	for i := range chatRequestBurst {
		if _, _, ok := gate.admit(2); !ok {
			t.Fatalf("active bucket refill request %d was refused", i)
		}
	}
	now = now.Add(10 * time.Second)
	if _, _, ok := gate.admit(3); !ok {
		t.Fatal("request triggering pruning was refused")
	}

	if _, exists := gate.buckets[1]; exists {
		t.Fatal("idle bucket was not pruned")
	}
	if _, exists := gate.buckets[2]; !exists {
		t.Fatal("recent bucket was pruned")
	}
	if len(gate.buckets) != 2 {
		t.Fatalf("bucket count = %d, want 2", len(gate.buckets))
	}

	if _, _, ok := gate.admit(2); !ok {
		t.Fatal("recent bucket lost its remaining token")
	}
	if _, retry, ok := gate.admit(2); ok || retry <= 0 {
		t.Fatalf("recent bucket was reset: ok=%t retry=%s", ok, retry)
	}

	staleRefund()
	for i := range chatRequestBurst {
		if _, _, ok := gate.admit(1); !ok {
			t.Fatalf("refunded pruned bucket request %d was refused", i)
		}
	}
	if _, retry, ok := gate.admit(1); ok || retry <= 0 {
		t.Fatalf("refunded pruned bucket exceeded burst: ok=%t retry=%s", ok, retry)
	}
}
