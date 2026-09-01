package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/williamlabdev/saas-forge/apps/bff/graph"
	"github.com/williamlabdev/saas-forge/apps/bff/internal/config"
	"github.com/williamlabdev/saas-forge/apps/bff/internal/domainapi"
)

func main() {
	cfg := config.Load()
	client := domainapi.NewClientWithGateway(cfg.DomainAPIURL, cfg.GatewaySecret)
	resolver := &graph.Resolver{Client: client}
	srv := handler.NewDefaultServer(graph.NewExecutableSchema(graph.Config{Resolvers: resolver}))

	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer, middleware.Logger)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   cfg.CORSOrigins,
		AllowedMethods:   []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
		AllowCredentials: true,
	}))
	r.Use(authHeaderMiddleware)
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	if os.Getenv("BFF_PLAYGROUND") != "false" {
		r.Handle("/playground", playground.Handler("BFF GraphQL", "/graphql"))
	}
	r.Handle("/graphql", fanoutMetricsMiddleware(srv))

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("bff listening on %s (domain=%s)", cfg.HTTPAddr, cfg.DomainAPIURL)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("bff: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

func authHeaderMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if h := r.Header.Get("Authorization"); h != "" {
			ctx = context.WithValue(ctx, graph.AuthHeaderKey{}, h)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// fanoutMetricsMiddleware logs one line per /graphql request with the number
// of Domain API round-trips and where the time went. This is the data source
// for ADR-002's gRPC trigger condition 1 ("p95 BFF latency dominated by
// sequential domain round-trips — measure first").
func fanoutMetricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, stats := domainapi.WithFanout(r.Context())
		start := time.Now()
		next.ServeHTTP(w, r.WithContext(ctx))
		if calls := stats.Calls(); calls > 0 {
			total := time.Since(start)
			domain := stats.DomainDuration()
			log.Printf(
				"bff fanout: calls=%d domain_ms=%.1f total_ms=%.1f overhead_ms=%.1f",
				calls,
				float64(domain.Microseconds())/1000,
				float64(total.Microseconds())/1000,
				float64((total-domain).Microseconds())/1000,
			)
		}
	})
}
