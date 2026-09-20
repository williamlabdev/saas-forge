package mediatransform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"io"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/objectstore"
)

// --- in-memory repository ----------------------------------------------------
//
// memStore reproduces the SQL semantics the worker leans on: a claim returns
// the asset's due pending rows and holds them until the "transaction" ends;
// FinishMediaVariant on pending/failed bumps attempts; a transaction that
// returns an error rolls its writes back. Deleting the asset mid-transaction
// is modelled too, because that is the race the worker's !found branch exists
// for.

type memStore struct {
	mu     sync.Mutex
	assets map[uuid.UUID]*domain.MediaAsset
	rows   map[uuid.UUID]map[string]*domain.MediaVariant
	// deleteDuring, when set, removes the asset (and its rows, as the cascade
	// would) the first time a transaction on it calls GetMediaAsset.
	deleteDuring map[uuid.UUID]bool
	listErr      error
	txCount      int
}

func newMemStore() *memStore {
	return &memStore{
		assets:       map[uuid.UUID]*domain.MediaAsset{},
		rows:         map[uuid.UUID]map[string]*domain.MediaVariant{},
		deleteDuring: map[uuid.UUID]bool{},
	}
}

func (m *memStore) addAsset(tenant, key, ct string, size int64) *domain.MediaAsset {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Unix(1_700_000_000, 0).UTC()
	a := &domain.MediaAsset{ID: uuid.New(), TenantID: tenant, StorageKey: key, ContentType: ct, SizeBytes: size, UploadedAt: &now, CreatedAt: now}
	m.assets[a.ID] = a
	rows := map[string]*domain.MediaVariant{}
	for _, p := range append([]string{domain.MediaPresetOriginal}, domain.MediaPresetNames()...) {
		v := &domain.MediaVariant{AssetID: a.ID, TenantID: tenant, Preset: p, State: domain.MediaVariantPending, CreatedAt: now, NextAttemptAt: now}
		if p == domain.MediaPresetOriginal {
			v.StorageKey = key
		}
		rows[p] = v
	}
	m.rows[a.ID] = rows
	return a
}

func (m *memStore) ListPendingMediaVariantAssets(_ context.Context, now time.Time, limit int) ([]repository.MediaVariantAssetRef, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var refs []repository.MediaVariantAssetRef
	for id, rows := range m.rows {
		for _, r := range rows {
			if r.State == domain.MediaVariantPending && !r.NextAttemptAt.After(now) {
				refs = append(refs, repository.MediaVariantAssetRef{AssetID: id, TenantID: r.TenantID})
				break
			}
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].AssetID.String() < refs[j].AssetID.String() })
	if len(refs) > limit {
		refs = refs[:limit]
	}
	return refs, nil
}

type memTx struct {
	store   *memStore
	tenant  string
	claimed map[uuid.UUID]bool
	// staged writes, applied on commit
	writes []func()
}

func (m *memStore) InTx(_ context.Context, tenantID string, fn func(Tx) error) error {
	m.mu.Lock()
	m.txCount++
	m.mu.Unlock()
	tx := &memTx{store: m, tenant: tenantID, claimed: map[uuid.UUID]bool{}}
	if err := fn(tx); err != nil {
		return err // rollback: staged writes discarded
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range tx.writes {
		w()
	}
	return nil
}

func (t *memTx) GetMediaAsset(_ context.Context, tenantID string, id uuid.UUID) (*domain.MediaAsset, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	if t.store.deleteDuring[id] {
		delete(t.store.deleteDuring, id)
		delete(t.store.assets, id)
		delete(t.store.rows, id)
	}
	a, ok := t.store.assets[id]
	if !ok || a.TenantID != tenantID {
		return nil, apperrors.ErrNotFound
	}
	cp := *a
	return &cp, nil
}

func (t *memTx) ClaimPendingMediaVariants(_ context.Context, tenantID string, assetID uuid.UUID, now time.Time) ([]*domain.MediaVariant, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	var out []*domain.MediaVariant
	for _, p := range append([]string{domain.MediaPresetOriginal}, domain.MediaPresetNames()...) {
		r, ok := t.store.rows[assetID][p]
		if !ok || r.TenantID != tenantID || r.State != domain.MediaVariantPending || r.NextAttemptAt.After(now) {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	t.claimed[assetID] = true
	return out, nil
}

func (t *memTx) FinishMediaVariant(_ context.Context, tenantID string, assetID uuid.UUID, preset string, o repository.MediaVariantOutcome) (bool, error) {
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	r, ok := t.store.rows[assetID][preset]
	if !ok || r.TenantID != tenantID {
		return false, nil
	}
	if !t.claimed[assetID] {
		return false, fmt.Errorf("finish without claim")
	}
	t.writes = append(t.writes, func() {
		r, ok := t.store.rows[assetID][preset]
		if !ok {
			return
		}
		r.State = o.State
		switch o.State {
		case domain.MediaVariantDone:
			r.StorageKey, r.ContentType, r.SizeBytes, r.WidthPx, r.HeightPx = o.StorageKey, o.ContentType, o.SizeBytes, o.WidthPx, o.HeightPx
			r.Error, r.SkipReason = "", ""
		case domain.MediaVariantSkipped:
			r.SkipReason, r.Error = o.SkipReason, ""
		case domain.MediaVariantPending, domain.MediaVariantFailed:
			r.Attempts++
			r.Error = o.Error
			if o.NextAttemptAt != nil {
				r.NextAttemptAt = *o.NextAttemptAt
			}
		}
	})
	return true, nil
}

func (m *memStore) row(id uuid.UUID, preset string) domain.MediaVariant {
	m.mu.Lock()
	defer m.mu.Unlock()
	return *m.rows[id][preset]
}

// --- in-memory bucket --------------------------------------------------------

type memBlobs struct {
	mu      sync.Mutex
	objects map[string][]byte
	types   map[string]string
	gets    int
	deleted []string
	putErr  error
	getErr  error
}

func newMemBlobs() *memBlobs {
	return &memBlobs{objects: map[string][]byte{}, types: map[string]string{}}
}

func (b *memBlobs) PresignPost(context.Context, string, time.Duration, objectstore.UploadConstraints) (objectstore.PresignedUpload, error) {
	return objectstore.PresignedUpload{}, errors.New("not used")
}
func (b *memBlobs) PresignGet(context.Context, string, time.Duration) (string, error) {
	return "", errors.New("not used")
}
func (b *memBlobs) Stat(_ context.Context, key string) (objectstore.ObjectInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	d, ok := b.objects[key]
	if !ok {
		return objectstore.ObjectInfo{}, objectstore.ErrNotFound
	}
	return objectstore.ObjectInfo{Size: int64(len(d)), ContentType: b.types[key]}, nil
}
func (b *memBlobs) Delete(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.objects, key)
	b.deleted = append(b.deleted, key)
	return nil
}
func (b *memBlobs) Get(_ context.Context, key string) (io.ReadCloser, objectstore.ObjectInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gets++
	if b.getErr != nil {
		return nil, objectstore.ObjectInfo{}, b.getErr
	}
	d, ok := b.objects[key]
	if !ok {
		return nil, objectstore.ObjectInfo{}, objectstore.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(d)), objectstore.ObjectInfo{Size: int64(len(d)), ContentType: b.types[key]}, nil
}
func (b *memBlobs) Put(_ context.Context, key string, r io.Reader, size int64, ct string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.putErr != nil {
		return b.putErr
	}
	d, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(d)) != size {
		return fmt.Errorf("size mismatch: %d vs %d", len(d), size)
	}
	b.objects[key] = d
	b.types[key] = ct
	return nil
}
func (b *memBlobs) put(key, ct string, d []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[key] = d
	b.types[key] = ct
}
func (b *memBlobs) keys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var ks []string
	for k := range b.objects {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// --- fixtures ----------------------------------------------------------------

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 60}))
	return buf.Bytes()
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newFixture() (*memStore, *memBlobs, *clock, *Worker) {
	store, blobs := newMemStore(), newMemBlobs()
	clk := &clock{t: time.Unix(1_700_000_000, 0).UTC()}
	w := NewWorker(store, blobs).WithClock(clk.now)
	return store, blobs, clk, w
}

// --- tests -------------------------------------------------------------------

func TestWorker_RendersEveryPresetForALargeJPEG(t *testing.T) {
	store, blobs, _, w := newFixture()
	src := jpegBytes(t, 2000, 1000)
	a := store.addAsset("t1", "t1/abc-00", "image/jpeg", int64(len(src)))
	blobs.put(a.StorageKey, "image/jpeg", src)

	require.NoError(t, w.Tick(context.Background()))

	assert.Equal(t, []string{"t1/abc-00", "t1/abc-00.large.jpg", "t1/abc-00.medium.jpg", "t1/abc-00.small.jpg", "t1/abc-00.thumb.jpg"}, blobs.keys())
	assert.Equal(t, 1, blobs.gets, "the source is fetched once for all presets")

	for _, c := range []struct {
		preset string
		w, h   int
	}{{"thumb", 320, 160}, {"small", 640, 320}, {"medium", 1024, 512}, {"large", 1920, 960}} {
		r := store.row(a.ID, c.preset)
		assert.Equal(t, domain.MediaVariantDone, r.State, c.preset)
		assert.Equal(t, "t1/abc-00."+c.preset+".jpg", r.StorageKey)
		assert.Equal(t, "image/jpeg", r.ContentType)
		assert.Equal(t, [2]int{c.w, c.h}, [2]int{r.WidthPx, r.HeightPx}, c.preset)
		assert.Equal(t, int64(len(blobs.objects[r.StorageKey])), r.SizeBytes, "recorded size is what was written")
		assert.Positive(t, r.SizeBytes)
		assert.Zero(t, r.Attempts)
	}
	o := store.row(a.ID, "original")
	assert.Equal(t, domain.MediaVariantDone, o.State)
	assert.Equal(t, "t1/abc-00", o.StorageKey, "the original row points at the source object itself")
	assert.Equal(t, [2]int{2000, 1000}, [2]int{o.WidthPx, o.HeightPx}, "measured, not client-declared")
	assert.Equal(t, int64(len(src)), o.SizeBytes)

	// Nothing left to do: a second tick touches nothing.
	gets := blobs.gets
	require.NoError(t, w.Tick(context.Background()))
	assert.Equal(t, gets, blobs.gets)
}

func TestWorker_NeverUpscales(t *testing.T) {
	store, blobs, _, w := newFixture()
	src := jpegBytes(t, 700, 350)
	a := store.addAsset("t1", "t1/small", "image/jpeg", int64(len(src)))
	blobs.put(a.StorageKey, "image/jpeg", src)

	require.NoError(t, w.Tick(context.Background()))

	assert.Equal(t, domain.MediaVariantDone, store.row(a.ID, "thumb").State)
	assert.Equal(t, domain.MediaVariantDone, store.row(a.ID, "small").State)
	for _, p := range []string{"medium", "large"} {
		r := store.row(a.ID, p)
		assert.Equal(t, domain.MediaVariantSkipped, r.State, p)
		assert.Equal(t, domain.MediaSkipSourceSmaller, r.SkipReason, p)
		assert.Empty(t, r.StorageKey, "a skipped rendition has no object")
	}
	assert.Equal(t, []string{"t1/small", "t1/small.small.jpg", "t1/small.thumb.jpg"}, blobs.keys())
	// A source exactly at a preset's width is not re-encoded for it either.
	src2 := jpegBytes(t, 640, 10)
	b := store.addAsset("t1", "t1/exact", "image/jpeg", int64(len(src2)))
	blobs.put(b.StorageKey, "image/jpeg", src2)
	require.NoError(t, w.Tick(context.Background()))
	assert.Equal(t, domain.MediaVariantSkipped, store.row(b.ID, "small").State)
	assert.Equal(t, domain.MediaVariantDone, store.row(b.ID, "thumb").State)
}

func TestWorker_AVIFIsSkippedWithoutFetching(t *testing.T) {
	store, blobs, _, w := newFixture()
	a := store.addAsset("t1", "t1/avif", "image/avif", 10)
	blobs.put(a.StorageKey, "image/avif", []byte("not really avif"))

	require.NoError(t, w.Tick(context.Background()))

	assert.Zero(t, blobs.gets, "unsupported formats are decided from the content type, not the bytes")
	for _, p := range append([]string{"original"}, domain.MediaPresetNames()...) {
		r := store.row(a.ID, p)
		assert.Equal(t, domain.MediaVariantSkipped, r.State, p)
		assert.Equal(t, domain.MediaSkipUnsupportedFormat, r.SkipReason, p)
	}
}

func TestWorker_GIFAndWebPComeOutAsPNG(t *testing.T) {
	store, blobs, _, w := newFixture()
	// A GIF wide enough for thumb only.
	img := image.NewRGBA(image.Rect(0, 0, 400, 100))
	var buf bytes.Buffer
	require.NoError(t, encodeGIF(&buf, img))
	a := store.addAsset("t1", "t1/anim", "image/gif", int64(buf.Len()))
	blobs.put(a.StorageKey, "image/gif", buf.Bytes())

	require.NoError(t, w.Tick(context.Background()))
	r := store.row(a.ID, "thumb")
	assert.Equal(t, domain.MediaVariantDone, r.State)
	assert.Equal(t, "t1/anim.thumb.png", r.StorageKey)
	assert.Equal(t, "image/png", r.ContentType)
	assert.Equal(t, "image/png", blobs.types[r.StorageKey])
}

func TestWorker_PixelBombRecordsOriginalDimsAndSkipsRenditions(t *testing.T) {
	store, blobs, _, w := newFixture()
	bomb := pixelBombPNG(60_000, 60_000)
	a := store.addAsset("t1", "t1/bomb", "image/png", int64(len(bomb)))
	blobs.put(a.StorageKey, "image/png", bomb)

	require.NoError(t, w.Tick(context.Background()))

	o := store.row(a.ID, "original")
	assert.Equal(t, domain.MediaVariantDone, o.State)
	assert.Equal(t, [2]int{60_000, 60_000}, [2]int{o.WidthPx, o.HeightPx}, "the header's claim is recorded; nothing was allocated for it")
	for _, p := range domain.MediaPresetNames() {
		r := store.row(a.ID, p)
		assert.Equal(t, domain.MediaVariantSkipped, r.State, p)
		assert.Equal(t, domain.MediaSkipTooLarge, r.SkipReason, p)
	}
	assert.Equal(t, []string{"t1/bomb"}, blobs.keys(), "no rendition was written")
}

func TestWorker_CorruptHeaderFailsTerminallyOnFirstAttempt(t *testing.T) {
	store, blobs, _, w := newFixture()
	a := store.addAsset("t1", "t1/junk", "image/png", 5)
	blobs.put(a.StorageKey, "image/png", []byte("junk!"))

	require.NoError(t, w.Tick(context.Background()))

	for _, p := range append([]string{"original"}, domain.MediaPresetNames()...) {
		r := store.row(a.ID, p)
		assert.Equal(t, domain.MediaVariantFailed, r.State, p)
		assert.Equal(t, 1, r.Attempts, p)
		assert.NotEmpty(t, r.Error, p)
	}
}

func TestWorker_TransientFailureRetriesWithBackoffThenFails(t *testing.T) {
	store, blobs, clk, w := newFixture()
	src := jpegBytes(t, 800, 400)
	a := store.addAsset("t1", "t1/flaky", "image/jpeg", int64(len(src)))
	blobs.put(a.StorageKey, "image/jpeg", src)
	blobs.putErr = errors.New("bucket says no")
	start := clk.now()

	// Attempt 1: rolled back, retry recorded, due in one minute.
	require.NoError(t, w.Tick(context.Background()))
	for _, p := range append([]string{"original"}, domain.MediaPresetNames()...) {
		r := store.row(a.ID, p)
		assert.Equal(t, domain.MediaVariantPending, r.State, p)
		assert.Equal(t, 1, r.Attempts, p)
		assert.Equal(t, start.Add(time.Minute), r.NextAttemptAt, p)
		assert.Contains(t, r.Error, "bucket says no")
	}
	assert.Equal(t, []string{"t1/flaky"}, blobs.keys(), "a rolled-back attempt leaves no object")

	// Not due yet: a tick 30s later does nothing.
	clk.advance(30 * time.Second)
	gets := blobs.gets
	require.NoError(t, w.Tick(context.Background()))
	assert.Equal(t, gets, blobs.gets)

	// Attempt 2: due in five minutes.
	clk.advance(31 * time.Second)
	t2 := clk.now()
	require.NoError(t, w.Tick(context.Background()))
	r := store.row(a.ID, "thumb")
	assert.Equal(t, domain.MediaVariantPending, r.State)
	assert.Equal(t, 2, r.Attempts)
	assert.Equal(t, t2.Add(5*time.Minute), r.NextAttemptAt)

	// Attempt 3: the cap. Terminal.
	clk.advance(5 * time.Minute)
	require.NoError(t, w.Tick(context.Background()))
	for _, p := range append([]string{"original"}, domain.MediaPresetNames()...) {
		r := store.row(a.ID, p)
		assert.Equal(t, domain.MediaVariantFailed, r.State, p)
		assert.Equal(t, domain.MaxMediaVariantAttempts, r.Attempts, p)
	}

	// And it is not picked up again, however far the clock goes.
	clk.advance(24 * time.Hour)
	gets = blobs.gets
	require.NoError(t, w.Tick(context.Background()))
	assert.Equal(t, gets, blobs.gets)
}

func TestWorker_RetryRecoversWhenTheBucketComesBack(t *testing.T) {
	store, blobs, clk, w := newFixture()
	src := jpegBytes(t, 800, 400)
	a := store.addAsset("t1", "t1/recover", "image/jpeg", int64(len(src)))
	blobs.put(a.StorageKey, "image/jpeg", src)
	blobs.getErr = errors.New("timeout")
	require.NoError(t, w.Tick(context.Background()))
	assert.Equal(t, 1, store.row(a.ID, "thumb").Attempts)

	blobs.getErr = nil
	clk.advance(time.Minute)
	require.NoError(t, w.Tick(context.Background()))
	r := store.row(a.ID, "thumb")
	assert.Equal(t, domain.MediaVariantDone, r.State)
	assert.Equal(t, 1, r.Attempts, "a success does not touch the attempt counter")
	assert.Empty(t, r.Error, "and clears the last error")
}

func TestWorker_AssetDeletedMidRenderCleansUpWhatItWrote(t *testing.T) {
	store, blobs, _, w := newFixture()
	src := jpegBytes(t, 800, 400)
	a := store.addAsset("t1", "t1/gone", "image/jpeg", int64(len(src)))
	blobs.put(a.StorageKey, "image/jpeg", src)
	// The asset disappears between the claim and the read — the moment the
	// DELETE's cascade wins.
	store.deleteDuring[a.ID] = true

	require.NoError(t, w.Tick(context.Background()))
	assert.Equal(t, []string{"t1/gone"}, blobs.keys(), "nothing rendered for an asset that is gone")
	_, ok := store.rows[a.ID]
	assert.False(t, ok)
}

func TestWorker_FinishMatchingNoRowDeletesTheFreshObject(t *testing.T) {
	// The narrower window: the asset survives the read but the row is gone by
	// the time the rendition is recorded. FinishMediaVariant reports !found
	// and the object just written must not be left behind.
	store, blobs, _, _ := newFixture()
	src := jpegBytes(t, 800, 400)
	a := store.addAsset("t1", "t1/late", "image/jpeg", int64(len(src)))
	blobs.put(a.StorageKey, "image/jpeg", src)
	vanish := &vanishingStore{memStore: store, after: a.ID}
	w := NewWorker(vanish, blobs)

	require.NoError(t, w.Tick(context.Background()))
	assert.Equal(t, []string{"t1/late"}, blobs.keys(), "renditions written after the row vanished were deleted again")
	assert.ElementsMatch(t, []string{"t1/late.thumb.jpg", "t1/late.small.jpg"}, blobs.deleted)
}

// vanishingStore drops the asset's rows right after the first Put-backed
// finish, i.e. once the worker has written its first rendition.
type vanishingStore struct {
	*memStore
	after uuid.UUID
}

func (v *vanishingStore) InTx(ctx context.Context, tenantID string, fn func(Tx) error) error {
	return v.memStore.InTx(ctx, tenantID, func(tx Tx) error {
		return fn(&vanishingTx{Tx: tx, s: v})
	})
}

type vanishingTx struct {
	Tx
	s     *vanishingStore
	count int
}

func (t *vanishingTx) FinishMediaVariant(ctx context.Context, tenantID string, assetID uuid.UUID, preset string, o repository.MediaVariantOutcome) (bool, error) {
	if assetID == t.s.after && preset != domain.MediaPresetOriginal {
		t.count++
		if t.count == 1 {
			// first rendition: the row is already gone
			t.s.mu.Lock()
			delete(t.s.rows, assetID)
			delete(t.s.assets, assetID)
			t.s.mu.Unlock()
		}
	}
	return t.Tx.FinishMediaVariant(ctx, tenantID, assetID, preset, o)
}

func TestWorker_ShutdownLeavesRowsPendingAndUnmarked(t *testing.T) {
	store, blobs, _, w := newFixture()
	src := jpegBytes(t, 800, 400)
	a := store.addAsset("t1", "t1/shut", "image/jpeg", int64(len(src)))
	blobs.put(a.StorageKey, "image/jpeg", src)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, w.Tick(ctx))
	r := store.row(a.ID, "thumb")
	assert.Equal(t, domain.MediaVariantPending, r.State)
	assert.Zero(t, r.Attempts, "a cancelled context is not a failed attempt")
	assert.Empty(t, r.Error)
}

func TestWorker_ListErrorIsReturnedNotSwallowed(t *testing.T) {
	store, _, _, w := newFixture()
	store.listErr = errors.New("db down")
	err := w.Tick(context.Background())
	require.ErrorContains(t, err, "db down")
}

func TestWorker_TickDrainsMoreThanOneBatch(t *testing.T) {
	store, blobs, _, w := newFixture()
	w.WithBatchSize(2)
	for i := 0; i < 5; i++ {
		src := jpegBytes(t, 400, 200)
		a := store.addAsset("t1", fmt.Sprintf("t1/k%d", i), "image/jpeg", int64(len(src)))
		blobs.put(a.StorageKey, "image/jpeg", src)
	}
	require.NoError(t, w.Tick(context.Background()))
	refs, err := store.ListPendingMediaVariantAssets(context.Background(), w.clock(), 100)
	require.NoError(t, err)
	assert.Empty(t, refs, "one tick drains the whole queue, not one batch of it")
}

func TestWorker_RunStopsOnCancel(t *testing.T) {
	_, _, _, w := newFixture()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx, time.Millisecond); close(done) }()
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestFailure_Schedule(t *testing.T) {
	w := NewWorker(nil, nil)
	now := time.Unix(1_700_000_000, 0).UTC()
	cause := errors.New("x")

	o := w.failure(&domain.MediaVariant{Attempts: 0}, cause, now, false)
	assert.Equal(t, domain.MediaVariantPending, o.State)
	assert.Equal(t, now.Add(time.Minute), *o.NextAttemptAt)

	o = w.failure(&domain.MediaVariant{Attempts: 1}, cause, now, false)
	assert.Equal(t, domain.MediaVariantPending, o.State)
	assert.Equal(t, now.Add(5*time.Minute), *o.NextAttemptAt)

	o = w.failure(&domain.MediaVariant{Attempts: 2}, cause, now, false)
	assert.Equal(t, domain.MediaVariantFailed, o.State, "the third failure is the last")
	assert.Nil(t, o.NextAttemptAt)

	o = w.failure(&domain.MediaVariant{Attempts: 0}, cause, now, true)
	assert.Equal(t, domain.MediaVariantFailed, o.State, "terminal causes skip the retries")

	long := errors.New(string(bytes.Repeat([]byte("e"), 2000)))
	o = w.failure(&domain.MediaVariant{}, long, now, false)
	assert.Len(t, o.Error, 500, "error text is truncated for the column, not the column for the error")
}

func encodeGIF(w io.Writer, img image.Image) error {
	return gif.Encode(w, img, nil)
}
