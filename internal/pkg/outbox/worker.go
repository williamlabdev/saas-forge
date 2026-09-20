package outbox

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	notifdomain "github.com/williamlabdev/saas-forge/internal/notification/domain"
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
	// notify pushes an in-app notice when a content event's row is dead-lettered
	// (ADR-023). Optional, same "degrade, never block" rule as webhooks/sender:
	// a deployment without the notification plane wired keeps delivering and
	// dead-lettering exactly as before, just silently.
	notify Notifier
}

// Notifier delivers the dead-letter nudge. Same shape as content/service's
// Notifier and SystemNotifier.NotifyTenantRoles itself — see that type's doc
// for why tenantID is a plain string and why the interface is this narrow.
type Notifier interface {
	NotifyTenantRoles(ctx context.Context, tenantID string, roles []string, exclude uuid.UUID, kind, title, body, link string) error
}

// deadLetterNotifyRoles mirrors content/service.notifyRoles: a dead-lettered
// webhook is a tenant-configuration problem (a bad URL, a receiver that has
// been down since before maxRetries ran out), and owner/admin are the roles
// that can act on it — reconfigure or remove the endpoint. Neither editors
// nor viewers manage webhook endpoints.
var deadLetterNotifyRoles = []string{"owner", "admin"}

// WithContentWebhooks routes content.* events to the tenant's registered
// endpoints. An option rather than two more constructor parameters, because
// every existing caller wires user events only and should not have to say
// "no webhooks" to keep doing so.
func (w *Worker) WithContentWebhooks(dir WebhookDirectory, sender WebhookSender) *Worker {
	w.webhooks = dir
	w.sender = sender
	return w
}

// WithNotifier enables the dead-letter nudge. Nil (the zero value) means the
// deployment has no notification plane wired, and dead-lettering stays silent
// — exactly the behaviour before this option existed.
func (w *Worker) WithNotifier(n Notifier) *Worker {
	w.notify = n
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
				// Notify only once the dead state is actually committed (markErr ==
				// nil) — "after commit, never before", the same rule execute() in
				// the scheduler follows. A retrying (not-yet-dead) failure must NOT
				// notify: that is normal at-least-once delivery still in flight, not
				// yet something owner/admin need to act on.
				if markErr == nil {
					w.notifyDeadLetter(ctx, row)
				}
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

// notifyDeadLetter tells the tenant's owners/admins that a content event's
// webhook delivery exhausted its retries (ADR-023). Gated to content.*
// events only: EventUserCreated/Updated/Deleted go to MCP, an internal
// system with its own operator-facing monitoring — a tenant's owner/admin
// have no endpoint to fix there and telling them about it would be noise
// they cannot act on. A content.* dead-letter, by contrast, means THEIR
// registered webhook endpoint is broken, which only they can fix.
//
// Best-effort and detached (context.WithoutCancel), same as every other
// notify call in this codebase: a notification-plane outage must not turn a
// delivery failure that was already recorded into a request failure, and
// there is no request here to fail anyway — this runs off the poll loop.
func (w *Worker) notifyDeadLetter(ctx context.Context, row Row) {
	if w.notify == nil || !IsContentEvent(row.EventType) {
		return
	}
	p, err := ParseContentPayload(row.Payload)
	if err != nil {
		// Malformed payload: deliver() would already have failed on the same
		// parse and this row is dead for that reason. Nothing to notify with —
		// no tenant to address it to — so log and move on, same as any other
		// best-effort notify failure.
		log.Printf("outbox: dead-letter notify %s: %v", row.ID, err)
		return
	}
	title := "A webhook endpoint stopped receiving events"
	// COUNTS, NOT CONTENT (same rule as content/service's notifyProposalFiled
	// and notifyEntryPendingReview): no endpoint URL, no event payload, just
	// the fact that delivery is failing. The event TYPE is safe to name —
	// unlike a content type's label, it carries no per-type ReadRoles gate.
	body := fmt.Sprintf(
		"Delivery of %s events to a registered webhook endpoint has failed repeatedly and will not be retried further. Check the endpoint's configuration.",
		row.EventType,
	)
	link := "/settings/webhooks"
	// exclude = uuid.Nil: there is no human actor here to exclude, unlike a
	// proposal filer or an entry's editor. Every owner/admin in the tenant
	// hears about it.
	if err := w.notify.NotifyTenantRoles(context.WithoutCancel(ctx), p.TenantID, deadLetterNotifyRoles, uuid.Nil, notifdomain.KindWebhookDead, title, body, link); err != nil {
		log.Printf("outbox: dead-letter notify tenant %s: %v", p.TenantID, err)
	}
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
