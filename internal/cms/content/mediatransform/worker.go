// Package mediatransform is the background worker that renders an uploaded
// image at the fixed presets (ADR-019).
//
// It is built on the scheduler's shape (ADR-017): a cross-tenant candidate
// scan, then one tenant-scoped transaction per asset in which SELECT ... FOR
// UPDATE SKIP LOCKED is the claim and the results are written before commit.
// There is no `processing` state and no reaper; a crash rolls the claim back.
//
// This is the ONLY code in the platform that moves media bytes. ADR-005's
// "bytes never pass through the API" was always about the request path — an
// upload is a presigned POST, a read is a presigned GET, and that is still
// true. What runs here is bounded on three axes so that it can never become
// the cost model's back door: pixels (MaxSourcePixels, checked from the header
// before a decode), time (assetTimeout per asset) and attempts
// (domain.MaxMediaVariantAttempts with backoff).
package mediatransform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/objectstore"
)

// DefaultPollInterval is how often the worker looks for pending rows when the
// caller passes no interval. Shorter than the scheduler's: a thumbnail an
// editor is waiting to see in the admin UI is a different kind of latency
// from a release timed to the minute.
const DefaultPollInterval = 15 * time.Second

// DefaultBatchSize is how many ASSETS one scan returns. A tick drains the
// queue in batches of this size until a scan comes back short.
const DefaultBatchSize = 10

// assetTimeout bounds one asset's fetch + decode + every preset's render and
// upload. Sixty seconds is generous for a 50-megapixel source on one core and
// short enough that a hung bucket cannot stall the loop for a tick's worth of
// assets.
const assetTimeout = 60 * time.Second

// MaxSourceBytes is the most the worker will read from the bucket for one
// source. It equals the upload ceiling (service.MaxUploadBytes) — an object
// larger than that did not arrive through the platform's upload path.
const MaxSourceBytes int64 = 25 << 20

// markTimeout bounds the retry bookkeeping that runs after the working
// transaction has already failed (see markRetry).
const markTimeout = 5 * time.Second

// backoff is the wait before each retry, indexed by how many attempts have
// already failed. With MaxMediaVariantAttempts = 3 the third entry is the
// wait AFTER the third failure and is therefore never used; it is listed so
// raising the cap is a one-constant change with a schedule already agreed.
var backoff = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}

// Tx is the tenant-scoped slice of the repository the worker uses inside one
// asset's transaction.
type Tx interface {
	GetMediaAsset(ctx context.Context, tenantID string, id uuid.UUID) (*domain.MediaAsset, error)
	ClaimPendingMediaVariants(ctx context.Context, tenantID string, assetID uuid.UUID, now time.Time) ([]*domain.MediaVariant, error)
	FinishMediaVariant(ctx context.Context, tenantID string, assetID uuid.UUID, preset string, o repository.MediaVariantOutcome) (bool, error)
}

// Store is the repository slice the worker depends on. Narrowed for the
// reason scheduler.Store is: the fake in the tests implements exactly this.
type Store interface {
	ListPendingMediaVariantAssets(ctx context.Context, now time.Time, limit int) ([]repository.MediaVariantAssetRef, error)
	InTx(ctx context.Context, tenantID string, fn func(Tx) error) error
}

// NewStore adapts the content repository to Store.
func NewStore(repo repository.ContentRepository) Store {
	return &repoStore{repo: repo}
}

type repoStore struct{ repo repository.ContentRepository }

func (s *repoStore) ListPendingMediaVariantAssets(ctx context.Context, now time.Time, limit int) ([]repository.MediaVariantAssetRef, error) {
	return s.repo.ListPendingMediaVariantAssets(ctx, now, limit)
}

func (s *repoStore) InTx(ctx context.Context, tenantID string, fn func(Tx) error) error {
	return s.repo.WithTx(ctx, tenantID, func(tx repository.ContentRepository) error { return fn(tx) })
}

// Worker renders pending variants. Construct with NewWorker; Run it on the
// process's shutdown context.
type Worker struct {
	store Store
	blobs objectstore.Store
	batch int
	now   func() time.Time
}

// NewWorker returns a worker over store and blobs. blobs must not be nil —
// the provider that builds this decides whether media is configured at all
// and returns no worker when it is not (cmd/server/providers.go).
func NewWorker(store Store, blobs objectstore.Store) *Worker {
	return &Worker{store: store, blobs: blobs, batch: DefaultBatchSize}
}

// WithClock replaces the worker's clock; tests use it to step time.
func (w *Worker) WithClock(now func() time.Time) *Worker {
	w.now = now
	return w
}

// WithBatchSize sets how many assets one scan returns.
func (w *Worker) WithBatchSize(n int) *Worker {
	if n > 0 {
		w.batch = n
	}
	return w
}

func (w *Worker) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now().UTC()
}

// Run polls until ctx is cancelled.
func (w *Worker) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.Tick(ctx); err != nil {
				log.Printf("media transform: %v", err)
			}
		}
	}
}

// maxBatchesPerTick caps how many scans one Tick performs, so a row that the
// retry bookkeeping cannot reach (database down) is re-listed a bounded
// number of times instead of spinning the tick forever.
const maxBatchesPerTick = 100

// Tick drains the queue: scan, process, repeat until a scan comes back short.
// Exported so tests can drive the worker a step at a time.
func (w *Worker) Tick(ctx context.Context) error {
	for i := 0; i < maxBatchesPerTick; i++ {
		now := w.clock()
		refs, err := w.store.ListPendingMediaVariantAssets(ctx, now, w.batch)
		if err != nil {
			return fmt.Errorf("list pending media variants: %w", err)
		}
		for _, ref := range refs {
			if ctx.Err() != nil {
				// Shutdown: whatever is left is still pending and will be
				// picked up on the next start. Not marking here is what keeps
				// a half-drained batch from being half-recorded.
				return nil
			}
			if err := w.process(ctx, ref, now); err != nil {
				w.markRetry(ctx, ref, err)
			}
		}
		if len(refs) < w.batch {
			return nil
		}
	}
	return nil
}

// process handles one asset entirely inside one transaction: claim the due
// rows, fetch and decode the source once, render each preset, record every
// outcome, commit. An error return means the transaction rolled back and
// NOTHING was recorded — the caller then records the retry separately.
func (w *Worker) process(ctx context.Context, ref repository.MediaVariantAssetRef, now time.Time) (err error) {
	ctx, cancel := context.WithTimeout(ctx, assetTimeout)
	defer cancel()
	// A decoder fed a crafted file is the one place a panic is plausible, and
	// this goroutine serves every tenant. Recovered into an ordinary error so
	// it lands on the retry path and, after three, on the row as `failed`.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return w.store.InTx(ctx, ref.TenantID, func(tx Tx) error {
		rows, err := tx.ClaimPendingMediaVariants(ctx, ref.TenantID, ref.AssetID, now)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil // another replica has it, or it is gone
		}
		asset, err := tx.GetMediaAsset(ctx, ref.TenantID, ref.AssetID)
		if errors.Is(err, apperrors.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return w.renderAll(ctx, tx, asset, rows, now)
	})
}

// renderAll is the per-asset pipeline once the rows are claimed.
func (w *Worker) renderAll(ctx context.Context, tx Tx, asset *domain.MediaAsset, rows []*domain.MediaVariant, now time.Time) error {
	finish := func(preset string, o repository.MediaVariantOutcome) error {
		found, err := tx.FinishMediaVariant(ctx, asset.TenantID, asset.ID, preset, o)
		if err != nil {
			return err
		}
		if !found && o.State == domain.MediaVariantDone && preset != domain.MediaPresetOriginal {
			// The asset was deleted while we worked (the row lock only holds
			// the cascade back until this transaction ends, and the DELETE
			// enumerates keys under lock — but our UPDATE matching nothing
			// means it already won). Remove what we just wrote; if this
			// best-effort delete fails too the object is an orphan the
			// operator sweeps, and that is the residual window ADR-019 §5
			// names.
			_ = w.blobs.Delete(ctx, o.StorageKey)
		}
		return nil
	}
	skipAll := func(reason string) error {
		for _, r := range rows {
			if err := finish(r.Preset, repository.MediaVariantOutcome{State: domain.MediaVariantSkipped, SkipReason: reason}); err != nil {
				return err
			}
		}
		return nil
	}
	failAll := func(cause error) error {
		for _, r := range rows {
			if err := finish(r.Preset, w.failure(r, cause, now, true)); err != nil {
				return err
			}
		}
		return nil
	}

	// AVIF: refused before a single byte is fetched. ADR-019 trigger (b).
	if !decodable(asset.ContentType) {
		return skipAll(domain.MediaSkipUnsupportedFormat)
	}
	data, info, err := w.fetch(ctx, asset.StorageKey)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", asset.StorageKey, err)
	}
	p, err := probeSource(data, asset.ContentType)
	switch {
	case errors.Is(err, errUnsupportedFormat):
		return skipAll(domain.MediaSkipUnsupportedFormat)
	case errors.Is(err, errTooLarge):
		// The header is trustworthy enough to record the `original` row from —
		// that is the one thing a pixel bomb can still tell us — while every
		// rendition is refused.
		for _, r := range rows {
			o := repository.MediaVariantOutcome{State: domain.MediaVariantSkipped, SkipReason: domain.MediaSkipTooLarge}
			if r.Preset == domain.MediaPresetOriginal {
				o = originalOutcome(asset, info, p.width, p.height)
			}
			if err := finish(r.Preset, o); err != nil {
				return err
			}
		}
		return nil
	case err != nil:
		// A header that does not parse is a property of the bytes, not of the
		// moment: terminal, without spending two more attempts on it.
		return failAll(err)
	}

	src, err := decodeSource(data, p)
	if err != nil {
		return fmt.Errorf("decode %s: %w", asset.StorageKey, err)
	}

	for _, r := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.Preset == domain.MediaPresetOriginal {
			if err := finish(r.Preset, originalOutcome(asset, info, src.width, src.height)); err != nil {
				return err
			}
			continue
		}
		maxWidth, ok := domain.MediaPresets[r.Preset]
		if !ok {
			// A preset the CHECK constraint allowed but this binary does not
			// know: an ordering problem between migration and deploy. Leave
			// it for the binary that does.
			continue
		}
		if src.width <= maxWidth {
			if err := finish(r.Preset, repository.MediaVariantOutcome{State: domain.MediaVariantSkipped, SkipReason: domain.MediaSkipSourceSmaller}); err != nil {
				return err
			}
			continue
		}
		out, err := render(src, maxWidth)
		if err != nil {
			return fmt.Errorf("render %s/%s: %w", asset.StorageKey, r.Preset, err)
		}
		key := variantKey(asset.StorageKey, r.Preset, out.ext)
		if err := w.blobs.Put(ctx, key, bytes.NewReader(out.data), int64(len(out.data)), out.contentType); err != nil {
			return fmt.Errorf("put %s: %w", key, err)
		}
		if err := finish(r.Preset, repository.MediaVariantOutcome{
			State: domain.MediaVariantDone, StorageKey: key, ContentType: out.contentType,
			SizeBytes: int64(len(out.data)), WidthPx: out.width, HeightPx: out.height,
		}); err != nil {
			return err
		}
	}
	return nil
}

// variantKey derives a rendition's storage key from its source's. Source keys
// are flat — `<tenant>/<uuid>-<hex>`, no extension (service.storageKey) — so
// appending `.<preset>.<ext>` cannot collide with any source key, keeps the
// tenant prefix (a bucket policy or a prefix listing sees the family
// together), and makes the rendition findable from the source key alone
// without a table lookup, which is what lets the delete path sweep them
// even if the row is already gone.
func variantKey(sourceKey, preset, ext string) string {
	return sourceKey + "." + preset + "." + ext
}

// fetch reads the whole source. Whole, not streamed: every decoder here needs
// random access for the header pass and the pixel pass, and the size is
// bounded by MaxSourceBytes before a byte past it is read.
func (w *Worker) fetch(ctx context.Context, key string) ([]byte, objectstore.ObjectInfo, error) {
	rc, info, err := w.blobs.Get(ctx, key)
	if err != nil {
		return nil, info, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, MaxSourceBytes+1))
	if err != nil {
		return nil, info, err
	}
	if int64(len(data)) > MaxSourceBytes {
		return nil, info, fmt.Errorf("source exceeds %d bytes", MaxSourceBytes)
	}
	return data, info, nil
}

// originalOutcome is the `original` row's record: the source's own key and
// bytes, with the dimensions the platform measured itself.
func originalOutcome(asset *domain.MediaAsset, info objectstore.ObjectInfo, width, height int) repository.MediaVariantOutcome {
	ct := info.ContentType
	if ct == "" {
		ct = asset.ContentType
	}
	size := info.Size
	if size == 0 {
		size = asset.SizeBytes
	}
	return repository.MediaVariantOutcome{
		State: domain.MediaVariantDone, StorageKey: asset.StorageKey, ContentType: ct,
		SizeBytes: size, WidthPx: width, HeightPx: height,
	}
}

// failure builds the outcome for one failed attempt on row r: a retry with
// backoff while attempts remain, `failed` once they are spent, or `failed`
// straight away when terminal says the cause cannot change.
func (w *Worker) failure(r *domain.MediaVariant, cause error, now time.Time, terminal bool) repository.MediaVariantOutcome {
	msg := cause.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	next := r.Attempts + 1
	if terminal || next >= domain.MaxMediaVariantAttempts {
		return repository.MediaVariantOutcome{State: domain.MediaVariantFailed, Error: msg}
	}
	wait := backoff[len(backoff)-1]
	if r.Attempts < len(backoff) {
		wait = backoff[r.Attempts]
	}
	at := now.Add(wait)
	return repository.MediaVariantOutcome{State: domain.MediaVariantPending, Error: msg, NextAttemptAt: &at}
}

// markRetry records a failed attempt after process's transaction rolled back.
// A fresh, short transaction re-claims the rows (they were released with the
// rollback) and bumps each one; if another replica got there first the claim
// is empty and there is nothing to record. Skipped on shutdown, for the
// scheduler's reason: a cancelled context is not a failed render.
func (w *Worker) markRetry(ctx context.Context, ref repository.MediaVariantAssetRef, cause error) {
	if ctx.Err() != nil {
		return
	}
	log.Printf("media transform: asset %s: %v", ref.AssetID, cause)
	mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markTimeout)
	defer cancel()
	now := w.clock()
	err := w.store.InTx(mctx, ref.TenantID, func(tx Tx) error {
		rows, err := tx.ClaimPendingMediaVariants(mctx, ref.TenantID, ref.AssetID, now)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if _, err := tx.FinishMediaVariant(mctx, ref.TenantID, ref.AssetID, r.Preset, w.failure(r, cause, now, false)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.Printf("media transform: asset %s: record retry: %v", ref.AssetID, err)
	}
}
