package service

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Review decisions (ADR-014 Amendment: review decisions).
//
// THE ONE THING TO UNDERSTAND ABOUT THIS FILE: a decision is a VERDICT, not a
// state. There is no "sent back" status on entries — that would be a second
// workflow machine layered on top of the one ADR-014 §1 already defined
// (draft/published, HasUnpublishedChanges), and every entry would need to
// answer to both. Instead each decision records the working-copy VERSION it
// was made against, and reviewDecisionNotSentBackExpr (postgres_repository.go)
// treats an entry as "sent back" for exactly as long as entries.version has
// not moved past that number. An author's next save moves it, and the entry
// falls back into /pending-review on its own — no separate "resolve" action to
// forget, no second flag to keep in sync with the first.

// maxReviewReasonLen bounds the one piece of free text this endpoint accepts.
// 2000 matches no other limit in this package by design — it is not a title,
// not a payload field with its own type-declared limit, and not unbounded
// either: a reason a reviewer is expected to type by hand into a form field,
// long enough for real feedback and short enough that CONTENT_REVIEW_REASON_TOO_LONG
// catches a caller that pasted something else in by mistake.
const maxReviewReasonLen = 2000

// RequestChangesInput is the request-changes request body.
type RequestChangesInput struct {
	Reason string `json:"reason"`
}

// ReviewDecisionDTO is one reviewer verdict as the wire sees it — both as the
// response to POST .../review/request-changes and as an entry in
// GET .../review-decisions and (only while current) EntryDTO.ReviewDecision.
//
// No `omitempty` anywhere here, unlike EntryDTO.ReviewDecision itself: THIS
// type is only ever constructed to describe a decision that exists, never
// zero-valued and returned regardless, so every field is a fact a reader must
// render rather than one that might be legitimately absent.
type ReviewDecisionDTO struct {
	ID           uuid.UUID `json:"id"`
	Decision     string    `json:"decision"`
	Reason       string    `json:"reason"`
	EntryVersion int       `json:"entry_version"`
	DecidedBy    uuid.UUID `json:"decided_by"`
	DecidedAt    time.Time `json:"decided_at"`
}

func projectReviewDecision(d domain.EntryReviewDecision) ReviewDecisionDTO {
	return ReviewDecisionDTO{
		ID:           d.ID,
		Decision:     d.Decision,
		Reason:       d.Reason,
		EntryVersion: d.EntryVersion,
		DecidedBy:    d.DecidedBy,
		DecidedAt:    d.DecidedAt,
	}
}

// errReviewReasonRequired covers both an empty body and a reason that is
// whitespace-only — btrim(reason) <> ” is the same rule the migration's CHECK
// enforces at the row, so a caller that somehow got past this Go-side check
// (there is no such path today) would still be refused at the database rather
// than storing a decision nobody could read.
var errReviewReasonRequired = apperrors.New(
	"CONTENT_REVIEW_REASON_REQUIRED",
	"reason must not be empty",
	422,
)

func errReviewReasonTooLong(n int) error {
	return apperrors.New(
		"CONTENT_REVIEW_REASON_TOO_LONG",
		"reason must be at most 2000 characters",
		422,
	).WithDetails(map[string]any{"length": n, "max": maxReviewReasonLen})
}

// errEntryNotPending answers a request-changes call aimed at an entry that is
// not currently in the release queue — either because it was never pending
// (a clean published entry, or one with no unpublished changes) or because it
// already carries a decision sent back against its current version. Both read
// the same to the caller: this entry is not something a reviewer has anything
// to say about right now.
var errEntryNotPending = apperrors.New(
	"CONTENT_ENTRY_NOT_PENDING",
	"entry is not currently pending review",
	http.StatusConflict,
)

// entryIsPendingReview mirrors pendingReviewExpr (postgres_repository.go) in
// Go, for the one call site that already has the row in hand and must not pay
// a second query to ask the database the same question: status <> published,
// or published with a working copy that has moved on from what is live.
func entryIsPendingReview(e *domain.Entry) bool {
	return e.Status != domain.StatusPublished || e.HasUnpublishedChanges
}

// RequestChanges records a reviewer's verdict that an entry needs more work
// before it can be published, and notifies the entry's last writer why.
//
// AUTHORIZED WITH content:publish — see the ContentService interface comment
// for the reasoning; it is not repeated here.
//
// ORDER OF CHECKS mirrors ScheduleEntry and RestoreEntryRevision: authorize,
// find the type, guard write access to the type, find the entry, guard
// confinement, THEN the optimistic lock (so a stale caller never learns
// whether a row it may not touch exists at a different version), THEN the
// domain-specific refusal (not pending). Reason validation is the one
// exception, checked BEFORE the recorder opens — same reasoning
// ScheduleEntry's action-validity check gives: until content is known to be a
// well-formed request there is nothing yet worth authorizing or recording.
//
// ONE DATABASE WRITE (the decision insert); the activity row and the
// notification are both best-effort writes AFTER it commits, in that order,
// exactly as every other write in this package finishes — see
// notifyEntryPendingReview and this package's activity recorder for why a
// notification outage must not roll back a decision that already landed.
func (s *contentService) RequestChanges(ctx context.Context, typeName string, id uuid.UUID, in RequestChangesInput, expectedVersion int) (_ ReviewDecisionDTO, err error) {
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return ReviewDecisionDTO{}, errReviewReasonRequired
	}
	if len(reason) > maxReviewReasonLen {
		return ReviewDecisionDTO{}, errReviewReasonTooLong(len(reason))
	}
	act := s.activityWrite(ctx, domain.ActivityEntryReviewChangesRequested, typeName).forEntry(id)
	defer func() { act.finish(ctx, err) }()

	sub, err := s.authorize(ctx, ActionContentPublish, id.String(), typeName)
	if err != nil {
		return ReviewDecisionDTO{}, err
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return ReviewDecisionDTO{}, err
	}
	if err := guardTypeWrite(ct, sub); err != nil {
		return ReviewDecisionDTO{}, err
	}
	existing, err := s.repo.GetEntry(ctx, sub.TenantID, ct.ID, id)
	if err != nil {
		return ReviewDecisionDTO{}, err
	}
	if err := guardOwned(ct, sub, existing); err != nil {
		return ReviewDecisionDTO{}, err
	}
	act.describe(ct.Fields, existing.Payload)
	// The optimistic lock, in RestoreEntryRevision's exact position: after
	// confinement, so a 409 never confirms the existence and version of a row
	// this caller may not touch.
	if expectedVersion > 0 && existing.Version != expectedVersion {
		return ReviewDecisionDTO{}, repository.ErrVersionConflict
	}
	// "In the queue" here means the FULL queue predicate, sent-back exclusion
	// included — not merely entryIsPendingReview. An entry already sent back
	// against its current version is, from a reviewer's point of view, exactly
	// as invisible as one that was never pending: /pending-review does not list
	// it (reviewDecisionNotSentBackExpr), so a second request-changes call
	// against it would be a decision about a row no reviewer could have been
	// looking at through the queue. Re-reviewing after a real re-edit works
	// fine — the author's save already moved existing.Version past the earlier
	// decision's EntryVersion by the time this runs, so the two checks below
	// agree with the queue again.
	if !entryIsPendingReview(existing) {
		return ReviewDecisionDTO{}, errEntryNotPending
	}
	prior, err := s.repo.GetLatestEntryReviewDecision(ctx, sub.TenantID, existing.ID)
	if err != nil {
		return ReviewDecisionDTO{}, err
	}
	if prior != nil && prior.EntryVersion == existing.Version {
		return ReviewDecisionDTO{}, errEntryNotPending
	}
	dec := &domain.EntryReviewDecision{
		ID:           uuid.New(),
		TenantID:     sub.TenantID,
		EntryID:      existing.ID,
		Decision:     domain.ReviewDecisionChangesRequested,
		Reason:       reason,
		EntryVersion: existing.Version,
		DecidedBy:    sub.ResponsibleUserID(),
		DecidedAt:    s.clock(),
	}
	if err := s.repo.CreateEntryReviewDecision(ctx, dec); err != nil {
		return ReviewDecisionDTO{}, err
	}
	act.withDetails(domain.ReviewDecisionDetails(reason))
	s.notifyEntryChangesRequested(ctx, sub, existing, reason)
	return projectReviewDecision(*dec), nil
}

// ListEntryReviewDecisions returns the entry's decision history, newest first
// — resolveRevisionEntry's guard sequence with the read verb, since this is a
// read with no write of its own: authorize, find the type, guard read access,
// find the entry, guard confinement. No activity recorder, on
// EntryReferencedBy's precedent (a read of derived metadata about an entry,
// not of the entry itself).
func (s *contentService) ListEntryReviewDecisions(ctx context.Context, typeName string, id uuid.UUID) ([]ReviewDecisionDTO, error) {
	sub, err := s.authorize(ctx, ActionContentRead, id.String(), typeName)
	if err != nil {
		return nil, err
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return nil, err
	}
	if err := guardTypeRead(ct, sub); err != nil {
		return nil, err
	}
	e, err := s.repo.GetEntry(ctx, sub.TenantID, ct.ID, id)
	if err != nil {
		return nil, err
	}
	if err := guardOwned(ct, sub, e); err != nil {
		return nil, err
	}
	decisions, err := s.repo.ListEntryReviewDecisions(ctx, sub.TenantID, id)
	if err != nil {
		return nil, err
	}
	out := make([]ReviewDecisionDTO, len(decisions))
	for i, d := range decisions {
		out[i] = projectReviewDecision(d)
	}
	return out, nil
}

// decorateWithReviewDecision attaches the entry's latest review decision to a
// single-GET DTO — withReviewDecision's own version-match guard decides
// whether it actually shows, so a stale decision looked up here simply does
// not attach. Same soft-fail shape as decorateWithSchedule: a lookup failure
// logs and returns the DTO unchanged rather than failing the read, because a
// review verdict is exactly as non-essential to rendering an entry as a
// schedule badge is.
func (s *contentService) decorateWithReviewDecision(ctx context.Context, sub authn.Subject, dto EntryDTO, entryID uuid.UUID) EntryDTO {
	if sub.PublicDelivery {
		return dto
	}
	dec, err := s.repo.GetLatestEntryReviewDecision(ctx, sub.TenantID, entryID)
	if err != nil {
		log.Printf("content review decision: read latest decision for entry %s of tenant %s: %v", entryID, sub.TenantID, err)
		return dto
	}
	return dto.withReviewDecision(dec)
}

// decorateEntriesWithReviewDecision is decorateWithReviewDecision for a page
// of entries at once — decorateEntriesWithSchedule's exact shape, batched
// through ListLatestEntryReviewDecisions rather than one lookup per row.
func (s *contentService) decorateEntriesWithReviewDecision(ctx context.Context, sub authn.Subject, items []EntryDTO, entries []*domain.Entry) []EntryDTO {
	if sub.PublicDelivery || len(entries) == 0 {
		return items
	}
	ids := make([]uuid.UUID, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	latest, err := s.repo.ListLatestEntryReviewDecisions(ctx, sub.TenantID, ids)
	if err != nil {
		log.Printf("content review decision: list latest decisions for tenant %s: %v", sub.TenantID, err)
		return items
	}
	for i, e := range entries {
		items[i] = items[i].withReviewDecision(latest[e.ID])
	}
	return items
}
