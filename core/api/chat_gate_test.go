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
