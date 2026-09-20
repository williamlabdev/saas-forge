package main

import (
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	secret := os.Getenv("WEBHOOK_SECRET")
	if secret == "" {
		// The secret is shown exactly once, in the 201 from
		// POST /api/v1/content/webhooks. There is no way to read it back —
		// that is deliberate, not an oversight.
		log.Fatal("WEBHOOK_SECRET is required (it is the value the registration returned)")
	}
	edge := os.Getenv("DELIVERY_EDGE_URL")
	if edge == "" {
		edge = "http://localhost:4100"
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":9999"
	}

	mirror := NewMirror(edge)
	// 4096 delivery ids is far more than the outbox can have in flight within
	// its retry window; see deliveryLog for why it is bounded at all.
	h := NewHandler(secret, newDeliveryLog(4096), mirror.Apply)

	mux := http.NewServeMux()
	mux.Handle("/webhook", h)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	logf("webhook consumer on %s, edge %s", addr, edge)
	// ReadHeaderTimeout is the one http.Server field a receiver cannot leave at
	// its zero value: without it a client can open a connection and dribble
	// header bytes forever, holding the slot for free (Slowloris). The sender's
	// own timeout is 10s, so nothing legitimate needs longer than this.
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
