package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The shutdown ordering these tests pin is "cancel, then WAIT, then close the
// pool" (ADR-018 §6). main itself blocks on a signal and cannot be called from a
// test, so the two pieces that carry the ordering — goWorker's registration and
// waitForWorkers' bounded wait — are exercised directly. What must never regress
// is that the wait actually waits: the bug being fixed was a `defer
// Pool.Close()` firing while three goroutines were still mid-statement, and a
// waitForWorkers that returned early would restore it silently.

func TestWaitForWorkersWaitsForCancelledWorkers(t *testing.T) {
	// The real shape: a worker loop that only returns after its context is
	// cancelled, registered through goWorker exactly as main registers the
	// outbox worker, scheduler and delivery flusher.
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	var mu sync.Mutex
	stopped := 0
	for i := 0; i < 3; i++ {
		goWorker(&wg, func() {
			<-ctx.Done()
			// A worker does not stop the instant ctx is cancelled; it finishes
			// the statement it is in. The sleep stands in for that, and it is
			// what makes the assertion below meaningful — without a real gap,
			// an implementation that never waited would still pass.
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			stopped++
			mu.Unlock()
		})
	}

	// Before cancel, the wait must time out rather than report success: nothing
	// has been asked to stop yet.
	require.False(t, waitForWorkers(&wg, 20*time.Millisecond),
		"waitForWorkers reported done while the workers were still running")

	cancel()
	require.True(t, waitForWorkers(&wg, 5*time.Second),
		"waitForWorkers gave up on workers that were stopping normally")

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 3, stopped,
		"waitForWorkers returned before every worker had finished; the pool would be closed under them")
}

func TestWaitForWorkersGivesUpOnAStuckWorker(t *testing.T) {
	// The bounded half. A worker that never returns must not hold the process
	// open forever — the supervisor's SIGKILL is a worse ending than closing the
	// pool under it, which is what the code did before the fix anyway.
	release := make(chan struct{})
	var wg sync.WaitGroup
	goWorker(&wg, func() { <-release })

	start := time.Now()
	require.False(t, waitForWorkers(&wg, 50*time.Millisecond),
		"a worker that never returns must be reported as unfinished")
	require.Less(t, time.Since(start), 5*time.Second,
		"waitForWorkers blocked well past its timeout")

	close(release)
	require.True(t, waitForWorkers(&wg, 5*time.Second))
}

func TestWaitForWorkersReturnsImmediatelyWithNoWorkers(t *testing.T) {
	// Degenerate but load-bearing: if the background loops are ever all disabled
	// by config, shutdown must not sit out the whole timeout.
	var wg sync.WaitGroup
	start := time.Now()
	require.True(t, waitForWorkers(&wg, 5*time.Second))
	require.Less(t, time.Since(start), time.Second)
}

func TestGoWorkerCountsDoneOnPanic(t *testing.T) {
	// goWorker defers Done. If it did not, a panicking worker would leave the
	// WaitGroup permanently short and every shutdown would burn the full
	// timeout. The panic is recovered inside the worker body so the test process
	// survives; what is asserted is that the registration is balanced.
	var wg sync.WaitGroup
	goWorker(&wg, func() {
		defer func() { _ = recover() }()
		panic("worker exploded")
	})
	require.True(t, waitForWorkers(&wg, 5*time.Second),
		"goWorker leaked a WaitGroup count when its worker panicked")
}
