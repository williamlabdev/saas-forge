// Command delivery is the public content delivery edge (ADR-004 option A).
//
// It is the only internet-facing process in the platform. It holds a delivery
// signing key — never the main one — and mints a short-lived, tenant-scoped,
// read-only credential per request. Every safety property it depends on is
// ALSO enforced by the Domain API (published-only, no writes); the edge does
// not get to be trusted about any of them.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/williamlabdev/saas-forge/apps/delivery/internal/config"
	"github.com/williamlabdev/saas-forge/apps/delivery/internal/handler"
	"github.com/williamlabdev/saas-forge/apps/delivery/internal/upstream"
	"github.com/williamlabdev/saas-forge/internal/auth/jwt"
)

func main() {
	cfg := config.Load()
	if err := config.Validate(cfg); err != nil {
		log.Fatalf("delivery: %v", err)
	}

	// Only the delivery key is supplied — the main secret is deliberately absent
	// from this process, so it cannot mint anything but delivery credentials.
	signer := jwt.NewSigner(nil, cfg.TokenTTL).WithDeliveryKey(cfg.DeliveryJWTSecret)
	up := upstream.NewClient(cfg.DomainAPIURL, cfg.GatewaySecret, signer)
	h := handler.New(up, handler.NewLimiter(cfg.RateLimit, cfg.RateWindow), cfg.CacheMaxAge)

	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer, middleware.Logger)
	h.Routes(r)

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("delivery edge listening on %s (domain=%s, rate=%d/%s)",
			cfg.HTTPAddr, cfg.DomainAPIURL, cfg.RateLimit, cfg.RateWindow)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("delivery: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}
