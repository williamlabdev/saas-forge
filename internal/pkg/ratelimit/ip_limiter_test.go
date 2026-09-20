package ratelimit

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestIPLimiter_Allow(t *testing.T) {
	l := NewIPLimiter(2, time.Minute)
	assert.True(t, l.Allow("1.2.3.4"))
	assert.True(t, l.Allow("1.2.3.4"))
	assert.False(t, l.Allow("1.2.3.4"))
	assert.True(t, l.Allow("5.6.7.8"))
}

func TestIPLimiter_Disabled(t *testing.T) {
	l := NewIPLimiter(0, time.Minute)
	for i := 0; i < 5; i++ {
		assert.True(t, l.Allow("x"))
	}
}

// A caller-controlled key space (public delivery keyed by tenant slug) must not
// let the map grow without bound: expired windows are swept on insert.
func TestIPLimiter_SweepsExpiredWindows(t *testing.T) {
	l := NewIPLimiter(5, 10*time.Millisecond)
	for i := 0; i < sweepThreshold+50; i++ {
		l.Allow(fmt.Sprintf("key-%d", i))
	}
	time.Sleep(20 * time.Millisecond) // every window above is now expired

	// One more insert past the threshold triggers the sweep.
	l.Allow("trigger")

	l.mu.Lock()
	size := len(l.seen)
	l.mu.Unlock()
	if size > sweepThreshold {
		t.Fatalf("map size %d did not shrink — expired windows are being retained", size)
	}
}

// Sweeping must not drop windows that are still counting.
func TestIPLimiter_SweepKeepsLiveWindows(t *testing.T) {
	l := NewIPLimiter(2, time.Hour)
	// Consume the window deliberately one call at a time — a short-circuiting
	// || would skip the second call whenever the first failed, leaving the
	// count at 1 and making the assertion below meaningless.
	for i := range 2 {
		if !l.Allow("live") {
			t.Fatalf("request %d should be allowed", i)
		}
	}
	for i := 0; i < sweepThreshold+10; i++ {
		l.Allow(fmt.Sprintf("filler-%d", i))
	}
	if l.Allow("live") {
		t.Fatal("live window was swept — its count must survive the sweep")
	}
}
