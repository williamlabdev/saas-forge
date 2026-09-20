package service

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Sweep defaults: one retention clock for uploaded orphans and overdue
// reservations alike (research D3 — 30 days dwarfs the 15-minute upload TTL,
// so a fresh reservation can never be mistaken for a stale one), and a
// bounded synchronous request (research D4).
const (
	defaultSweepOlderThan = 720 * time.Hour
	defaultSweepLimit     = 100
	maxSweepLimit         = 500
)

// Sweep actions and reasons. A failed item keeps action "skipped" with the
// error CODE as its reason (not the literal "failed") — the data-model shape
// carries the code so the operator learns which failure, without any internal
// message crossing the API.
const (
	sweepActionDeleted     = "deleted"
	sweepActionSkipped     = "skipped"
	sweepActionWouldDelete = "would_delete"

	sweepReasonReferenced = "referenced"
	sweepReasonFresh      = "fresh"
)

// SweepInput is the fully-validated input to SweepOrphans. Zero values take
// the defaults; OlderThan parsing (time.ParseDuration, "" means default) and
// the 400 on garbage live in the handler — this struct only carries durations.
type SweepInput struct {
	OlderThan time.Duration
	Limit     int
	DryRun    bool
}

// SweepItem is one candidate's fate.
type SweepItem struct {
	ID     uuid.UUID `json:"id"`
	Action string    `json:"action"`
	Reason string    `json:"reason,omitempty"`
}

// SweepSkipped classifies what was not deleted.
type SweepSkipped struct {
	Referenced int `json:"referenced"`
	Fresh      int `json:"fresh"`
	Failed     int `json:"failed"`
}

// SweepResult is the operator's whole answer: what went, what stayed and why,
// and whether another call is needed.
type SweepResult struct {
	Deleted   int          `json:"deleted"`
	Skipped   SweepSkipped `json:"skipped"`
	Truncated bool         `json:"truncated"`
	Remaining int          `json:"remaining"`
	Items     []SweepItem  `json:"items"`
}

// SweepOrphans deletes one bounded batch of orphaned media: assets neither
// link table names (draft nor published snapshot) older than OlderThan,
// overdue reservations included — they satisfy the NOT EXISTS definition
// trivially. Each candidate goes through DeleteMediaAsset(force=false), so
// the reference recheck, the 409 preview filter and the metadata-first /
// bytes-best-effort semantics are the single-delete path's verbatim, and a
// link landing mid-sweep counts as skipped/referenced instead of failing the
// batch (TOCTOU is closed by rechecking, not by locking).
//
// Enumeration and deletion are two phases: phase 1 pages ListMediaAssets
// WITHOUT writing (offsets stay stable), phase 2 processes at most Limit
// candidates. Remaining is measured against the enumeration snapshot, so rows
// this call deleted do not inflate it.
func (s *contentService) SweepOrphans(ctx context.Context, in SweepInput) (SweepResult, error) {
	sub, err := s.authorize(ctx, ActionContentDelete, "media", "")
	if err != nil {
		return SweepResult{}, err
	}
	olderThan := in.OlderThan
	if olderThan <= 0 {
		olderThan = defaultSweepOlderThan
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultSweepLimit
	}
	if limit > maxSweepLimit {
		limit = maxSweepLimit
	}
	cutoff := time.Now().UTC().Add(-olderThan)

	candidates, total, err := s.sweepCandidates(ctx, sub.TenantID, limit)
	if err != nil {
		return SweepResult{}, err
	}
	res := SweepResult{Items: make([]SweepItem, 0, len(candidates))}
	for _, a := range candidates {
		if a.CreatedAt.After(cutoff) {
			res.Skipped.Fresh++
			res.Items = append(res.Items, SweepItem{ID: a.ID, Action: sweepActionSkipped, Reason: sweepReasonFresh})
			continue
		}
		if in.DryRun {
			if ref, err := s.sweepReferenced(ctx, sub.TenantID, a.ID); err != nil {
				res.Skipped.Failed++
				res.Items = append(res.Items, SweepItem{ID: a.ID, Action: sweepActionSkipped, Reason: sweepErrorCode(err)})
			} else if ref {
				res.Skipped.Referenced++
				res.Items = append(res.Items, SweepItem{ID: a.ID, Action: sweepActionSkipped, Reason: sweepReasonReferenced})
			} else {
				// Counted as deleted: the dry-run answer is "this is what a
				// real call would delete", with zero writes behind it.
				res.Deleted++
				res.Items = append(res.Items, SweepItem{ID: a.ID, Action: sweepActionWouldDelete})
			}
			continue
		}
		if err := s.DeleteMediaAsset(ctx, a.ID, false); err != nil {
			if code := sweepErrorCode(err); code == "CONTENT_MEDIA_IN_USE" {
				res.Skipped.Referenced++
				res.Items = append(res.Items, SweepItem{ID: a.ID, Action: sweepActionSkipped, Reason: sweepReasonReferenced})
			} else {
				res.Skipped.Failed++
				res.Items = append(res.Items, SweepItem{ID: a.ID, Action: sweepActionSkipped, Reason: code})
			}
			continue
		}
		res.Deleted++
		res.Items = append(res.Items, SweepItem{ID: a.ID, Action: sweepActionDeleted})
	}
	if total > len(candidates) {
		res.Truncated = true
		res.Remaining = total - len(candidates)
	}
	return res, nil
}

// sweepCandidates pages the orphan list up to limit rows. No writes happen
// here, so offsets cannot shift under the paging.
func (s *contentService) sweepCandidates(ctx context.Context, tenantID string, limit int) ([]*domain.MediaAsset, int, error) {
	var out []*domain.MediaAsset
	total := 0
	for offset := 0; ; {
		page, tot, err := s.repo.ListMediaAssets(ctx, tenantID, repository.MediaListFilter{
			Orphan: true, Limit: 100, Offset: offset,
		})
		if err != nil {
			return nil, 0, err
		}
		total = tot
		out = append(out, page...)
		if len(out) >= limit || len(out) >= tot || len(page) == 0 {
			break
		}
		offset += len(page)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, total, nil
}

// sweepReferenced is the dry-run half of DeleteMediaAsset(force=false)'s
// preamble: existence plus the dual-table reference check, read-only. It
// duplicates two repository calls rather than threading a dry-run flag
// through the delete path, because a flag that close to the bytes would make
// "would delete" and "did delete" share a code path whose only difference is
// a boolean — the shape most likely to one day delete for real.
func (s *contentService) sweepReferenced(ctx context.Context, tenantID string, id uuid.UUID) (bool, error) {
	if _, err := s.repo.GetMediaAsset(ctx, tenantID, id); err != nil {
		return false, err
	}
	// limit=1: Total is the full count in both implementations, so one row
	// answers "referenced?" without fetching the preview.
	refs, err := s.repo.MediaReferencedBy(ctx, tenantID, id, 1, 0)
	if err != nil {
		return false, err
	}
	return refs.Total > 0, nil
}

func sweepErrorCode(err error) string {
	if ae, ok := apperrors.As(err); ok {
		return ae.Code
	}
	return "INTERNAL"
}
