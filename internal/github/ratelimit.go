package github

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Limiter tracks one GitHub rate-limit resource ("core", "graphql") from the
// X-RateLimit-* response headers. It hands out request slots, keeps a reserve for
// other callers, and parks callers until the window resets when the budget is spent.
type Limiter struct {
	mu        sync.Mutex
	limit     int
	remaining int // -1 until the first response says otherwise
	reset     time.Time
	inflight  int
	reserve   int

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// NewLimiter returns a limiter that never spends the last reserve requests.
func NewLimiter(reserve int) *Limiter {
	return &Limiter{remaining: -1, reserve: reserve, now: time.Now, sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Acquire takes one request slot. It waits for the window reset when the remaining
// budget, less the requests already in flight, is at or below the reserve.
func (l *Limiter) Acquire(ctx context.Context) error {
	for {
		l.mu.Lock()
		now := l.now()
		if l.remaining >= 0 && !l.reset.IsZero() && !now.Before(l.reset) {
			// The window has rolled over; the next response will say by how much.
			l.remaining = -1
		}
		if l.remaining < 0 || l.remaining-l.inflight > l.reserve {
			l.inflight++
			l.mu.Unlock()
			return nil
		}
		wait := l.reset.Sub(now) + time.Second
		if l.reset.IsZero() {
			wait = time.Minute
		}
		l.mu.Unlock()
		if err := l.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// Release returns a slot and records the headers of its response (nil if the
// request failed before a response).
func (l *Limiter) Release(h http.Header) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight > 0 {
		l.inflight--
	}
	l.observeLocked(h)
}

func (l *Limiter) observeLocked(h http.Header) {
	if h == nil {
		return
	}
	rem, err1 := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	rst, err2 := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	if err1 != nil || err2 != nil {
		return
	}
	reset := time.Unix(rst, 0)
	// Responses arrive out of order; keep the newest window and its lowest count.
	if reset.After(l.reset) || l.remaining < 0 || rem < l.remaining {
		l.remaining = rem
		l.reset = reset
	}
	if lim, err := strconv.Atoi(h.Get("X-RateLimit-Limit")); err == nil {
		l.limit = lim
	}
}

// Budget is how many more requests may start now without touching the reserve.
// It is -1 while the budget is unknown.
func (l *Limiter) Budget() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.remaining < 0 || (!l.reset.IsZero() && !l.now().Before(l.reset)) {
		return -1
	}
	return max(0, l.remaining-l.inflight-l.reserve)
}

// Chunk is the size of the next batch of requests: as many as fit in one buffer of
// the double-buffered writer, but never more than the rate-limit budget allows. With
// the budget spent it returns 1, and that request parks in Acquire until the reset.
func (l *Limiter) Chunk(bufferCap int) int {
	b := l.Budget()
	if b < 0 {
		// Unknown budget: probe with one request before committing to a chunk.
		return 1
	}
	return max(1, min(bufferCap, b))
}

// Snapshot is the limiter's state for /api/v1/status.
type Snapshot struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Reset     time.Time `json:"reset"`
	InFlight  int       `json:"in_flight"`
	Reserve   int       `json:"reserve"`
}

func (l *Limiter) Snapshot() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Snapshot{l.limit, l.remaining, l.reset, l.inflight, l.reserve}
}
