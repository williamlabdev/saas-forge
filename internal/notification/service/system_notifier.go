package service

import (
	"context"
	"log"
	"time"

	"github.com/google/uuid"
	tenantdomain "github.com/williamlabdev/saas-forge/internal/tenant/domain"

	"github.com/williamlabdev/saas-forge/internal/notification/domain"
	"github.com/williamlabdev/saas-forge/internal/notification/repository"
)

// TenantRoster is the narrow slice of the tenant module SystemNotifier needs
// — "who is in this tenant, and what is their role" — defined here on the
// consumer side, the same shape this codebase already uses for
// ProposalNotifier and outbox.WebhookDirectory, so the notification module
// depends on one method rather than the whole tenant service. William's task
// brief confirmed there is no import cycle in depending on
// internal/tenant/domain directly (unlike MemberEmailLookup elsewhere, this
// has no reason to mirror the struct locally: Member is already the
// consumer-facing read shape, not a tenant-internal one).
type TenantRoster interface {
	Members(ctx context.Context, tenantID uuid.UUID) ([]tenantdomain.Member, error)
}

// EmailSender delivers the email half of a notification. SystemNotifier
// treats it as best-effort and optional — see NoopEmailSender, the only
// implementation this repository ships (no real provider is integrated by
// 2.5a; ADR-013 already noted this product has no email channel at all, and
// this interface exists so a future provider has somewhere to plug in
// without touching SystemNotifier's callers).
type EmailSender interface {
	Send(ctx context.Context, userID uuid.UUID, title, body string) error
}

// NoopEmailSender is the EmailSender every composition root wires today. It
// logs exactly once, at construction (i.e. at process startup, since both
// composition roots build it during wiring rather than lazily), so an
// operator reading startup logs sees plainly that notifications are in-app
// only — not once per notification, which would be startup noise repeated
// forever.
type NoopEmailSender struct{}

func NewNoopEmailSender() *NoopEmailSender {
	log.Printf("notification: email not configured; notifications are in-app only")
	return &NoopEmailSender{}
}

func (NoopEmailSender) Send(context.Context, uuid.UUID, string, string) error {
	return nil
}

// SystemNotifier writes a notification that no caller asked for.
//
// WHY IT DOES NOT AUTHORIZE, when NotificationService.Create right next to it
// does. Create serves an HTTP caller, and the subject it authorizes is the
// recipient — it can only ever write to the mailbox of the person making the
// request. There is no "send to someone else" in that shape and this is not
// that shape: the sender here is the platform reacting to an event, so there is
// no subject in context to authorize and no principal whose permissions could
// be the answer. The activity recorder writes straight to its repository for
// the same reason.
//
// WHAT KEEPS THIS FROM BECOMING A BACKDOOR. The recipient is chosen by the
// caller, so every call site has to justify its own choice; that justification
// belongs at the call site, not here (see notifyProposalFiled and
// notifyEntryPendingReview in the content service). What this type
// deliberately does NOT offer is a read — a system notifier that could list
// somebody's mailbox would be a permission bypass rather than a write nobody
// authorized.
type SystemNotifier struct {
	repo    repository.NotificationRepository
	tenants TenantRoster
	email   EmailSender
}

// NewSystemNotifier wires the notification repository, the tenant roster
// NotifyTenantRoles needs, and an email sender. email is REQUIRED to be
// non-nil at each composition root (both wire NewNoopEmailSender() today);
// this constructor does not default it, because a silently-defaulted email
// channel is exactly the kind of drift-between-composition-roots this task's
// own review of cmd/server/providers.go caught once already.
func NewSystemNotifier(repo repository.NotificationRepository, tenants TenantRoster, email EmailSender) *SystemNotifier {
	return &SystemNotifier{repo: repo, tenants: tenants, email: email}
}

// NotifyUser drops one notification into userID's mailbox, then makes a
// best-effort attempt to email them.
//
// tenantID and link are both plain strings, empty meaning "none" — not
// *uuid.UUID / *string — so the three narrow consumer interfaces this method
// satisfies (content service's Notifier, scheduler.Notifier, outbox.Notifier)
// can all be spelled in ordinary Go types without any of those packages
// importing uuid pointer plumbing or this package's domain types. An invalid
// or empty tenantID degrades to a tenant-less row rather than failing the
// call — see domain.Notification.TenantID's doc: nil/absent tenant already
// has defined meaning (matches every tenant view), so a bad tenant string is
// treated the same as no tenant, not as an error worth failing a committed
// event over.
//
// Title and body are platform-authored copy, not caller input, so they are
// not run through the validator that guards CreateInput — there is no
// untrusted string here to reject. Call sites that would interpolate user or
// tenant data into either are the ones that need to think about what they
// are copying in.
func (n *SystemNotifier) NotifyUser(ctx context.Context, userID uuid.UUID, tenantID, kind, title, body, link string) error {
	note := &domain.Notification{
		ID:        uuid.New(),
		UserID:    userID,
		Title:     title,
		Body:      body,
		Kind:      kind,
		CreatedAt: time.Now().UTC(),
	}
	if tid, err := uuid.Parse(tenantID); err == nil {
		note.TenantID = &tid
	} else if tenantID != "" {
		log.Printf("system notifier: ignoring malformed tenant id %q for user %s", tenantID, userID)
	}
	if link != "" {
		note.Link = &link
	}
	if err := n.repo.Create(ctx, note); err != nil {
		return err
	}
	if n.email != nil {
		if err := n.email.Send(ctx, userID, title, body); err != nil {
			log.Printf("system notifier: email %s: %v", userID, err)
		}
	}
	return nil
}

// NotifyTenantRoles notifies every member of tenantID whose role is in roles,
// except exclude (typically the actor whose own action produced the event).
// uuid.Nil excludes nobody — the shape a system-triggered event with no human
// actor uses (webhook dead-letter).
//
// A malformed tenantID or a failure to list members fails the WHOLE call,
// unlike NotifyUser's single-recipient degrade: there is no member list to
// fall back to, so "notify nobody" is the only honest outcome, and the caller
// (always a best-effort, logged call site — see proposal_notify.go and the
// scheduler/outbox workers) is what decides that a failure here does not
// propagate further.
//
// One notification write per matching member; a failure notifying one member
// is logged and does not stop the rest.
func (n *SystemNotifier) NotifyTenantRoles(ctx context.Context, tenantID string, roles []string, exclude uuid.UUID, kind, title, body, link string) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return err
	}
	members, err := n.tenants.Members(ctx, tid)
	if err != nil {
		return err
	}
	roleSet := make(map[string]bool, len(roles))
	for _, r := range roles {
		roleSet[r] = true
	}
	for _, m := range members {
		if m.UserID == exclude || !roleSet[m.Role] {
			continue
		}
		if err := n.NotifyUser(ctx, m.UserID, tenantID, kind, title, body, link); err != nil {
			log.Printf("system notifier: notify %s in tenant %s: %v", m.UserID, tenantID, err)
		}
	}
	return nil
}
