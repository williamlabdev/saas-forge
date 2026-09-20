package repository

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// media_variants against real Postgres (migration 000043). What lives here is
// exactly what the service's in-memory repository can only restate: the RLS
// policies and the worker's scan GUC, FOR UPDATE SKIP LOCKED as the claim, ON
// DELETE CASCADE, and the migration's own reversibility.

func mvNow() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// seedImage reserves, lands and enqueues one image asset for tenant, the way
// CompleteMediaUpload does, and returns its id and storage key.
func seedImage(t *testing.T, ctx context.Context, repo *PostgresContentRepository, tenant string) (uuid.UUID, string) {
	t.Helper()
	id := uuid.New()
	key := tenant + "/" + id.String() + "-abcd"
	require.NoError(t, repo.CreateMediaAsset(ctx, &domain.MediaAsset{
		ID: id, TenantID: tenant, StorageKey: key, ContentType: "image/jpeg", CreatedAt: mvNow(),
	}))
	require.NoError(t, repo.WithTx(ctx, tenant, func(r ContentRepository) error {
		if err := r.MarkMediaUploaded(ctx, tenant, id, 1000, "image/jpeg"); err != nil {
			return err
		}
		return r.EnqueueMediaVariants(ctx, tenant, id, key)
	}))
	return id, key
}

func TestMediaVariants_EnqueueIsIdempotentAndKeepsKeys(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "mv_enqueue")
	repo := NewPostgresContentRepository(pool, nil)
	id, key := seedImage(t, ctx, repo, "t1")

	rows, err := repo.ListMediaVariants(ctx, "t1", id)
	require.NoError(t, err)
	require.Len(t, rows, 5)
	assert.Equal(t, []string{"original", "thumb", "small", "medium", "large"}, presets(rows), "fixed order from the CASE, not from insertion")
	assert.Equal(t, key, rows[0].StorageKey)
	assert.Equal(t, domain.MediaVariantPending, rows[1].State)

	// Finish one done, one failed twice; re-enqueue resets the state and the
	// counter but leaves the rendition's key so the next render overwrites.
	found, err := repo.FinishMediaVariant(ctx, "t1", id, "thumb", MediaVariantOutcome{
		State: domain.MediaVariantDone, StorageKey: key + ".thumb.jpg", ContentType: "image/jpeg", SizeBytes: 10, WidthPx: 320, HeightPx: 200,
	})
	require.NoError(t, err)
	require.True(t, found)
	for i := 0; i < 2; i++ {
		next := mvNow().Add(time.Minute)
		_, err = repo.FinishMediaVariant(ctx, "t1", id, "large", MediaVariantOutcome{State: domain.MediaVariantPending, Error: "boom", NextAttemptAt: &next})
		require.NoError(t, err)
	}
	rows, err = repo.ListMediaVariants(ctx, "t1", id)
	require.NoError(t, err)
	assert.Equal(t, 2, rows[4].Attempts)
	assert.Equal(t, "boom", rows[4].Error)

	require.NoError(t, repo.EnqueueMediaVariants(ctx, "t1", id, key))
	rows, err = repo.ListMediaVariants(ctx, "t1", id)
	require.NoError(t, err)
	require.Len(t, rows, 5, "an upsert, not a second set of rows")
	for _, r := range rows {
		assert.Equal(t, domain.MediaVariantPending, r.State, r.Preset)
		assert.Zero(t, r.Attempts, r.Preset)
		assert.Empty(t, r.Error, r.Preset)
	}
	assert.Equal(t, key+".thumb.jpg", rows[1].StorageKey, "the old rendition key survives the reset")

	// Finishing a row the tenant does not own matches nothing.
	found, err = repo.FinishMediaVariant(ctx, "t2", id, "thumb", MediaVariantOutcome{State: domain.MediaVariantSkipped, SkipReason: "x"})
	require.NoError(t, err)
	assert.False(t, found)

	// The CHECK constraints are the last line: an unknown preset or state is
	// refused by the database, whatever the binary thought.
	_, err = pool.Exec(ctx, `INSERT INTO media_variants (asset_id, tenant_id, preset, state) VALUES ($1, 't1', 'huge', 'pending')`, id)
	require.Error(t, err, "preset CHECK")
	_, err = pool.Exec(ctx, `UPDATE media_variants SET state = 'processing' WHERE asset_id = $1`, id)
	require.Error(t, err, "state CHECK — there is no processing state, by design")
}

func presets(rows []*domain.MediaVariant) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Preset)
	}
	return out
}

func TestMediaVariants_WorkerScanCrossesTenantsOnlyWithItsGUC(t *testing.T) {
	ctx, pool, container := startContentDB(t, "mv_rls")
	repo := NewPostgresContentRepository(pool, nil)
	seedImage(t, ctx, repo, "t1")
	seedImage(t, ctx, repo, "t2")

	_, err := pool.Exec(ctx, `
		CREATE ROLE mtapp LOGIN PASSWORD 'mtpw' NOSUPERUSER;
		GRANT USAGE ON SCHEMA public TO mtapp;
		GRANT SELECT, INSERT, UPDATE, DELETE ON media_assets, media_variants TO mtapp;
	`)
	require.NoError(t, err)
	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "5432")
	require.NoError(t, err)
	app, err := pgxpool.New(ctx, "postgres://mtapp:mtpw@"+host+":"+port.Port()+"/mv_rls?sslmode=disable")
	require.NoError(t, err)
	defer app.Close()

	// Without any GUC the table is closed: app_current_tenant() is NULL.
	var bare int
	tx, err := app.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM media_variants`).Scan(&bare))
	_ = tx.Rollback(ctx)
	assert.Zero(t, bare, "an unscoped read must see nothing")

	// With the tenant GUC, one tenant's rows and only theirs.
	appRepo := NewPostgresContentRepository(app, nil)
	var t1Rows int
	require.NoError(t, appRepo.withTenant(ctx, "t1", func(q querier) error {
		return q.QueryRow(ctx, `SELECT count(*) FROM media_variants`).Scan(&t1Rows)
	}))
	assert.Equal(t, 5, t1Rows)

	// The scan sees both tenants' due assets, through its own GUC.
	refs, err := appRepo.ListPendingMediaVariantAssets(ctx, mvNow(), 10)
	require.NoError(t, err)
	assert.Len(t, refs, 2, "the worker serves every tenant")

	// The scan GUC is SELECT-only, reaches ONE table, and is NOT the
	// scheduler's GUC: the two escape hatches are separate on purpose so
	// widening one cannot widen the other.
	tx, err = app.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SELECT set_config('app.media_transform_scan', 'on', true)`)
	require.NoError(t, err)
	var assetsSeen, variantsSeen int
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM media_assets`).Scan(&assetsSeen))
	assert.Zero(t, assetsSeen, "the scan GUC must not open media_assets")
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM media_variants`).Scan(&variantsSeen))
	assert.Equal(t, 10, variantsSeen)
	tag, err := tx.Exec(ctx, `UPDATE media_variants SET state = 'failed'`)
	require.NoError(t, err)
	assert.Zero(t, tag.RowsAffected(), "with no tenant set an UPDATE matches no rows: the scan policy is SELECT-only")
	_ = tx.Rollback(ctx)

	tx, err = app.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SELECT set_config('app.scheduler_scan', 'on', true)`)
	require.NoError(t, err)
	var viaScheduler int
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM media_variants`).Scan(&viaScheduler))
	assert.Zero(t, viaScheduler, "the scheduler's GUC opens nothing here")
	_ = tx.Rollback(ctx)
}

func TestMediaVariants_ClaimIsExactlyOnceAcrossTwoWorkers(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "mv_skiplocked")
	repo := NewPostgresContentRepository(pool, nil)
	id, _ := seedImage(t, ctx, repo, "t1")

	// 1. Structure: a claim held by one transaction is invisible to a second
	//    concurrent one — it skips, it does not wait.
	locked := make(chan struct{})
	loserDone := make(chan struct{})
	var loser []*domain.MediaVariant
	var loserErr error
	go func() {
		defer close(loserDone)
		loserErr = repo.WithTx(ctx, "t1", func(r ContentRepository) error {
			<-locked
			var err error
			loser, err = r.ClaimPendingMediaVariants(ctx, "t1", id, mvNow())
			return err
		})
	}()
	var winner []*domain.MediaVariant
	require.NoError(t, repo.WithTx(ctx, "t1", func(r ContentRepository) error {
		var err error
		winner, err = r.ClaimPendingMediaVariants(ctx, "t1", id, mvNow())
		if err != nil {
			return err
		}
		close(locked)
		select {
		case <-loserDone:
		case <-time.After(10 * time.Second):
			t.Error("the second claim blocked instead of skipping — SKIP LOCKED is missing")
		}
		return nil
	}))
	require.NoError(t, loserErr)
	assert.Len(t, winner, 5)
	assert.Empty(t, loser)

	// A rolled-back claim leaves the rows pending and untouched.
	rows, err := repo.ListMediaVariants(ctx, "t1", id)
	require.NoError(t, err)
	for _, r := range rows {
		assert.Equal(t, domain.MediaVariantPending, r.State)
		assert.Zero(t, r.Attempts)
	}

	// 2. Volume: two workers race over N assets; every row is finished by
	//    exactly one of them.
	const n = 12
	for i := 1; i < n; i++ {
		seedImage(t, ctx, repo, "t1")
	}
	var mu sync.Mutex
	finished := map[string]int{}
	worker := func(name string) {
		for {
			refs, err := repo.ListPendingMediaVariantAssets(ctx, mvNow(), 100)
			if err != nil {
				t.Error(err)
				return
			}
			if len(refs) == 0 {
				return
			}
			for _, ref := range refs {
				err := repo.WithTx(ctx, ref.TenantID, func(r ContentRepository) error {
					claimed, err := r.ClaimPendingMediaVariants(ctx, ref.TenantID, ref.AssetID, mvNow())
					if err != nil {
						return err
					}
					for _, v := range claimed {
						found, err := r.FinishMediaVariant(ctx, ref.TenantID, ref.AssetID, v.Preset, MediaVariantOutcome{State: domain.MediaVariantSkipped, SkipReason: name})
						if err != nil {
							return err
						}
						if !found {
							return fmt.Errorf("claimed row vanished")
						}
						mu.Lock()
						finished[ref.AssetID.String()+"/"+v.Preset]++
						mu.Unlock()
					}
					return nil
				})
				if err != nil {
					t.Error(err)
				}
			}
		}
	}
	var wg sync.WaitGroup
	for _, name := range []string{"a", "b"} {
		wg.Add(1)
		go func(name string) { defer wg.Done(); worker(name) }(name)
	}
	wg.Wait()
	assert.Len(t, finished, n*5, "every row finished")
	for k, c := range finished {
		assert.Equal(t, 1, c, "%s finished more than once", k)
	}
	var pending int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM media_variants WHERE state = 'pending'`).Scan(&pending))
	assert.Zero(t, pending)

	// 3. Due-ness: a retry scheduled in the future is not offered.
	future, _ := seedImage(t, ctx, repo, "t1")
	next := mvNow().Add(time.Hour)
	for _, p := range append([]string{"original"}, domain.MediaPresetNames()...) {
		_, err := repo.FinishMediaVariant(ctx, "t1", future, p, MediaVariantOutcome{State: domain.MediaVariantPending, Error: "later", NextAttemptAt: &next})
		require.NoError(t, err)
	}
	refs, err := repo.ListPendingMediaVariantAssets(ctx, mvNow(), 100)
	require.NoError(t, err)
	assert.Empty(t, refs, "not due yet")
	refs, err = repo.ListPendingMediaVariantAssets(ctx, mvNow().Add(2*time.Hour), 100)
	require.NoError(t, err)
	assert.Len(t, refs, 1)
	require.NoError(t, repo.WithTx(ctx, "t1", func(r ContentRepository) error {
		claimed, err := r.ClaimPendingMediaVariants(ctx, "t1", future, mvNow())
		assert.Empty(t, claimed, "the claim honours next_attempt_at too")
		return err
	}))
}

func TestMediaVariants_CascadeAndDeleteReturnsRenditionKeys(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "mv_cascade")
	repo := NewPostgresContentRepository(pool, nil)
	id, key := seedImage(t, ctx, repo, "t1")
	for _, p := range []string{"thumb", "small"} {
		_, err := repo.FinishMediaVariant(ctx, "t1", id, p, MediaVariantOutcome{
			State: domain.MediaVariantDone, StorageKey: key + "." + p + ".jpg", ContentType: "image/jpeg", SizeBytes: 1, WidthPx: 1, HeightPx: 1,
		})
		require.NoError(t, err)
	}
	_, err := repo.FinishMediaVariant(ctx, "t1", id, "large", MediaVariantOutcome{State: domain.MediaVariantSkipped, SkipReason: "source_smaller"})
	require.NoError(t, err)

	// The wrong tenant deletes nothing and learns nothing.
	_, err = repo.DeleteMediaAsset(ctx, "t2", id)
	require.ErrorIs(t, err, apperrors.ErrNotFound)

	keys, err := repo.DeleteMediaAsset(ctx, "t1", id)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{key + ".thumb.jpg", key + ".small.jpg"}, keys,
		"only rendition objects that exist: not the original's key, not pending or skipped rows")
	var left int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM media_variants WHERE asset_id = $1`, id).Scan(&left))
	assert.Zero(t, left, "ON DELETE CASCADE")

	// The raw cascade too, for a delete that bypasses the repository.
	id2, _ := seedImage(t, ctx, repo, "t1")
	_, err = pool.Exec(ctx, `DELETE FROM media_assets WHERE id = $1`, id2)
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM media_variants WHERE asset_id = $1`, id2).Scan(&left))
	assert.Zero(t, left)
}

// 000043 down/up/down/up, explicitly, on a database with rows in it. The
// generic rollback test (rls_integration_test.go) checks every down's DROP
// claims against the catalog; this one checks the round trip leaves a usable
// table behind and that the down really takes the function with it.
func TestMediaVariants_Migration000043RoundTrip(t *testing.T) {
	ctx, pool, _ := startContentDB(t, "mv_migrate")
	repo := NewPostgresContentRepository(pool, nil)
	seedImage(t, ctx, repo, "t1")

	dir := contentMigrationDir()
	up, err := os.ReadFile(filepath.Join(dir, "000043_media_variants.up.sql"))
	require.NoError(t, err)
	down, err := os.ReadFile(filepath.Join(dir, "000043_media_variants.down.sql"))
	require.NoError(t, err)

	exists := func(q string) bool {
		var n int
		require.NoError(t, pool.QueryRow(ctx, q).Scan(&n))
		return n > 0
	}
	tableQ := `SELECT count(*) FROM pg_tables WHERE tablename = 'media_variants'`
	fnQ := `SELECT count(*) FROM pg_proc WHERE proname = 'app_media_transform_scan'`
	polQ := `SELECT count(*) FROM pg_policies WHERE tablename = 'media_variants'`
	idxQ := `SELECT count(*) FROM pg_indexes WHERE indexname = 'media_variants_pending_idx'`

	require.True(t, exists(tableQ))
	require.True(t, exists(fnQ))
	require.True(t, exists(polQ))
	require.True(t, exists(idxQ))

	for round := 0; round < 2; round++ {
		_, err = pool.Exec(ctx, string(down))
		require.NoError(t, err, "down round %d", round)
		assert.False(t, exists(tableQ))
		assert.False(t, exists(fnQ), "the scan function goes with the table")
		assert.False(t, exists(polQ))
		assert.False(t, exists(idxQ))
		// media_assets is untouched by the rollback.
		assert.True(t, exists(`SELECT count(*) FROM media_assets`))

		_, err = pool.Exec(ctx, string(up))
		require.NoError(t, err, "up round %d", round)
		assert.True(t, exists(tableQ))
		assert.True(t, exists(fnQ))
		assert.Equal(t, 5, count(t, ctx, pool, `SELECT count(*) FROM pg_policies WHERE tablename = 'media_variants'`))
		assert.True(t, exists(idxQ))
	}
	// Usable after the round trip.
	id, _ := seedImage(t, ctx, repo, "t1")
	rows, err := repo.ListMediaVariants(ctx, "t1", id)
	require.NoError(t, err)
	assert.Len(t, rows, 5)
}

func count(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, q).Scan(&n))
	return n
}
