package domainapi

import (
	"context"
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
