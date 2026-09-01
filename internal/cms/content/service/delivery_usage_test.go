package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
)

// failingRepo fails AddDeliveryReads a fixed number of times, then succeeds.
type failingRepo struct {
	*memRepo
	mu        sync.Mutex
	failsLeft int
	attempts  int
}

func (f *failingRepo) AddDeliveryReads(ctx context.Context, tenant string, day time.Time, n int64) error {
	f.mu.Lock()
	f.attempts++
	if f.failsLeft > 0 {
		f.failsLeft--
		f.mu.Unlock()
		return errors.New("db down")
	}
	f.mu.Unlock()
	return f.memRepo.AddDeliveryReads(ctx, tenant, day, n)
}

var _ repository.ContentRepository = (*failingRepo)(nil)

func TestDeliveryCounter_FlushAggregates(t *testing.T) {
	c := NewDeliveryCounter()
	repo := &memRepo{}
	day := time.Now().UTC()

	for range 3 {
		c.Record("t1")
	}
	c.Record("t2")

	require.NoError(t, c.Flush(context.Background(), repo, day))

	got, _ := repo.DeliveryReadsForDay(context.Background(), "t1", day)
	require.Equal(t, int64(3), got, "three reads must fold into one write")
	got2, _ := repo.DeliveryReadsForDay(context.Background(), "t2", day)
	require.Equal(t, int64(1), got2)
}

// Draining must reset the buffer, or a second flush would double-count.
func TestDeliveryCounter_FlushIsNotCumulative(t *testing.T) {
	c := NewDeliveryCounter()
	repo := &memRepo{}
	day := time.Now().UTC()

	c.Record("t1")
	require.NoError(t, c.Flush(context.Background(), repo, day))
	require.NoError(t, c.Flush(context.Background(), repo, day))

	got, _ := repo.DeliveryReadsForDay(context.Background(), "t1", day)
	require.Equal(t, int64(1), got, "an empty second flush must not re-add")
}

// A failed write must put the count BACK — dropping it would silently
// under-report exactly when the database is struggling.
func TestDeliveryCounter_FailedFlushIsRetried(t *testing.T) {
	repo := &failingRepo{memRepo: &memRepo{}, failsLeft: 1}
	c := NewDeliveryCounter()
	day := time.Now().UTC()

	for range 5 {
		c.Record("t1")
	}
	require.Error(t, c.Flush(context.Background(), repo, day), "first flush should surface the failure")

	got, _ := repo.DeliveryReadsForDay(context.Background(), "t1", day)
	require.Equal(t, int64(0), got, "nothing should have landed yet")

	require.NoError(t, c.Flush(context.Background(), repo, day))
	got, _ = repo.DeliveryReadsForDay(context.Background(), "t1", day)
	require.Equal(t, int64(5), got, "the retained count must be retried, not lost")
}

func TestDeliveryCounter_NilSafe(t *testing.T) {
	var c *DeliveryCounter
	c.Record("t1") // must not panic
	require.NoError(t, c.Flush(context.Background(), &memRepo{}, time.Now()))
}

func TestDeliveryCounter_ConcurrentRecord(t *testing.T) {
	c := NewDeliveryCounter()
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() { defer wg.Done(); c.Record("t1") }()
	}
	wg.Wait()

	repo := &memRepo{}
	day := time.Now().UTC()
	require.NoError(t, c.Flush(context.Background(), repo, day))
	got, _ := repo.DeliveryReadsForDay(context.Background(), "t1", day)
	require.Equal(t, int64(50), got)
}

// --- service-level: only delivery reads are counted --------------------------

func newSvcWithCounter() (ContentService, *memRepo, *DeliveryCounter) {
	repo := &memRepo{}
	c := NewDeliveryCounter()
	return NewContentServiceWithDelivery(repo, authz.NewAllowAllAuthorizer(), staticPlan(Quota{}), c), repo, c
}

func TestDeliveryReads_CountedForDeliveryOnly(t *testing.T) {
	svc, repo, counter := newSvcWithCounter()
	admin := ctxTenant("t1")
	e := seedEntry(t, svc, admin)
	if _, err := svc.SetEntryStatus(admin, "order", e.ID, domain.StatusPublished, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Admin traffic must NOT count as public delivery volume.
	if _, err := svc.ListEntries(admin, "order", ListEntriesInput{}); err != nil {
		t.Fatalf("admin list: %v", err)
	}
	if _, err := svc.GetEntry(admin, "order", e.ID); err != nil {
		t.Fatalf("admin get: %v", err)
	}

	del := ctxDelivery("t1")
	if _, err := svc.ListEntries(del, "order", ListEntriesInput{}); err != nil {
		t.Fatalf("delivery list: %v", err)
	}
	if _, err := svc.GetEntry(del, "order", e.ID); err != nil {
		t.Fatalf("delivery get: %v", err)
	}

	day := time.Now().UTC()
	require.NoError(t, counter.Flush(context.Background(), repo, day))
	got, _ := repo.DeliveryReadsForDay(context.Background(), "t1", day)
	require.Equal(t, int64(2), got, "only the two delivery reads count")
}

// A delivery request that 404s (draft by id) must not be counted — nothing was
// delivered.
func TestDeliveryReads_NotCountedWhenNothingDelivered(t *testing.T) {
	svc, repo, counter := newSvcWithCounter()
	admin := ctxTenant("t1")
	draft := seedEntry(t, svc, admin)

	if _, err := svc.GetEntry(ctxDelivery("t1"), "order", draft.ID); err == nil {
		t.Fatal("expected 404 for a draft")
	}

	day := time.Now().UTC()
	require.NoError(t, counter.Flush(context.Background(), repo, day))
	got, _ := repo.DeliveryReadsForDay(context.Background(), "t1", day)
	require.Equal(t, int64(0), got)
}

func TestUsage_ReportsDeliveryReads(t *testing.T) {
	svc, repo, counter := newSvcWithCounter()
	admin := ctxTenant("t1")
	e := seedEntry(t, svc, admin)
	if _, err := svc.SetEntryStatus(admin, "order", e.ID, domain.StatusPublished, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}
	for range 4 {
		if _, err := svc.ListEntries(ctxDelivery("t1"), "order", ListEntriesInput{}); err != nil {
			t.Fatalf("delivery list: %v", err)
		}
	}
	require.NoError(t, counter.Flush(context.Background(), repo, time.Now().UTC()))

	usage, err := svc.Usage(admin)
	require.NoError(t, err)
	require.Equal(t, int64(4), usage.DeliveryReadsToday)
}

// The translation group view is a delivery read like any other. It used to
// record nothing, so every read through this path simply escaped quota and
// billing while GetEntry/ListEntries/ResolveMediaURL all counted.
func TestDeliveryReads_CountedForTranslationGroupView(t *testing.T) {
	svc, repo, counter := newSvcWithCounter()
	admin := ctxTenant("t1")
	e := seedEntry(t, svc, admin)
	if _, err := svc.SetEntryStatus(admin, "order", e.ID, domain.StatusPublished, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if _, err := svc.ListTranslations(admin, "order", e.ID); err != nil {
		t.Fatalf("admin translations: %v", err)
	}
	if _, err := svc.ListTranslations(ctxDelivery("t1"), "order", e.ID); err != nil {
		t.Fatalf("delivery translations: %v", err)
	}

	day := time.Now().UTC()
	require.NoError(t, counter.Flush(context.Background(), repo, day))
	got, _ := repo.DeliveryReadsForDay(context.Background(), "t1", day)
	require.Equal(t, int64(1), got, "the delivery read counts, the admin one does not")
}

// GetEntry 404s an unpublished id specifically so that "exists but forbidden" is
// indistinguishable from "does not exist". The group view must answer the same
// way, or it becomes the oracle GetEntry refuses to be — telling a holder of a
// legitimately-obtained id whether the row was retracted or deleted.
func TestDelivery_TranslationGroupViewIsNotAnExistenceOracle(t *testing.T) {
	svc, _, _ := newSvcWithCounter()
	admin := ctxTenant("t1")
	unpublished := seedEntry(t, svc, admin) // stays draft
	del := ctxDelivery("t1")

	_, errUnpublished := svc.ListTranslations(del, "order", unpublished.ID)
	_, errMissing := svc.ListTranslations(del, "order", uuid.New())

	if errUnpublished == nil {
		t.Fatal("an unpublished source answered successfully — that is the oracle")
	}
	// Indistinguishable is the whole point: same code, same message.
	if errUnpublished.Error() != errMissing.Error() {
		t.Fatalf("unpublished (%v) and nonexistent (%v) must be indistinguishable", errUnpublished, errMissing)
	}

	// The admin still sees their own draft's group — this restricts one audience.
	if _, err := svc.ListTranslations(admin, "order", unpublished.ID); err != nil {
		t.Fatalf("admin must still read the group of their own draft: %v", err)
	}
}
