package outbox

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/pkg/mcp"
	"github.com/williamlabdev/saas-forge/internal/pkg/metrics"
)

// markTimeout bounds the detached mark operations that run even during shutdown.
const markTimeout = 5 * time.Second

// defaultStaleThreshold is used when a non-positive threshold is passed.
const defaultStaleThreshold = 60 * time.Second

// Worker polls the outbox and pushes events to their downstreams: user.* to
// MCP, content.* to tenant-registered webhooks.
type Worker struct {
	repo           Repository
	client         mcp.Client
	limit          int
	maxRetries     int
	staleThreshold time.Duration
	registry       *metrics.Registry
	// webhooks + sender route content.* events. Optional: a deployment without
	// the CMS wired leaves them nil, and a content event then fails LOUD through
	// the default arm rather than vanishing — same rule as an unknown type.
	webhooks WebhookDirectory
	sender   WebhookSender
}

// WithContentWebhooks routes content.* events to the tenant's registered
// endpoints. An option rather than two more constructor parameters, because
// every existing caller wires user events only and should not have to say
// "no webhooks" to keep doing so.
func (w *Worker) WithContentWebhooks(dir WebhookDirectory, sender WebhookSender) *Worker {
	w.webhooks = dir
	w.sender = sender
	return w
}

func NewWorker(repo Repository, client mcp.Client, batchSize, maxRetries int, staleThreshold time.Duration, registry *metrics.Registry) *Worker {
	if batchSize <= 0 {
		batchSize = 10
	}
	if maxRetries <= 0 {
		maxRetries = 5
	}
	if staleThreshold <= 0 {
		staleThreshold = defaultStaleThreshold
	}
	if registry == nil {
		registry = metrics.NewRegistry()
	}
	return &Worker{
		repo:           repo,
		client:         client,
		limit:          batchSize,
		maxRetries:     maxRetries,
		staleThreshold: staleThreshold,
		registry:       registry,
	}
}

func (w *Worker) Registry() *metrics.Registry {
	return w.registry
}

// Run blocks until ctx is cancelled, polling at interval.
func (w *Worker) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := w.repo.ReclaimStale(ctx, w.staleThreshold); err != nil {
				log.Printf("outbox worker: reclaim stale: %v", err)
			} else if n > 0 {
				log.Printf("outbox worker: reclaimed %d stale processing row(s) to pending", n)
			}
			if err := w.processBatch(ctx); err != nil {
				log.Printf("outbox worker: %v", err)
			}
			st, err := w.repo.PendingStats(ctx)
			if err != nil {
				log.Printf("outbox worker: pending stats: %v", err)
			} else {
				w.registry.SetOutboxGauges(st.Pending, st.LagSeconds, st.Processing, st.ProcessingLagSeconds)
			}
			log.Printf("outbox metrics: pending=%d lag_sec=%.1f processing=%d proc_lag_sec=%.1f delivered=%d retried=%d failed=%d dead=%d",
				st.Pending, st.LagSeconds, st.Processing, st.ProcessingLagSeconds,
				w.registry.OutboxDelivered.Load(),
				w.registry.OutboxRetried.Load(),
				w.registry.OutboxFailed.Load(),
				w.registry.OutboxDead.Load(),
			)
		}
	}
}

func (w *Worker) processBatch(ctx context.Context) error {
	rows, err := w.repo.ClaimPending(ctx, w.limit)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := w.deliver(ctx, row); err != nil {
			markCtx, cancel := w.markContext(ctx)
			dead, markErr := w.repo.MarkFailedWithRetry(markCtx, row.ID, err.Error(), w.maxRetries)
			cancel()
			if markErr != nil {
				log.Printf("outbox: mark retry %s: %v", row.ID, markErr)
			}
			if dead {
				w.registry.OutboxDead.Add(1)
			} else {
				w.registry.OutboxRetried.Add(1)
			}
			w.registry.OutboxFailed.Add(1)
			continue
		}
		markCtx, cancel := w.markContext(ctx)
		markErr := w.repo.MarkDone(markCtx, row.ID)
		cancel()
		if markErr != nil {
			log.Printf("outbox: mark done %s: %v", row.ID, markErr)
			continue
		}
		// OutboxDelivered counts only rows actually delivered to a downstream and
		// then marked done — never a no-op. See deliver's default branch (TKT-OBX-2).
		w.registry.OutboxDelivered.Add(1)
	}
	return nil
}

// markContext returns a context for MarkDone/MarkFailedWithRetry that is detached
// from ctx's cancellation, so a row that was already claimed still gets marked
// even when the worker ctx is cancelled during shutdown — otherwise it would be
// left stuck in 'processing' (TKT-OBX-1).
func (w *Worker) markContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), markTimeout)
}

func (w *Worker) deliver(ctx context.Context, row Row) error {
	switch row.EventType {
	case EventUserCreated, EventUserUpdated, EventUserDeleted:
		p, err := ParseUserPayload(row.Payload)
		if err != nil {
			return err
		}
		uid, err := uuid.Parse(p.UserID)
		if err != nil {
			return err
		}
		return w.client.UpsertUserState(ctx, mcp.UpsertRequest{
			UserID:         uid,
			Status:         p.Status,
			StatusVersion:  p.StatusVersion,
			IdempotencyKey: row.IdempotencyKey,
			EventType:      row.EventType,
		})
	default:
		if IsContentEvent(row.EventType) {
			return w.deliverContent(ctx, row)
		}
		// TKT-OBX-2: an event type with no delivery handler must NOT be silently
		// marked done and counted as delivered — that turns unrouted events into a
		// black hole. Fail loud so the row goes through the retry/dead-letter path
		// and the gap surfaces in metrics and logs.
		return fmt.Errorf("outbox: no delivery handler for event type %q", row.EventType)
	}
}

// deliverContent fans one content event out to every endpoint the tenant has
// registered. Fan-out at DELIVERY time, not enqueue time: the outbox row is
// the event, and one row per (event, endpoint) would make the registry's state
// at write time part of the event's identity.
//
// The consequence is the retry unit: any endpoint failing retries the WHOLE
// row, so an endpoint that already succeeded will hear the event again. That
// is the at-least-once contract webhooks carry anyway (a timeout after the
// receiver processed is indistinguishable from one before), which is why the
// delivery id header exists — receivers deduplicate on it.
//
// Zero registered endpoints is SUCCESS, not TKT-OBX-2's black hole: the event
// was routed to a handler and delivered to everyone subscribed, which is
// no one. Refusing it instead would dead-letter every content write made
// before the first webhook is registered.
func (w *Worker) deliverContent(ctx context.Context, row Row) error {
	if w.webhooks == nil || w.sender == nil {
		return fmt.Errorf("outbox: content event %q but no webhook delivery is wired", row.EventType)
	}
	p, err := ParseContentPayload(row.Payload)
	if err != nil {
		return err
	}
	endpoints, err := w.webhooks.ActiveWebhookEndpoints(ctx, p.TenantID)
	if err != nil {
		return fmt.Errorf("outbox: list webhooks for %s: %w", p.TenantID, err)
	}
	for _, ep := range endpoints {
		if err := w.sender.Send(ctx, ep, row.EventType, row.ID.String(), row.Payload); err != nil {
			return err
		}
	}
	return nil
}
