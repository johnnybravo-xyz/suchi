package api

import (
	"testing"
	"time"
)

func TestChatGateBoundsRateAndConcurrency(t *testing.T) {
	now := time.Unix(100, 0)
	gate := newChatGate()
	gate.now = func() time.Time { return now }

	releaseOne, _, ok := gate.enter(1)
	if !ok {
		t.Fatal("first request was refused")
	}
	releaseTwo, _, ok := gate.enter(2)
	if !ok {
		t.Fatal("second concurrent request was refused")
	}
	if _, retry, ok := gate.enter(3); ok || retry <= 0 {
		t.Fatalf("third concurrent request ok=%t retry=%s", ok, retry)
	}
	releaseOne()
	releaseTwo()

	for i := range chatRequestBurst {
		release, _, ok := gate.enter(9)
		if !ok {
			t.Fatalf("burst request %d was refused", i)
		}
		release()
	}
	if _, retry, ok := gate.enter(9); ok || retry <= 0 {
		t.Fatalf("request beyond burst ok=%t retry=%s", ok, retry)
	}
	now = now.Add(10 * time.Second)
	release, _, ok := gate.enter(9)
	if !ok {
		t.Fatal("refill did not admit a request")
	}
	release()
}
