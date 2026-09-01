package api

import (
	"sync"
	"time"
)

const (
	chatRequestsPerMinute = 6
	chatRequestBurst      = 2
	chatConcurrentReads   = 2
	chatConcurrentCalls   = 2
)

type chatRateBucket struct {
	tokens float64
	last   time.Time
}

// chatGate bounds retrieval load, hosted spend, and small local runtimes.
// It is process-local by design; deployments that need a distributed quota
// can still place one at the reverse proxy.
type chatGate struct {
	mu      sync.Mutex
	buckets map[int64]chatRateBucket
	reads   chan struct{}
	slots   chan struct{}
	now     func() time.Time
}

func newChatGate() *chatGate {
	return &chatGate{
		buckets: make(map[int64]chatRateBucket),
		reads:   make(chan struct{}, chatConcurrentReads),
		slots:   make(chan struct{}, chatConcurrentCalls),
		now:     time.Now,
	}
}

// Bound multi-pass FTS globally and leave half the read pool for other APIs.
func (g *chatGate) acquireRetrieval() (release func(), retryAfter time.Duration, ok bool) {
	if g == nil {
		return func() {}, 0, true
	}
	select {
	case g.reads <- struct{}{}:
		return func() { <-g.reads }, 0, true
	default:
		return nil, time.Second, false
	}
}

// Reserve before retrieval; refund requests that never reach a provider.
func (g *chatGate) admit(userID int64) (refund func(), retryAfter time.Duration, ok bool) {
	if g == nil {
		return func() {}, 0, true
	}

	now := g.now()
	g.mu.Lock()
	bucket, exists := g.buckets[userID]
	if !exists {
		bucket = chatRateBucket{tokens: chatRequestBurst, last: now}
	}
	elapsed := now.Sub(bucket.last).Minutes()
	bucket.tokens = min(float64(chatRequestBurst), bucket.tokens+elapsed*chatRequestsPerMinute)
	bucket.last = now
	if bucket.tokens < 1 {
		missing := 1 - bucket.tokens
		retryAfter = time.Duration(missing/float64(chatRequestsPerMinute)*float64(time.Minute)) + time.Second
		g.buckets[userID] = bucket
		g.mu.Unlock()
		return nil, retryAfter, false
	}
	bucket.tokens--
	g.buckets[userID] = bucket
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			now := g.now()
			g.mu.Lock()
			bucket := g.buckets[userID]
			elapsed := now.Sub(bucket.last).Minutes()
			bucket.tokens = min(float64(chatRequestBurst),
				bucket.tokens+elapsed*chatRequestsPerMinute+1)
			bucket.last = now
			g.buckets[userID] = bucket
			g.mu.Unlock()
		})
	}, 0, true
}

// Keep slow archive reads from occupying live-provider slots.
func (g *chatGate) acquireProvider() (release func(), retryAfter time.Duration, ok bool) {
	if g == nil {
		return func() {}, 0, true
	}

	select {
	case g.slots <- struct{}{}:
		return func() { <-g.slots }, 0, true
	default:
		return nil, time.Second, false
	}
}
