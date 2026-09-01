package ratelimit

import (
	"sync"
	"time"
)

// IPLimiter enforces a fixed-window request cap per IP (in-process; Redis in M1).
type IPLimiter struct {
	max    int
	window time.Duration
	mu     sync.Mutex
	seen   map[string]windowState
}

type windowState struct {
	count   int
	resetAt time.Time
}

// NewIPLimiter returns a limiter with max requests per window. max <= 0 disables limiting.
func NewIPLimiter(max int, window time.Duration) *IPLimiter {
	if max <= 0 {
		return &IPLimiter{max: 0}
	}
	if window <= 0 {
		window = time.Minute
	}
	return &IPLimiter{
		max:    max,
		window: window,
		seen:   make(map[string]windowState),
	}
}

// sweepThreshold is the map size at which Allow drops expired windows before
// inserting. Without it the map only ever grows: every distinct key is retained
// forever, so any endpoint whose key is caller-controlled (a public delivery
// path keyed by tenant slug, an unauthenticated login keyed by spoofed IP) lets
// a caller grow it without bound. Sweeping is O(n) but amortised — it runs only
// when the map is already large and only on insert.
const sweepThreshold = 1024

// Allow reports whether the key may proceed (and consumes one slot when true).
func (l *IPLimiter) Allow(key string) bool {
	if l == nil || l.max <= 0 || key == "" {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	st, ok := l.seen[key]
	if !ok && len(l.seen) >= sweepThreshold {
		for k, v := range l.seen {
			if now.After(v.resetAt) {
				delete(l.seen, k)
			}
		}
	}
	if !ok || now.After(st.resetAt) {
		l.seen[key] = windowState{count: 1, resetAt: now.Add(l.window)}
		return true
	}
	if st.count >= l.max {
		return false
	}
	st.count++
	l.seen[key] = st
	return true
}
