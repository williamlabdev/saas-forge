package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// EntryReviewDecision is one reviewer's verdict on a specific version of an
// entry (ADR-014 Amendment: review decisions).
//
// IT RECORDS A REFUSAL, NOT AN APPROVAL. ADR-014 §1 already has the approve
// half: pressing publish IS the approval, and needs no row of its own because
// Entry.PublishedVersion already says "someone approved this version". This
// type exists for the other outcome — a reviewer looked at EntryVersion and
// sent it back — which has no state to piggyback on.
//
// EntryVersion, NOT A "resolved" FLAG, is what tells a stale decision from a
// live one. An entry whose CURRENT Version still equals a decision's
// EntryVersion is sent back and not yet re-edited; the moment the author
// saves again, Entry.Version moves on and the decision becomes historical
// without this row changing. See the queue predicate in
// postgres_repository.go's ListPendingReview and dto.go's withReviewDecision,
// both of which do this comparison rather than reading a status column.
type EntryReviewDecision struct {
	ID       uuid.UUID
	TenantID string

	EntryID uuid.UUID

	// Decision is ReviewDecisionChangesRequested today. Kept as a string, not
	// hard-coded into the type name, so a second decision value (should one
	// ever earn its own row instead of reusing publish) has somewhere to go.
	Decision string

	// Reason is mandatory and non-blank (migration 000051's CHECK) — a
	// reviewer sending work back without saying why is the failure this
	// feature exists to prevent.
	Reason string

	// EntryVersion is THE VERSION THE REVIEWER WAS LOOKING AT, not a pointer
	// to a revision row. See the type doc.
	EntryVersion int

	DecidedBy uuid.UUID
	DecidedAt time.Time
}

// Review decision values. Kept in lockstep with
// entry_review_decisions_decision_check.
const (
	ReviewDecisionChangesRequested = "changes_requested"
)

// MaxReviewDecisionsPerEntry bounds the history read (GET
// .../review-decisions). Unlike publish revisions this is not a retention
// policy enforced by a trim — decisions are cheap, small rows with no payload
// to prune for size — it is only a page size, chosen the same as the queue's
// own default limit: enough for a reviewer to see an entry's whole back-and-
// forth without an unbounded query.
const MaxReviewDecisionsPerEntry = 50

// AllReviewDecisions is the legal decision set.
func AllReviewDecisions() []string {
	return []string{ReviewDecisionChangesRequested}
}

// ValidReviewDecision reports whether d is a legal decision value.
func ValidReviewDecision(d string) bool {
	for _, x := range AllReviewDecisions() {
		if x == d {
			return true
		}
	}
	return false
}

// ReviewDecisionDetails is the Activity.Details payload of an
// entry.review.changes_requested line: the reason, carried as a structured
// fact rather than folded into the title (RestoreDetails in
// publish_revision.go is the direct template — one fact this action has and
// changed_keys/title have no field for).
func ReviewDecisionDetails(reason string) json.RawMessage {
	b, err := json.Marshal(map[string]any{
		"reason": reason,
	})
	if err != nil {
		// Unreachable: the map holds one string. Returning nil rather than
		// panicking keeps a marshalling bug from taking down a review.
		return nil
	}
	return b
}
