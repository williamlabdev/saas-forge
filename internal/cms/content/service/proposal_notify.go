package service

import (
	"context"
	"fmt"
	"log"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	notifdomain "github.com/williamlabdev/saas-forge/internal/notification/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
)

// Proposal notifications (ADR-013 §3 step 8, william ruled 2026-08-07;
// widened to owner/admin fan-out and to pending-review 2026-09-09, ADR-023).
//
// WHY THIS EXISTS AT ALL. The queue is pull-only: a proposal is visible to
// whoever opens /admin/:appId/schema/proposals and to nobody else, while
// proposalTTL runs for seven days from the moment it is filed. Without a push
// of some kind, EXPIRY IS THE DEFAULT RATHER THAN THE EXCEPTION — the agent
// asked, nobody was told, and the row aged out answering "pending" the whole
// time.
//
// WHY NOT ADR-011 WEBHOOKS, which the ADR listed as the other candidate.
// 000029_content_webhooks.up.sql:10-13 already wrote the condition that rules
// them out: there is no per-event subscription column because the five content
// events have ONE audience (rebuild/purge), and `events text[]` is "additive
// when a real second audience appears". A proposal notice IS that second
// audience, so routing it through webhooks means adding the filter column, the
// subscription API and a default for existing rows first — otherwise every
// receiver wired up to rebuild a cache starts getting proposal payloads. And a
// webhook notifies a SYSTEM; the thing that has to happen here is a person
// pressing a button.

// Notifier delivers the nudge. It is deliberately narrower than the
// notification plane's own service: this caller has no subject to authorize
// (see SystemNotifier), and it never reads.
//
// NAMED Notifier, NOT ProposalNotifier, since ADR-023: content_service.go's
// pending-review hook (notifyEntryPendingReview) reuses the exact same field
// and method — both are "tell owner/admin something in this tenant needs
// their attention", so one interface serves both rather than the service
// holding two near-identical dependencies. tenantID is a plain string, the
// same shape authn.Subject.TenantID and repository.SchemaProposal.TenantID
// already carry through this package — see SystemNotifier.NotifyTenantRoles'
// doc for why the parse happens at the implementation, not at every call
// site.
type Notifier interface {
	NotifyTenantRoles(ctx context.Context, tenantID string, roles []string, exclude uuid.UUID, kind, title, body, link string) error
	// NotifyUser delivers to exactly one recipient, added for
	// notifyEntryChangesRequested (ADR-014 Amendment: review decisions) — a
	// review decision is aimed at the entry's last writer specifically, not at
	// a role, so the fan-out shape above does not fit: there is no set of
	// tenant members among whom "somebody should look at this" would be true.
	NotifyUser(ctx context.Context, userID uuid.UUID, tenantID, kind, title, body, link string) error
}

// notifyRoles is the fan-out this file and content_service.go's pending-review
// hook both notify: owner and admin. Neither editors nor viewers decide a
// schema proposal or a publish, so neither is in the audience for "something
// is waiting on you to decide".
var notifyRoles = []string{"owner", "admin"}

// WithNotifier enables the nudge. Nil means the deployment has no
// notification plane wired, and both the proposal queue and pending-review
// stay silent — which is exactly what they were before this file existed.
//
// Nil is NOT a refusal here, unlike the agent-credential revocation checker
// where "cannot ask" has to mean "deny". A missing notifier degrades a
// convenience; it cannot let anything through.
func WithNotifier(svc ContentService, n Notifier) ContentService {
	if n == nil {
		return svc
	}
	if cs, ok := svc.(*contentService); ok {
		cs.notify = n
		return cs
	}
	return svc
}

// notifyProposalFiled tells the tenant's owners/admins that a proposal is
// waiting, EXCEPT the person who filed it.
//
// FIXED 2026-09-09 (ADR-023), CLOSING THE GAP THIS COMMENT USED TO DOCUMENT.
// The old rule fired only for agent proposals (!sub.IsAgent()), reasoning that
// content:schema:propose was owner/admin-only, so a human proposer was by
// construction an approver and notifying them would be telling them what they
// just did. 補裁 T (2026-08-30) opened propose to editor, and from that day an
// editor's proposal notified NOBODY — the gap this file's previous revision
// tracked as a known, deliberately-unfixed defect (see the git history of this
// file, and TestEditorProposalNotifiesNobodyYet's rewrite in
// proposal_notify_test.go, which is the test that used to pin the gap and now
// pins the fix).
//
// THE FIX enumerates the tenant's owner/admin members via
// Notifier.NotifyTenantRoles and excludes the proposer — an agent proposal
// already excluded the proposer implicitly (its principal is, by 補裁 O, an
// owner/admin who therefore now ALSO receives its own proposal's notice
// unless excluded explicitly), and a human owner/admin proposer no longer
// needs the special-case "notify nobody" branch the old code hard-coded: they
// are simply the one member NotifyTenantRoles excludes, and every OTHER
// owner/admin still hears about it. That is a behaviour change from the old
// rule (co-owners of a tenant now learn about each other's proposals) and is
// the correct reading of "somebody other than the proposer should be able to
// decide this" once more than one human can hold the deciding role.
func (s *contentService) notifyProposalFiled(ctx context.Context, sub authn.Subject, rec *repository.SchemaProposal, plan PlanResult) {
	if s.notify == nil {
		return
	}
	title := "Schema proposal awaiting review"
	// COUNTS, NOT CONTENT. The body could say which types change — the recipient
	// is an approver who will see exactly that on the queue page — but
	// notifications are a table with no field-level masking, written by one role
	// and read by another, which is the same trap step 3 hit when an activity
	// title turned out to be a payload value. A count cannot carry a name, so
	// the question "may this recipient see this string" does not arise.
	body := fmt.Sprintf(
		"A schema proposal with %d change(s) is awaiting review. Decide before %s; after that it can no longer be approved.",
		plan.Applicable+plan.Refused+plan.Blocked,
		rec.ExpiresAt.Format("2006-01-02 15:04 UTC"),
	)
	link := fmt.Sprintf("/schema/proposals/%s", rec.ID)
	// context.WithoutCancel and best-effort, for the same reason the activity
	// recorder uses it: the proposal is already committed. Failing the request
	// now would tell the agent its proposal did not land when it did, and
	// putting this write in the proposal's transaction would let a notification
	// outage roll back proposals — strictly worse than a queue nobody was
	// pushed about, which is where we started.
	if err := s.notify.NotifyTenantRoles(context.WithoutCancel(ctx), rec.TenantID, notifyRoles, rec.ProposedBy, notifdomain.KindSchemaProposal, title, body, link); err != nil {
		log.Printf("content schema proposal: notify tenant %s: %v", rec.TenantID, err)
	}
}

// notifyEntryPendingReview tells the tenant's owners/admins that a published
// entry now has an unpublished draft waiting for them, EXCEPT the editor who
// just made the save. Called from UpdateEntry (content_service.go) only on the
// false->true transition of (published && HasUnpublishedChanges) — see that
// call site's comment for why the transition, not the state, is what fires
// this.
//
// DEBOUNCED BY CONSTRUCTION, NOT BY A TIMER (ADR-023). A tenant that wants
// "don't notify me again until this entry publishes" gets it for free from the
// transition gate: every subsequent PATCH while the entry is still pending
// leaves wasPending == nowPending == true and this method is never called
// again for that entry until a publish resets HasUnpublishedChanges to false
// and a later edit re-opens the transition. No debounce table, no TTL, no
// background sweep — the entry's own state is the debounce.
func (s *contentService) notifyEntryPendingReview(ctx context.Context, sub authn.Subject, e *domain.Entry) {
	if s.notify == nil {
		return
	}
	title := "Entry has unpublished changes"
	// COUNTS, NOT CONTENT — same rule as notifyProposalFiled, and for a reason
	// that bites harder here: ReadRoles is a PER-TYPE restriction (D3/ADR-009),
	// so "owner/admin" is not a guarantee this recipient may read THIS type —
	// canReadType can name a ReadRoles set that excludes owner/admin entirely.
	// notifyProposalFiled's body has the same rule for the softer reason that no
	// caller could name the affected types cheaply; this one has the harder
	// reason that naming ct.Label could be an authorization leak, not just an
	// avoidable one. The type name is not in the link either — see below.
	body := "An entry was saved and now has changes waiting to be published."
	// The link deliberately does NOT embed the type name (contrast
	// notifyProposalFiled's /schema/proposals/{id}, which is safe because a
	// schema proposal has no per-type ReadRoles gate of its own). Routing by
	// entry id alone lets the console resolve the type server-side, behind the
	// same guardTypeRead check GetEntry already applies, so a recipient without
	// read access to this type sees a 403 on the link rather than the type's
	// name in the id-agnostic column an id alone cannot carry it in.
	link := fmt.Sprintf("/content/entries/%s", e.ID)
	if err := s.notify.NotifyTenantRoles(context.WithoutCancel(ctx), sub.TenantID, notifyRoles, sub.UserID, notifdomain.KindEntryPendingReview, title, body, link); err != nil {
		log.Printf("content entry pending review: notify tenant %s: %v", sub.TenantID, err)
	}
}

// notifyEntryChangesRequested tells the entry's last writer that a reviewer
// sent it back (ADR-014 Amendment: review decisions), the ONE recipient
// NotifyUser exists for rather than notifyEntryPendingReview's owner/admin
// fan-out: the person who needs to act is whoever wrote what the reviewer just
// looked at, not everyone who could have.
//
// e.UpdatedBy IS THE RECIPIENT — the same last-writer resolution
// PendingEntryDTO.ActorUserID reuses, deliberately: it is the only record this
// service already keeps of "who would read this as addressed to them",
// spelled once and consumed by both. A nil UpdatedBy (an entry the migration
// backfilled, or one whose last write was unattributed) skips the notice
// rather than guessing a recipient; NO recipient is the honest answer there,
// not the reviewer, and not nobody-in-particular in a fan-out that does not
// fit this hook to begin with.
//
// SELF-REVIEW SKIPS. A reviewer sending back their own last edit already knows
// what they just did — the notification would tell them nothing except that
// the platform is capable of it. Compared with sub.ResponsibleUserID(), the
// same actor an agent's write is filed under, so a human reviewing an agent's
// change and a human reviewing their own change are judged by the same rule.
func (s *contentService) notifyEntryChangesRequested(ctx context.Context, sub authn.Subject, e *domain.Entry, reason string) {
	if s.notify == nil || e.UpdatedBy == nil {
		return
	}
	if *e.UpdatedBy == sub.ResponsibleUserID() {
		return
	}
	title := "Changes requested on your entry"
	// The reason ITSELF, unlike notifyEntryPendingReview's counts-not-content
	// rule — the spec calls for it explicitly, and the asymmetry is not an
	// oversight: pending-review's body is read by anyone with owner/admin
	// across every type in the tenant (a role-scoped fan-out with no per-type
	// filter, see that method's comment), while THIS notice goes to the one
	// person who already holds ReadRoles on this exact entry by virtue of
	// having just written it. There is no wider audience for the reason to leak
	// to.
	body := fmt.Sprintf("A reviewer requested changes: %s", reason)
	link := fmt.Sprintf("/content/entries/%s", e.ID)
	if err := s.notify.NotifyUser(context.WithoutCancel(ctx), *e.UpdatedBy, sub.TenantID, notifdomain.KindEntryChangesRequested, title, body, link); err != nil {
		log.Printf("content entry changes requested: notify user %s: %v", *e.UpdatedBy, err)
	}
}
