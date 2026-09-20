package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
)

// seedSweepOrphan runs the full reserve → land → complete cycle and then
// backdates the row, so the sweep's age filter treats it as `age` old.
func seedSweepOrphan(t *testing.T, svc ContentService, repo *memRepo, store *fakeStore, ctx context.Context, age time.Duration) uuid.UUID {
	t.Helper()
	id := uploadAsset(t, svc, repo, store, ctx)
	backdateAsset(t, repo, id, age)
	return id
}

func backdateAsset(t *testing.T, repo *memRepo, id uuid.UUID, age time.Duration) {
	t.Helper()
	for _, a := range repo.assets {
		if a.ID == id {
			a.CreatedAt = time.Now().UTC().Add(-age)
			return
		}
	}
	t.Fatalf("asset %s not found", id)
}

func assetStorageKey(t *testing.T, repo *memRepo, id uuid.UUID) string {
	t.Helper()
	for _, a := range repo.assets {
		if a.ID == id {
			return a.StorageKey
		}
	}
	t.Fatalf("asset %s not found", id)
	return ""
}

// TestSweepOrphans_DeletesOldUnreferencedKeepsRest is US1: three old orphans
// go, one fresh orphan is counted but kept, and the two referenced assets are
// never even candidates — the orphan list (dual NOT EXISTS) excludes them, so
// the per-item recheck only ever fires on a mid-sweep link (see the TOCTOU
// test below). Counts must add up against remaining.
func TestSweepOrphans_DeletesOldUnreferencedKeepsRest(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")
	const day = 24 * time.Hour

	var orphans []uuid.UUID
	var orphanKeys []string
	for i := 0; i < 3; i++ {
		id := seedSweepOrphan(t, svc, repo, store, ctx, 40*day)
		orphans = append(orphans, id)
		orphanKeys = append(orphanKeys, assetStorageKey(t, repo, id))
	}
	draftLinked := seedSweepOrphan(t, svc, repo, store, ctx, 40*day)
	pubLinked := seedSweepOrphan(t, svc, repo, store, ctx, 40*day)
	fresh := uploadAsset(t, svc, repo, store, ctx)
	entry := uuid.New()
	repo.links = map[uuid.UUID][]uuid.UUID{entry: {draftLinked}}
	repo.publishedLinks = map[uuid.UUID][]uuid.UUID{entry: {pubLinked}}

	res, err := svc.SweepOrphans(ctx, SweepInput{OlderThan: 30 * day, Limit: 100})
	require.NoError(t, err)
	require.Equal(t, 3, res.Deleted)
	require.Equal(t, 0, res.Skipped.Referenced, "referenced assets are not candidates; the recheck only fires mid-sweep")
	require.Equal(t, 1, res.Skipped.Fresh)
	require.Equal(t, 0, res.Skipped.Failed)
	require.False(t, res.Truncated)
	require.Equal(t, 0, res.Remaining)
	require.Len(t, repo.assets, 3, "draft-linked, published-linked and fresh survive")

	survivors := map[uuid.UUID]bool{draftLinked: true, pubLinked: true, fresh: true}
	for _, a := range repo.assets {
		assert.True(t, survivors[a.ID], "unexpected survivor %s", a.ID)
	}
	for _, k := range orphanKeys {
		assert.Contains(t, store.deleted, k, "orphan bytes must go with the row")
	}
	assert.Contains(t, store.objects, assetStorageKey(t, repo, draftLinked))
	_ = orphans
}

// TestSweepOrphans_DryRunWritesNothing: the same input reports what WOULD go
// but moves neither rows nor bytes.
func TestSweepOrphans_DryRunWritesNothing(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")
	const day = 24 * time.Hour

	seedSweepOrphan(t, svc, repo, store, ctx, 40*day)
	seedSweepOrphan(t, svc, repo, store, ctx, 41*day)
	uploadAsset(t, svc, repo, store, ctx) // fresh
	rowsBefore := len(repo.assets)
	objectsBefore := len(store.objects)

	res, err := svc.SweepOrphans(ctx, SweepInput{OlderThan: 30 * day, Limit: 100, DryRun: true})
	require.NoError(t, err)
	require.Equal(t, 2, res.Deleted, "dry-run counts what would be deleted")
	require.Len(t, repo.assets, rowsBefore, "dry-run must not move rows")
	require.Len(t, store.objects, objectsBefore, "dry-run must not move bytes")
	require.Empty(t, store.deleted)
	for _, it := range res.Items {
		if it.Action == "would_delete" {
			continue
		}
		require.Equal(t, "skipped", it.Action)
		require.Equal(t, "fresh", it.Reason)
	}
}

// TestSweepOrphans_TruncatesAtLimit: twelve candidates and a limit of ten
// delete ten, report truncated with two remaining; a second call finishes.
func TestSweepOrphans_TruncatesAtLimit(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")
	const day = 24 * time.Hour

	for i := 0; i < 12; i++ {
		seedSweepOrphan(t, svc, repo, store, ctx, time.Duration(40+i)*day)
	}

	res, err := svc.SweepOrphans(ctx, SweepInput{OlderThan: 30 * day, Limit: 10})
	require.NoError(t, err)
	require.Equal(t, 10, res.Deleted)
	require.True(t, res.Truncated)
	require.Equal(t, 2, res.Remaining)
	require.Len(t, repo.assets, 2)

	res, err = svc.SweepOrphans(ctx, SweepInput{OlderThan: 30 * day, Limit: 10})
	require.NoError(t, err)
	require.Equal(t, 2, res.Deleted)
	require.False(t, res.Truncated)
	require.Equal(t, 0, res.Remaining)
	require.Empty(t, repo.assets)
}

// linkRaceRepo links the victim into an entry on the third reverse-lookup —
// the moment the sweep rechecks the third candidate. See memRepo.self.
type linkRaceRepo struct {
	*memRepo
	calls  int
	victim uuid.UUID
	entry  uuid.UUID
}

func (r *linkRaceRepo) MediaReferencedBy(ctx context.Context, tenantID string, assetID uuid.UUID, limit, offset int) (repository.ReferencedByResult, error) {
	r.calls++
	if r.calls == 3 {
		if r.publishedLinks == nil {
			r.publishedLinks = map[uuid.UUID][]uuid.UUID{}
		}
		r.publishedLinks[r.entry] = append(r.publishedLinks[r.entry], r.victim)
	}
	return r.memRepo.MediaReferencedBy(ctx, tenantID, assetID, limit, offset)
}

// TestSweepOrphans_MidSweepLinkIsSkippedNotError is the TOCTOU case: an asset
// linked after enumeration but before its turn must count as
// skipped/referenced, not fail the sweep.
func TestSweepOrphans_MidSweepLinkIsSkippedNotError(t *testing.T) {
	mem := &memRepo{}
	store := newFakeStore()
	race := &linkRaceRepo{memRepo: mem, entry: uuid.New()}
	mem.self = race
	svc := WithMediaStore(NewContentServiceWithDelivery(race, authz.NewAllowAllAuthorizer(), staticPlan(Quota{}), NewDeliveryCounter()), store)
	ctx := ctxTenant("t1")
	const day = 24 * time.Hour

	// Distinct ages pin the enumeration order (newest first): the 42-day
	// asset is third, so it is the victim.
	seedSweepOrphan(t, svc, mem, store, ctx, 40*day)
	seedSweepOrphan(t, svc, mem, store, ctx, 41*day)
	victim := seedSweepOrphan(t, svc, mem, store, ctx, 42*day)
	seedSweepOrphan(t, svc, mem, store, ctx, 43*day)
	race.victim = victim

	res, err := svc.SweepOrphans(ctx, SweepInput{OlderThan: 30 * day, Limit: 100})
	require.NoError(t, err, "a mid-sweep link must not fail the sweep")
	require.Equal(t, 3, res.Deleted)
	require.Equal(t, 1, res.Skipped.Referenced)
	require.Equal(t, 0, res.Skipped.Fresh)
	require.Len(t, mem.assets, 1)
	require.Equal(t, victim, mem.assets[0].ID)
}

// TestSweepOrphans_DeletesOverdueReservationsKeepsFresh is US2: an overdue
// reservation is an orphan with no bytes (row goes, no bucket delete), and a
// five-minute-old reservation survives.
func TestSweepOrphans_DeletesOverdueReservationsKeepsFresh(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")
	const day = 24 * time.Hour

	stale, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "image/png"})
	require.NoError(t, err)
	backdateAsset(t, repo, stale.AssetID, 40*day)

	freshUp, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "image/png"})
	require.NoError(t, err)

	res, err := svc.SweepOrphans(ctx, SweepInput{OlderThan: 30 * day, Limit: 100})
	require.NoError(t, err)
	require.Equal(t, 1, res.Deleted)
	require.Equal(t, 1, res.Skipped.Fresh)
	require.False(t, res.Truncated)
	require.Empty(t, store.deleted, "a reservation has no bytes to delete")

	_, err = svc.GetMediaAsset(ctx, stale.AssetID)
	require.Error(t, err, "overdue reservation row must be gone")
	_, err = svc.GetMediaAsset(ctx, freshUp.AssetID)
	require.NoError(t, err, "fresh reservation must survive")
}
