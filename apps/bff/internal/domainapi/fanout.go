package domainapi

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"
)

// FanoutStats accumulates the Domain API calls made while serving one BFF
// request. ADR-002 defers gRPC until fan-out is a *measured* problem — this is
// the measurement: the /graphql handler attaches a collector to the request
// context and logs it after the resolvers run (see cmd/bff).
type FanoutStats struct {
	calls       atomic.Int64
	domainNanos atomic.Int64
}

// Calls is the number of Domain API round-trips recorded so far.
func (s *FanoutStats) Calls() int64 { return s.calls.Load() }

// DomainDuration is the summed wall-clock time spent in Domain API calls.
// Resolvers run sequentially today, so this approximates the request's
// domain-bound latency; revisit if resolvers become concurrent.
func (s *FanoutStats) DomainDuration() time.Duration {
	return time.Duration(s.domainNanos.Load())
}

func (s *FanoutStats) record(d time.Duration) {
	s.calls.Add(1)
	s.domainNanos.Add(int64(d))
}

type fanoutKey struct{}

// WithFanout returns a context carrying a fresh collector plus the collector
// itself. Client.do records into it; requests without one are unaffected.
func WithFanout(ctx context.Context) (context.Context, *FanoutStats) {
	s := &FanoutStats{}
	return context.WithValue(ctx, fanoutKey{}, s), s
}

func fanoutFromContext(ctx context.Context) *FanoutStats {
	s, _ := ctx.Value(fanoutKey{}).(*FanoutStats)
	return s
}

// Process-level aggregates backing BFF GET /metrics (spec 001).
// Counters (never reset except in tests), same hand-rolled Prometheus-text
// style as internal/pkg/metrics — no client lib on purpose.
var (
	bffGraphqlRequests   atomic.Int64
	bffDomainFanoutCalls atomic.Int64
	bffDomainSecondsBits atomic.Uint64 // float64 bits
)

// RecordGraphQLRequest folds one request's collector into the process
// counters. Nil-safe: a request without a collector still counts as one
// GraphQL request with zero domain calls.
func RecordGraphQLRequest(s *FanoutStats) {
	bffGraphqlRequests.Add(1)
	if s == nil {
		return
	}
	if calls := s.Calls(); calls > 0 {
		bffDomainFanoutCalls.Add(calls)
	}
	if d := s.DomainDuration(); d > 0 {
		for {
			old := bffDomainSecondsBits.Load()
			next := math.Float64bits(math.Float64frombits(old) + d.Seconds())
			if bffDomainSecondsBits.CompareAndSwap(old, next) {
				break
			}
		}
	}
}

// RenderMetrics returns the BFF counters in Prometheus text exposition format.
func RenderMetrics() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# HELP bff_graphql_requests_total Total /graphql requests served.\n")
	fmt.Fprintf(&b, "# TYPE bff_graphql_requests_total counter\n")
	fmt.Fprintf(&b, "bff_graphql_requests_total %d\n", bffGraphqlRequests.Load())
	fmt.Fprintf(&b, "# HELP bff_domain_fanout_calls_total Total Domain API round-trips across /graphql requests.\n")
	fmt.Fprintf(&b, "# TYPE bff_domain_fanout_calls_total counter\n")
	fmt.Fprintf(&b, "bff_domain_fanout_calls_total %d\n", bffDomainFanoutCalls.Load())
	fmt.Fprintf(&b, "# HELP bff_domain_seconds_total Total wall-clock seconds spent in Domain API calls.\n")
	fmt.Fprintf(&b, "# TYPE bff_domain_seconds_total counter\n")
	fmt.Fprintf(&b, "bff_domain_seconds_total %v\n", math.Float64frombits(bffDomainSecondsBits.Load()))
	return b.String()
}

func resetBFFMetrics() {
	bffGraphqlRequests.Store(0)
	bffDomainFanoutCalls.Store(0)
	bffDomainSecondsBits.Store(0)
}
