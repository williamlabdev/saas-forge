package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/williamlabdev/saas-forge/internal/pkg/config"
	"github.com/williamlabdev/saas-forge/internal/pkg/requestctx"
)

func main() {
	cfg, err := config.LoadUserFromEnv()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	rt := config.LoadRuntimeFromEnv()
	// Validate runtime config explicitly here rather than relying on a hand-patched
	// wire_gen.go: validateRuntime returns only error, which wire cannot wire as a
	// provider, so `make wire` would otherwise drop this guard on regeneration.
	if err := validateRuntime(rt); err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := config.ValidateUserSecrets(cfg, rt.IsProduction()); err != nil {
		log.Fatalf("config: %v", err)
	}
	requestctx.SetTrustProxyHeaders(rt.TrustProxyHeaders)
	config.LogProductionWarnings(rt)

	ctx := context.Background()
	application, err := InitializeApp(ctx, cfg, rt)
	if err != nil {
		log.Fatalf("wire: %v", err)
	}
	// NOT `defer application.Pool.Close()`, and the difference is the whole of
	// the shutdown fix below. A deferred close runs the instant main returns,
	// which is BEFORE the three background goroutines have necessarily noticed
	// their context was cancelled — so the last thing a shutting-down process
	// does is hand a closed pool to a worker that is mid-statement. The visible
	// symptom is a log line about a closed pool on every clean restart; the
	// invisible one is a delivery-usage flush that was about to write the final
	// window and never did.
	//
	// Closed explicitly at the end instead, AFTER the goroutines have been
	// waited for. It is not deferred at all, because every early exit above this
	// point is a log.Fatalf, which does not run defers either way.

	bootstrapAdmin(ctx, application.IAM, rt)

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	// One WaitGroup for the three long-lived background loops. The HTTP server
	// is deliberately NOT in it: it is stopped by Shutdown, which has its own
	// wait built in, and adding it here would mean waiting twice for the same
	// thing under two different deadlines.
	var workers sync.WaitGroup
	goWorker(&workers, func() {
		application.Worker.Run(workerCtx, application.Runtime.OutboxPollInterval)
	})
	log.Printf("outbox worker started (poll=%s)", application.Runtime.OutboxPollInterval)

	// Scheduled publish/unpublish (ADR-017). Same context as the outbox worker,
	// so one cancelWorker stops both: a schedule half-executed against a
	// closing pool is worse than one that stays pending, and the worker leaves
	// it pending precisely because the context is cancelled.
	goWorker(&workers, func() {
		application.Scheduler.Run(workerCtx, application.Runtime.SchedulePollInterval)
	})
	log.Printf("content scheduler started (poll=%s)", application.Runtime.SchedulePollInterval)

	// Image renditions (ADR-019). Only when media is configured — the provider
	// returns nil otherwise, and a worker with no bucket has nothing to do.
	// Same context as the others: a render interrupted by shutdown rolls its
	// claim back and is simply pending on the next start.
	if application.MediaTransform != nil {
		goWorker(&workers, func() {
			application.MediaTransform.Run(workerCtx, application.Runtime.MediaTransformPollInterval)
		})
		log.Printf("media transform worker started (poll=%s)", application.Runtime.MediaTransformPollInterval)
	}

	// Folds buffered public delivery reads into the daily bucket. Shares the
	// worker context so shutdown flushes the final window (ADR-004 amendment).
	const deliveryFlushInterval = 30 * time.Second
	goWorker(&workers, func() {
		application.DeliveryCounter.RunFlusher(workerCtx, application.ContentRepo, deliveryFlushInterval, func(err error) {
			log.Printf("delivery usage flush: %v", err)
		})
	})

	go func() {
		log.Printf("listening on %s (authz=%s)", application.Server.Addr, application.Runtime.AuthzMode)
		if err := application.Server.ListenAndServe(); err != nil && err.Error() != "http: Server closed" {
			log.Fatalf("server: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	// ORDER: stop accepting, then stop the workers, then close the pool.
	//
	// cancelWorker first so the background loops start unwinding while Shutdown
	// drains the in-flight requests — the two waits overlap instead of adding
	// up. Shutdown next, because a request still in flight needs the pool.
	// waitForWorkers last, and only then the pool: a pool closed while any of
	// the three is mid-statement produces errors on a shutdown that was
	// otherwise clean, which is the failure this ordering exists to remove.
	cancelWorker()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := application.Server.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	if !waitForWorkers(&workers, shutdownTimeout) {
		// Reported, not fatal, and the pool is closed anyway below. A worker
		// that will not stop is a bug worth a line in the log; refusing to exit
		// over it would turn that bug into a process an operator has to kill,
		// and the supervisor's own timeout would end it less gracefully than
		// this does.
		log.Printf("shutdown: background workers did not finish within %s", shutdownTimeout)
	}
	application.Pool.Close()
}

// shutdownTimeout bounds BOTH halves of shutdown independently — the HTTP drain
// and the worker wait each get the whole of it. They are sequential, so the
// worst case is twice this, which is the honest trade: a single shared deadline
// would mean a slow drain silently leaving no time at all to wait for the
// workers, and the wait is the half that protects the pool.
const shutdownTimeout = 10 * time.Second

// goWorker starts a background loop and registers it with the WaitGroup.
//
// The Add-before-go and the deferred Done are the two halves nobody may
// separate: Add inside the goroutine races with the Wait, and a Done that is not
// deferred is skipped by any panic, leaving shutdown to block for the whole
// timeout every time. Wrapping both in one function is what makes a fourth
// worker impossible to add wrongly — `go application.Whatever.Run(...)` is the
// shape this replaces, and it registered nothing at all.
func goWorker(wg *sync.WaitGroup, run func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		run()
	}()
}

// waitForWorkers blocks until every registered worker has returned, or until the
// timeout expires. It reports whether they all finished.
//
// A BOUNDED wait, not wg.Wait(). An unbounded one turns a single stuck worker
// into a process that never exits — the supervisor eventually SIGKILLs it, and
// SIGKILL is exactly the ungraceful ending the whole shutdown path exists to
// avoid. Bounding it means the bad case degrades to what the code did BEFORE
// this change (close the pool under a running worker) instead of to something
// worse.
//
// Extracted from main so it can be tested: main itself blocks on a signal and
// cannot be called from a test at all, and "cancel, then wait, then close" is
// precisely the ordering worth pinning.
func waitForWorkers(wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
