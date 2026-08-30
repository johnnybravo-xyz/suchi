package api

import (
	"sync"
	"time"
)

const (
	chatRequestsPerMinute = 6
	chatRequestBurst      = 2
	chatConcurrentCalls   = 2
)

type chatRateBucket struct {
	tokens float64
	last   time.Time
}

// chatGate protects both hosted-model spend and small local model runtimes.
// It is process-local by design; deployments that need a distributed quota
// can still place one at the reverse proxy.
type chatGate struct {
	mu      sync.Mutex
	buckets map[int64]chatRateBucket
	slots   chan struct{}
	now     func() time.Time
}

func newChatGate() *chatGate {
	return &chatGate{
		buckets: make(map[int64]chatRateBucket),
		slots:   make(chan struct{}, chatConcurrentCalls),
		now:     time.Now,
	}
}

func (g *chatGate) enter(userID int64) (release func(), retryAfter time.Duration, ok bool) {
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

	select {
	case g.slots <- struct{}{}:
		return func() { <-g.slots }, 0, true
	default:
		return nil, time.Second, false
	}
}
