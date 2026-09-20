package service

import (
	"context"
	"sort"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// memRepo's half of entry_review_decisions (000051, ADR-014 Amendment: review
// decisions). No unique constraint to mirror — schedule_memrepo_test.go's
// header explains the pattern this file follows.

func (m *memRepo) CreateEntryReviewDecision(_ context.Context, d *domain.EntryReviewDecision) error {
	cp := *d
	m.reviewDecisions = append(m.reviewDecisions, &cp)
	return nil
}

// latestReviewDecision is the shared lookup GetLatestEntryReviewDecision and
// ListPendingReview's sent-back exclusion both use — one selection rule, not
// two that could drift.
func (m *memRepo) latestReviewDecision(tenantID string, entryID uuid.UUID) *domain.EntryReviewDecision {
	var latest *domain.EntryReviewDecision
	for _, d := range m.reviewDecisions {
		if d.TenantID != tenantID || d.EntryID != entryID {
			continue
		}
		if latest == nil || d.DecidedAt.After(latest.DecidedAt) {
			latest = d
		}
	}
	return latest
}

func (m *memRepo) GetLatestEntryReviewDecision(_ context.Context, tenantID string, entryID uuid.UUID) (*domain.EntryReviewDecision, error) {
	latest := m.latestReviewDecision(tenantID, entryID)
	if latest == nil {
		return nil, nil
	}
	cp := *latest
	return &cp, nil
}

func (m *memRepo) ListLatestEntryReviewDecisions(_ context.Context, tenantID string, entryIDs []uuid.UUID) (map[uuid.UUID]*domain.EntryReviewDecision, error) {
	if len(entryIDs) == 0 {
		return nil, nil
	}
	want := make(map[uuid.UUID]bool, len(entryIDs))
	for _, id := range entryIDs {
		want[id] = true
	}
	out := make(map[uuid.UUID]*domain.EntryReviewDecision, len(entryIDs))
	for _, id := range entryIDs {
		if !want[id] {
			continue
		}
		if latest := m.latestReviewDecision(tenantID, id); latest != nil {
			cp := *latest
			out[id] = &cp
		}
	}
	return out, nil
}

func (m *memRepo) ListEntryReviewDecisions(_ context.Context, tenantID string, entryID uuid.UUID) ([]domain.EntryReviewDecision, error) {
	var out []domain.EntryReviewDecision
	for _, d := range m.reviewDecisions {
		if d.TenantID == tenantID && d.EntryID == entryID {
			out = append(out, *d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].DecidedAt.After(out[j].DecidedAt) })
	if len(out) > domain.MaxReviewDecisionsPerEntry {
		out = out[:domain.MaxReviewDecisionsPerEntry]
	}
	return out, nil
}
