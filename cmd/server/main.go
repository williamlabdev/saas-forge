package main

import (
	"context"
	"log"
	"os"
	"os/signal"
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
	defer application.Pool.Close()

	bootstrapAdmin(ctx, application.IAM, rt)

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	go application.Worker.Run(workerCtx, application.Runtime.OutboxPollInterval)
	log.Printf("outbox worker started (poll=%s)", application.Runtime.OutboxPollInterval)

	// Folds buffered public delivery reads into the daily bucket. Shares the
	// worker context so shutdown flushes the final window (ADR-004 amendment).
	const deliveryFlushInterval = 30 * time.Second
	go application.DeliveryCounter.RunFlusher(workerCtx, application.ContentRepo, deliveryFlushInterval, func(err error) {
		log.Printf("delivery usage flush: %v", err)
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

	cancelWorker()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := application.Server.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
