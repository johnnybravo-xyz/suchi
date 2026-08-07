package lang

import (
	"log/slog"
	"sync"
)

// Chain composes an ordered list of Detectors. BestAbove walks
// them in registration order, returning the first result with
// Confidence > threshold. Detectors that error are logged and
// skipped — one flaky detector never fails the whole ingest.
//
// Zero-value Chain is a valid empty chain (BestAbove returns
// ok=false). Detectors are typically registered at boot from
// main.go; Register is safe to call from init() as long as it
// doesn't race with a live BestAbove call — production layout
// always registers before serve, so the mutex is defensive.
type Chain struct {
	mu        sync.RWMutex
	detectors []Detector
	log       *slog.Logger
}

// NewChain constructs a chain over the given detectors (in
// order). Pass nil for log to disable per-detector-error
// logging.
func NewChain(log *slog.Logger, dets ...Detector) *Chain {
	return &Chain{
		detectors: append([]Detector(nil), dets...),
		log:       log,
	}
}

// Register appends a detector to the chain. Plugins call this
// at boot to slot themselves in behind the built-in metadata
// hints.
func (c *Chain) Register(d Detector) {
	if d == nil {
		return
	}
	c.mu.Lock()
	c.detectors = append(c.detectors, d)
	c.mu.Unlock()
}

// Len returns the number of registered detectors. Useful for
// boot-time logging: "language detection: N sources registered".
func (c *Chain) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.detectors)
}

// BestAbove walks detectors in order and returns the first
// Result whose Confidence > threshold and whose Code is
// non-empty. Second value is false when nothing cleared the
// bar — the caller should leave documents.languages empty in
// that case, not stamp a low-confidence guess.
func (c *Chain) BestAbove(text string, threshold float64) (Result, bool) {
	c.mu.RLock()
	dets := append([]Detector(nil), c.detectors...)
	c.mu.RUnlock()

	for _, d := range dets {
		results, err := d.Detect(text)
		if err != nil {
			if c.log != nil {
				c.log.Debug("lang.detect.error", "detector", d.Name(), "err", err.Error())
			}
			continue
		}
		for _, r := range results {
			if r.Code != "" && r.Confidence > threshold {
				if c.log != nil {
					c.log.Debug("lang.detect.match",
						"detector", d.Name(), "code", r.Code, "confidence", r.Confidence)
				}
				return r, true
			}
		}
	}
	return Result{}, false
}
