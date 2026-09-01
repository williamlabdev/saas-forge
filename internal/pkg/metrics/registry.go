package metrics

import (
	"fmt"
	"math"
	"net/http"
	"sync/atomic"
	"time"
)

// Registry holds lightweight counters exposed at GET /metrics (Prometheus text, no client lib).
type Registry struct {
	startedAt time.Time

	AuthLoginSuccess atomic.Uint64
	AuthLoginFailure atomic.Uint64
	AuthRateLimited  atomic.Uint64
	// AgentRateLimited counts requests refused by the per-agent limiter
	// (ADR-013 補裁 S-2 題一). Separate from AuthRateLimited because they answer
	// different questions: that one is "is someone brute-forcing login", this
	// one is "is an agent misbehaving, or is the cap set too low" — and folding
	// them together would make either unreadable.
	AgentRateLimited atomic.Uint64

	OutboxDelivered atomic.Uint64
	OutboxFailed    atomic.Uint64
	OutboxRetried   atomic.Uint64
	OutboxDead      atomic.Uint64

	outboxPending     atomic.Int64
	outboxLagBits     atomic.Uint64 // float64 bits for lag seconds
	outboxProcessing  atomic.Int64
	outboxProcLagBits atomic.Uint64 // float64 bits for oldest 'processing' lag seconds
}

func NewRegistry() *Registry {
	return &Registry{startedAt: time.Now().UTC()}
}

// SetOutboxGauges updates pending/processing counts and their lag (seconds) for
// /metrics. Processing count surfaces rows claimed but not yet marked, so a
// stale-processing backlog (TKT-OBX-1) is visible and alertable instead of silent.
func (r *Registry) SetOutboxGauges(pending int64, lagSeconds float64, processing int64, processingLagSeconds float64) {
	if r == nil {
		return
	}
	r.outboxPending.Store(pending)
	r.outboxLagBits.Store(math.Float64bits(clampNonNegative(lagSeconds)))
	r.outboxProcessing.Store(processing)
	r.outboxProcLagBits.Store(math.Float64bits(clampNonNegative(processingLagSeconds)))
}

func clampNonNegative(v float64) float64 {
	if v < 0 || math.IsNaN(v) {
		return 0
	}
	return v
}

func (r *Registry) outboxLagSeconds() float64 {
	return math.Float64frombits(r.outboxLagBits.Load())
}

func (r *Registry) outboxProcessingLagSeconds() float64 {
	return math.Float64frombits(r.outboxProcLagBits.Load())
}

// Handler serves Prometheus-compatible text exposition.
func (r *Registry) Handler() http.Handler {
	if r == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		uptime := time.Since(r.startedAt).Seconds()
		_, _ = fmt.Fprintf(w, "# HELP process_uptime_seconds Process uptime\n"+
			"# TYPE process_uptime_seconds gauge\n"+
			"process_uptime_seconds %.3f\n", uptime)
		writeCounter(w, "auth_login_success_total", "Successful logins", r.AuthLoginSuccess.Load())
		writeCounter(w, "auth_login_failure_total", "Failed logins", r.AuthLoginFailure.Load())
		writeCounter(w, "auth_login_rate_limited_total", "Login attempts blocked by rate limit", r.AuthRateLimited.Load())
		writeCounter(w, "agent_rate_limited_total", "Agent requests blocked by the per-agent rate limit", r.AgentRateLimited.Load())
		writeCounter(w, "outbox_delivered_total", "Outbox events delivered", r.OutboxDelivered.Load())
		writeCounter(w, "outbox_failed_total", "Outbox delivery failures", r.OutboxFailed.Load())
		writeCounter(w, "outbox_retried_total", "Outbox rows scheduled for retry", r.OutboxRetried.Load())
		writeCounter(w, "outbox_dead_total", "Outbox rows moved to dead letter", r.OutboxDead.Load())
		writeGauge(w, "outbox_pending", "Outbox rows awaiting delivery", float64(r.outboxPending.Load()))
		writeGauge(w, "outbox_lag_seconds", "Age in seconds of oldest pending outbox row", r.outboxLagSeconds())
		writeGauge(w, "outbox_processing", "Outbox rows claimed but not yet marked done/failed", float64(r.outboxProcessing.Load()))
		writeGauge(w, "outbox_processing_lag_seconds", "Age in seconds of oldest processing outbox row", r.outboxProcessingLagSeconds())
	})
}

func writeCounter(w http.ResponseWriter, name, help string, value uint64) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, value)
}

func writeGauge(w http.ResponseWriter, name, help string, value float64) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %.3f\n", name, help, name, name, value)
}
