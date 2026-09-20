package service

import (
	"context"
	"sync"
	"time"

	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
)

// DeliveryCounter accumulates public delivery reads per tenant in memory and
// folds them into the daily bucket periodically (ADR-004 amendment: load must
// be attributable to a tenant).
//
// It counts in memory on purpose. The delivery path is the read-optimised,
// CDN-cacheable one; a DB write per read would put a round-trip on every
// uncached public request and serialise same-tenant traffic on a single row.
//
// The honest trade-off: an unflushed window is lost if the process dies, so
// these numbers are usage VISIBILITY, not billing of record. Nothing that
// protects the platform depends on them — the rate limit lives at the edge and
// is independent.
type DeliveryCounter struct {
	mu     sync.Mutex
	counts map[string]int64
}

func NewDeliveryCounter() *DeliveryCounter {
	return &DeliveryCounter{counts: make(map[string]int64)}
}

// Record adds one read for a tenant. Safe on a nil receiver so wiring the
// counter stays optional (tests, deployments with delivery off).
func (c *DeliveryCounter) Record(tenantID string) {
	if c == nil || tenantID == "" {
		return
	}
	c.mu.Lock()
	c.counts[tenantID]++
	c.mu.Unlock()
}

// drain atomically takes the pending counts and resets the buffer.
func (c *DeliveryCounter) drain() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.counts) == 0 {
		return nil
	}
	out := c.counts
	c.counts = make(map[string]int64)
	return out
}

// Flush folds the pending counts into the daily buckets. A tenant whose write
// fails is PUT BACK so the next flush retries it — dropping on error would
// silently under-report exactly when the database is struggling.
func (c *DeliveryCounter) Flush(ctx context.Context, repo repository.ContentRepository, day time.Time) error {
	if c == nil {
		return nil
	}
	pending := c.drain()
	if len(pending) == 0 {
		return nil
	}
	var firstErr error
	for tenant, n := range pending {
		if err := repo.AddDeliveryReads(ctx, tenant, day, n); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			c.mu.Lock()
			c.counts[tenant] += n
			c.mu.Unlock()
		}
	}
	return firstErr
}

// RunFlusher flushes on an interval until ctx is done, then flushes once more so
// a graceful shutdown does not discard the final window.
func (c *DeliveryCounter) RunFlusher(ctx context.Context, repo repository.ContentRepository, interval time.Duration, onErr func(error)) {
	if c == nil || interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Best effort on the way out: ctx is already cancelled, so use a
			// fresh short-lived one for the final write.
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := c.Flush(final, repo, time.Now().UTC()); err != nil && onErr != nil {
				onErr(err)
			}
			cancel()
			return
		case <-t.C:
			if err := c.Flush(ctx, repo, time.Now().UTC()); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}
