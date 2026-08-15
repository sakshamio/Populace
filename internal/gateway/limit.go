package gateway

import (
	"sync"
	"time"
)

// Limiter is a per-client token bucket.
//
// Rate limiting is not the same control as admission control and both are
// needed. Admission caps concurrency so the model server stays at its measured
// optimum; the limiter caps a single client's share so one runaway worker
// cannot spend the whole hour's capacity while everyone else is shed.
type Limiter struct {
	perMin int
	mu     sync.Mutex
	b      map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func NewLimiter(perMin int) *Limiter {
	return &Limiter{perMin: perMin, b: make(map[string]*bucket)}
}

func (l *Limiter) Allow(client string) bool {
	if l.perMin <= 0 {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	bk, ok := l.b[client]
	if !ok {
		// Start full so a client's first request is never rejected -- an
		// empty initial bucket makes a fresh deploy look broken.
		bk = &bucket{tokens: float64(l.perMin), last: now}
		l.b[client] = bk
	}
	refill := now.Sub(bk.last).Minutes() * float64(l.perMin)
	bk.tokens = minF(float64(l.perMin), bk.tokens+refill)
	bk.last = now
	if bk.tokens < 1 {
		return false
	}
	bk.tokens--
	return true
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
