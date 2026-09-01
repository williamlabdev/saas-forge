package service

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// PendingEntryDTO is one row of the release queue (ADR-014 §2).
//
// IT CARRIES NO PAYLOAD, and that is the design rather than an omission. This
// query spans every content type in the tenant, so it cannot apply the per-type
// read rule or the field-level mask that ProjectEntry applies to an entry the
// caller asked for BY TYPE — there is no single type to check against. Shipping
// values here would route restricted content around both fences, which is the
// leak §3's title fence already had to be built against and the one §6's diff
// mask closes on the other side. The queue answers "which things are waiting",
// and the diff — asked per entry, per type, with both fences on — answers
// "what changed".
//
// Title is the same denormalised label the activity stream uses, derived by
// domain.TitleFor from fields with no read restriction. "" is the honest answer
// when no field qualifies, and the console renders the id.
//
// No `omitempty` anywhere: these are the fields a reader must render, and a key
// that disappears when it holds the zero value is a key an API-shape test can
// silently stop covering. `HasPublishedSnapshot: false` is a fact the console
// acts on — it is what separates a draft that was never released from one that
// was taken down.
type PendingEntryDTO struct {
	ID          uuid.UUID `json:"id"`
	ContentType string    `json:"content_type"`
	Title       string    `json:"title"`

	// Status is the editorial state, and it is the ONLY liveness judgement.
	// Since ADR-014 §5.1 kept the snapshot through a retract, "has a snapshot"
	// and "is live" are different conditions; see src/lib/entry-diff.ts in the
	// saas-platform-console repo (ADR-016), which states the same correction on
	// the client side.
	Status string `json:"status"`
	// HasPublishedSnapshot separates `retracted` from `not-published`. Both are
	// non-published, and only the snapshot tells them apart — merging them would
	// tell a reviewer an entry has never been released when in fact somebody took
	// it down.
	HasPublishedSnapshot bool `json:"has_published_snapshot"`
	// HasUnpublishedChanges is the live half's reason for being in the queue.
	// False on every draft, which is exactly why the queue's criterion is not
	// this flag alone — see pendingReviewExpr.
	HasUnpublishedChanges bool `json:"has_unpublished_changes"`

	// The last writer, as the same trio the activity stream and the per-field
	// attribution carry, under the same names, so the console renders all three
	// through describeWriter rather than growing a fourth spelling of the rule.
	// ActorUserID is who ANSWERS — the principal, for an agent.
	ActorKind    *string    `json:"actor_kind"`
	ActorUserID  *uuid.UUID `json:"actor_user_id"`
	ActorAgentID *string    `json:"actor_agent_id"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// ListPendingReviewInput bounds one read of the queue.
type ListPendingReviewInput struct {
	Limit int
}

// ListPendingReview returns everything in the tenant waiting on a person
// (ADR-014 §2's top half), agent-touched first.
//
// AUTHORIZATION is ListActivity's shape, deliberately: content:review:list is a
// verb of its own as of the 2026-08-06 ruling that named this queue alongside
// the stream. It shipped on content:list and so reached viewer; it no longer
// does. owner/admin/editor is also exactly who holds content:publish, which is
// the queue's whole purpose — it routes work to the people who can clear it.
//
// AN AGENT IS REFUSED IT by the same two layers in the same order as the stream
// — §4's untyped rule first, the agent gate behind it; see ListActivity. The
// reason is the mirror of the stream's: this is the list of an agent's own
// output waiting on a person, and an agent that could read it could watch how
// long its work sits unreviewed.
//
// The delivery and preview audiences are refused at the chokepoint (isReadAction
// fails closed on an unlisted verb) and again below; ListActivity's comment
// carries the reason the second layer earns its keep.
//
// A SEPARATE VERB FROM THE STREAM, not a shared one, even though the roles
// coincide today — see the authz package for that argument.
//
// NOT RECORDED in the activity stream. §3's dominance rule records an agent's
// reads and a person's refused writes, not a person's successful read — and no
// agent can reach this path at all.
func (s *contentService) ListPendingReview(ctx context.Context, in ListPendingReviewInput) ([]PendingEntryDTO, error) {
	sub, err := s.authorize(ctx, ActionContentReviewList, "pending-review", "")
	if err != nil {
		return nil, err
	}
	// Second layer; ListActivity's comment carries the argument for why this is
	// not decoration.
	if sub.PublicDelivery {
		return nil, apperrors.ErrForbidden
	}
	// The DATA layer travels into the query, because this is the one entry read
	// with no single type for the service to guard against (ADR-009's second
	// layer; see pendingReviewVisibleExpr for why it cannot live here).
	entries, err := s.repo.ListPendingReview(ctx, repository.PendingReviewFilter{
		TenantID:     sub.TenantID,
		ViewerRole:   sub.TenantRole,
		ViewerUserID: sub.ResponsibleUserID(),
		Limit:        in.Limit,
	})
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return []PendingEntryDTO{}, nil
	}
	// One read of the type list, not one per row: ListContentTypes already loads
	// every type's fields in a single grouped query, and the alternative — asking
	// per entry — is the N+1 this cross-type query exists to avoid, reintroduced
	// one layer up.
	types, err := s.repo.ListContentTypes(ctx, sub.TenantID)
	if err != nil {
		return nil, err
	}
	byID := make(map[uuid.UUID]*domain.ContentType, len(types))
	for _, ct := range types {
		byID[ct.ID] = ct
	}
	out := make([]PendingEntryDTO, 0, len(entries))
	for _, e := range entries {
		row := PendingEntryDTO{
			ID:                    e.ID,
			Status:                e.Status,
			HasPublishedSnapshot:  len(e.PublishedPayload) > 0,
			HasUnpublishedChanges: e.HasUnpublishedChanges,
			ActorKind:             e.UpdatedByKind,
			ActorUserID:           e.UpdatedBy,
			ActorAgentID:          e.UpdatedByAgent,
			UpdatedAt:             e.UpdatedAt,
		}
		// A type the list did not return leaves the name and title empty rather
		// than dropping the row. The row is still a thing waiting on somebody,
		// and a queue that silently omits entries is the failure this whole
		// section exists to prevent; an unlabelled row is visible and diagnosable,
		// a missing one is neither.
		if ct, ok := byID[e.ContentTypeID]; ok {
			row.ContentType = ct.Name
			row.Title = domain.TitleFor(ct.Fields, e.Payload)
		}
		out = append(out, row)
	}
	return out, nil
}
