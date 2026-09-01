package service

import (
	"context"
	"fmt"
	"log"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
)

// Proposal notifications (ADR-013 §3 step 8, william ruled 2026-08-07).
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

// ProposalNotifier delivers the nudge. It is deliberately narrower than the
// notification plane's own service: this caller has no subject to authorize
// (see SystemNotifier), and it never reads.
type ProposalNotifier interface {
	NotifyUser(ctx context.Context, userID uuid.UUID, title, body string) error
}

// WithProposalNotifier enables the nudge. Nil means the deployment has no
// notification plane wired, and the queue stays pull-only — which is exactly
// what it was before this file existed.
//
// Nil is NOT a refusal here, unlike the agent-credential revocation checker
// where "cannot ask" has to mean "deny". A missing notifier degrades a
// convenience; it cannot let anything through.
func WithProposalNotifier(svc ContentService, n ProposalNotifier) ContentService {
	if n == nil {
		return svc
	}
	if cs, ok := svc.(*contentService); ok {
		cs.notify = n
		return cs
	}
	return svc
}

// notifyProposalFiled tells the responsible human that an agent is waiting.
//
// ONLY FOR AGENT PROPOSALS. The original reasoning was that
// content:schema:propose is owner/admin, so a human who files a proposal is
// already someone who can approve it and notifying them is telling them what
// they just did. Agents are the case the notification exists for: the proposer
// cannot act on its own proposal, and the queue is closed to it (補裁 Q-2).
//
// 🔴 THAT REASONING EXPIRED ON 2026-08-30 AND THIS CODE HAS NOT CAUGHT UP.
// 補裁 T opened propose (and plan) to editor, which is exactly the day the ⚠️
// that used to stand here named in advance: a human proposer is no longer by
// construction an approver. An editor who files a proposal today notifies
// NOBODY, and the 7-day TTL (補裁 Q-3) runs anyway — so the failure mode is a
// proposal that expires unseen unless an owner/admin happens to open the queue.
//
// It is left unfixed DELIBERATELY rather than overlooked, and the reason is
// that the fix is not a line here. The recipient would have to become "the
// approvers", which means enumerating the tenant's owner/admin memberships —
// a repository this service does not hold, N recipients instead of one, and a
// product decision about whether every owner gets pinged for every proposal.
// That is a ruling, not a refactor; it is tracked in ADR-013's open items.
//
// The gap is a degradation and not a regression: before 補裁 T an editor could
// not file a proposal at all, so no proposal that used to be announced has
// stopped being announced. The queue page still shows it (補裁 Q-4).
//
// THE RECIPIENT IS THE PRINCIPAL, and that is not a new rule. 補裁 F3 already
// ruled that an agent's writes are attributed to the person who minted the
// credential ("who is responsible", not "who typed"), and 補裁 O narrowed
// minting to owner/admin — so the principal is, by construction, someone who
// holds the verb that decides this proposal. This is that same rule applied to
// one more surface.
//
// ⚠️ Known single point: if the minter is on leave or has been suspended
// without their account being deleted, the notice lands in a mailbox nobody
// reads while the TTL runs anyway. That is the downstream of the ADR's open
// item on suspended-but-not-deleted principals, and the second layer for it is
// a pending count in the console chrome, not a second recipient here.
func (s *contentService) notifyProposalFiled(ctx context.Context, sub authn.Subject, rec *repository.SchemaProposal, plan PlanResult) {
	if s.notify == nil || !sub.IsAgent() {
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
		"An agent proposed %d schema change(s). Review and decide before %s; after that it can no longer be approved.",
		plan.Applicable+plan.Refused+plan.Blocked,
		rec.ExpiresAt.Format("2006-01-02 15:04 UTC"),
	)
	// context.WithoutCancel and best-effort, for the same reason the activity
	// recorder uses it: the proposal is already committed. Failing the request
	// now would tell the agent its proposal did not land when it did, and
	// putting this write in the proposal's transaction would let a notification
	// outage roll back proposals — strictly worse than a queue nobody was
	// pushed about, which is where we started.
	if err := s.notify.NotifyUser(context.WithoutCancel(ctx), rec.ProposedBy, title, body); err != nil {
		log.Printf("content schema proposal: notify %s for tenant %s: %v", rec.ProposedBy, rec.TenantID, err)
	}
}
