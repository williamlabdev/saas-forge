package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/pkg/mcp"
	"github.com/williamlabdev/saas-forge/internal/pkg/metrics"
)

// --- fakes -----------------------------------------------------------------

type failCall struct {
	id      uuid.UUID
	lastErr string
	max     int
}

type fakeRepo struct {
	claim      []Row
	claimErr   error
	claimCalls int

	doneIDs   []uuid.UUID
	doneErr   error
	doneCtxOK bool // set true if the last MarkDone saw a live (non-cancelled) ctx

	failCalls []failCall
	failDead  bool
	failErr   error

	reclaimed        int64
	reclaimErr       error
	reclaimOlderThan time.Duration
	reclaimCalls     int
}

func (r *fakeRepo) EnqueueTx(context.Context, pgx.Tx, uuid.UUID, string, json.RawMessage, string) error {
	return nil
}

func (r *fakeRepo) ClaimPending(_ context.Context, _ int) ([]Row, error) {
	r.claimCalls++
	if r.claimErr != nil {
		return nil, r.claimErr
	}
	return r.claim, nil
}

func (r *fakeRepo) MarkDone(ctx context.Context, id uuid.UUID) error {
	r.doneCtxOK = ctx.Err() == nil
	r.doneIDs = append(r.doneIDs, id)
	return r.doneErr
}

func (r *fakeRepo) ReclaimStale(_ context.Context, olderThan time.Duration) (int64, error) {
	r.reclaimCalls++
	r.reclaimOlderThan = olderThan
	return r.reclaimed, r.reclaimErr
}

func (r *fakeRepo) MarkFailedWithRetry(_ context.Context, id uuid.UUID, lastErr string, max int) (bool, error) {
	r.failCalls = append(r.failCalls, failCall{id: id, lastErr: lastErr, max: max})
	return r.failDead, r.failErr
}

func (r *fakeRepo) CountPending(context.Context) (int64, error) { return 0, nil }

func (r *fakeRepo) PendingStats(context.Context) (PendingStats, error) { return PendingStats{}, nil }

type fakeClient struct {
	reqs []mcp.UpsertRequest
	fn   func(mcp.UpsertRequest) error
}

func (c *fakeClient) UpsertUserState(_ context.Context, req mcp.UpsertRequest) error {
	c.reqs = append(c.reqs, req)
	if c.fn != nil {
		return c.fn(req)
	}
	return nil
}

// --- helpers ---------------------------------------------------------------

func validRow(t *testing.T, eventType string) Row {
	t.Helper()
	payload, err := MarshalPayload(UserPayload{UserID: uuid.NewString(), Status: "active", StatusVersion: 2})
	require.NoError(t, err)
	return Row{ID: uuid.New(), EventType: eventType, Payload: payload, IdempotencyKey: uuid.NewString()}
}

// --- tests -----------------------------------------------------------------

func TestNewWorker_AppliesDefaults(t *testing.T) {
	w := NewWorker(&fakeRepo{}, &fakeClient{}, 0, -1, 0, nil)
	assert.Equal(t, 10, w.limit)
	assert.Equal(t, 5, w.maxRetries)
	assert.Equal(t, defaultStaleThreshold, w.staleThreshold, "non-positive stale threshold should fall back to default")
	assert.NotNil(t, w.registry, "nil registry should be replaced with a fresh one")
}

func TestProcessBatch_DeliversAndMarksDone(t *testing.T) {
	row := validRow(t, EventUserCreated)
	repo := &fakeRepo{claim: []Row{row}}
	client := &fakeClient{}
	reg := metrics.NewRegistry()
	w := NewWorker(repo, client, 10, 3, time.Minute, reg)
	assert.Same(t, reg, w.Registry(), "Registry() should expose the injected registry")

	require.NoError(t, w.processBatch(context.Background()))

	require.Len(t, client.reqs, 1)
	assert.Equal(t, EventUserCreated, client.reqs[0].EventType)
	assert.Equal(t, row.IdempotencyKey, client.reqs[0].IdempotencyKey)
	assert.Equal(t, []uuid.UUID{row.ID}, repo.doneIDs)
	assert.Empty(t, repo.failCalls)
	assert.Equal(t, uint64(1), reg.OutboxDelivered.Load())
	assert.Equal(t, uint64(0), reg.OutboxFailed.Load())
}

func TestProcessBatch_RetriesOnDeliveryError(t *testing.T) {
	row := validRow(t, EventUserUpdated)
	repo := &fakeRepo{claim: []Row{row}, failDead: false}
	client := &fakeClient{fn: func(mcp.UpsertRequest) error { return errors.New("mcp down") }}
	reg := metrics.NewRegistry()
	w := NewWorker(repo, client, 10, 3, time.Minute, reg)

	require.NoError(t, w.processBatch(context.Background()))

	require.Len(t, repo.failCalls, 1)
	assert.Equal(t, row.ID, repo.failCalls[0].id)
	assert.Equal(t, 3, repo.failCalls[0].max, "worker must pass its maxRetries through")
	assert.Contains(t, repo.failCalls[0].lastErr, "mcp down")
	assert.Empty(t, repo.doneIDs, "failed row must not be marked done")
	assert.Equal(t, uint64(1), reg.OutboxRetried.Load())
	assert.Equal(t, uint64(1), reg.OutboxFailed.Load())
	assert.Equal(t, uint64(0), reg.OutboxDead.Load())
	assert.Equal(t, uint64(0), reg.OutboxDelivered.Load())
}

func TestProcessBatch_DeadLettersAfterMaxRetries(t *testing.T) {
	row := validRow(t, EventUserDeleted)
	repo := &fakeRepo{claim: []Row{row}, failDead: true}
	client := &fakeClient{fn: func(mcp.UpsertRequest) error { return errors.New("still failing") }}
	reg := metrics.NewRegistry()
	w := NewWorker(repo, client, 10, 1, time.Minute, reg)

	require.NoError(t, w.processBatch(context.Background()))

	require.Len(t, repo.failCalls, 1)
	assert.Equal(t, uint64(1), reg.OutboxDead.Load())
	assert.Equal(t, uint64(1), reg.OutboxFailed.Load())
	assert.Equal(t, uint64(0), reg.OutboxRetried.Load())
}

func TestProcessBatch_PoisonInvalidJSON(t *testing.T) {
	row := Row{ID: uuid.New(), EventType: EventUserCreated, Payload: json.RawMessage(`{bad json`), IdempotencyKey: "k"}
	repo := &fakeRepo{claim: []Row{row}}
	client := &fakeClient{}
	reg := metrics.NewRegistry()
	w := NewWorker(repo, client, 10, 3, time.Minute, reg)

	require.NoError(t, w.processBatch(context.Background()))

	assert.Empty(t, client.reqs, "poison payload must not be delivered")
	require.Len(t, repo.failCalls, 1)
	assert.Contains(t, repo.failCalls[0].lastErr, "parse payload")
	assert.Empty(t, repo.doneIDs)
	assert.Equal(t, uint64(1), reg.OutboxFailed.Load())
}

func TestProcessBatch_PoisonInvalidUUID(t *testing.T) {
	payload, err := MarshalPayload(UserPayload{UserID: "not-a-uuid", Status: "active", StatusVersion: 1})
	require.NoError(t, err)
	row := Row{ID: uuid.New(), EventType: EventUserCreated, Payload: payload, IdempotencyKey: "k"}
	repo := &fakeRepo{claim: []Row{row}}
	client := &fakeClient{}
	reg := metrics.NewRegistry()
	w := NewWorker(repo, client, 10, 3, time.Minute, reg)

	require.NoError(t, w.processBatch(context.Background()))

	assert.Empty(t, client.reqs, "unparseable UUID must not be delivered")
	require.Len(t, repo.failCalls, 1)
	assert.Empty(t, repo.doneIDs)
	assert.Equal(t, uint64(1), reg.OutboxFailed.Load())
}

func TestProcessBatch_UnregisteredEventTypeFailsLoud(t *testing.T) {
	// TKT-OBX-2: an event type with no delivery handler must NOT be marked done and
	// counted as delivered. It goes through the retry/dead-letter path so the gap
	// is visible instead of a silent black hole.
	row := Row{ID: uuid.New(), EventType: "ticket.created", Payload: json.RawMessage(`{}`), IdempotencyKey: "k"}
	repo := &fakeRepo{claim: []Row{row}}
	client := &fakeClient{}
	reg := metrics.NewRegistry()
	w := NewWorker(repo, client, 10, 3, time.Minute, reg)

	require.NoError(t, w.processBatch(context.Background()))

	assert.Empty(t, client.reqs, "unregistered event type makes no MCP call")
	assert.Empty(t, repo.doneIDs, "unregistered event type must not be marked done")
	require.Len(t, repo.failCalls, 1, "unregistered event type goes through MarkFailedWithRetry")
	assert.Contains(t, repo.failCalls[0].lastErr, "no delivery handler")
	assert.Equal(t, uint64(0), reg.OutboxDelivered.Load(), "no-op must never count as delivered")
	assert.Equal(t, uint64(1), reg.OutboxFailed.Load())
	assert.Equal(t, uint64(1), reg.OutboxRetried.Load())
}

func TestProcessBatch_MarksDoneWithDetachedCtxDuringShutdown(t *testing.T) {
	// TKT-OBX-1: even if the worker ctx is already cancelled (shutdown), a claimed
	// row must still be marked so it is not left stuck in 'processing'.
	row := validRow(t, EventUserCreated)
	repo := &fakeRepo{claim: []Row{row}}
	client := &fakeClient{}
	reg := metrics.NewRegistry()
	w := NewWorker(repo, client, 10, 3, time.Minute, reg)

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel() // simulate shutdown before the batch marks its rows

	require.NoError(t, w.processBatch(cancelledCtx))

	assert.Equal(t, []uuid.UUID{row.ID}, repo.doneIDs, "claimed row is still marked done during shutdown")
	assert.True(t, repo.doneCtxOK, "MarkDone must receive a live (detached) ctx, not the cancelled one")
}

func TestRun_ReclaimsStaleEachTick(t *testing.T) {
	repo := &fakeRepo{reclaimed: 2}
	w := NewWorker(repo, &fakeClient{}, 10, 3, 90*time.Second, metrics.NewRegistry())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx, time.Millisecond)
		close(done)
	}()
	// Let at least one tick run, then stop.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}

	assert.Positive(t, repo.reclaimCalls, "Run must call ReclaimStale on its tick")
	assert.Equal(t, 90*time.Second, repo.reclaimOlderThan, "Run must pass the configured stale threshold")
}

func TestProcessBatch_ClaimPendingErrorPropagates(t *testing.T) {
	repo := &fakeRepo{claimErr: errors.New("db gone")}
	w := NewWorker(repo, &fakeClient{}, 10, 3, time.Minute, metrics.NewRegistry())

	err := w.processBatch(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db gone")
}

func TestProcessBatch_MarkDoneErrorSkipsDeliveredCounter(t *testing.T) {
	row := validRow(t, EventUserCreated)
	repo := &fakeRepo{claim: []Row{row}, doneErr: errors.New("mark done failed")}
	client := &fakeClient{}
	reg := metrics.NewRegistry()
	w := NewWorker(repo, client, 10, 3, time.Minute, reg)

	require.NoError(t, w.processBatch(context.Background()))

	require.Len(t, client.reqs, 1, "delivery happens before MarkDone")
	// MarkDone failed, so the row is not counted as delivered (it will be retried).
	assert.Equal(t, uint64(0), reg.OutboxDelivered.Load())
}

func TestProcessBatch_MarkFailedRetryErrorStillCountsFailure(t *testing.T) {
	row := validRow(t, EventUserUpdated)
	repo := &fakeRepo{
		claim:    []Row{row},
		failErr:  errors.New("mark retry failed"), // bookkeeping error is logged, not fatal
		failDead: false,
	}
	client := &fakeClient{fn: func(mcp.UpsertRequest) error { return errors.New("mcp down") }}
	reg := metrics.NewRegistry()
	w := NewWorker(repo, client, 10, 3, time.Minute, reg)

	require.NoError(t, w.processBatch(context.Background()))

	require.Len(t, repo.failCalls, 1)
	assert.Equal(t, uint64(1), reg.OutboxFailed.Load(), "failure is counted even if MarkFailedWithRetry errors")
	assert.Equal(t, uint64(1), reg.OutboxRetried.Load())
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	repo := &fakeRepo{}
	w := NewWorker(repo, &fakeClient{}, 10, 3, time.Minute, metrics.NewRegistry())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx, time.Millisecond)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

func TestProcessBatch_MixedBatchContinuesPastFailures(t *testing.T) {
	ok := validRow(t, EventUserCreated)
	failRow := validRow(t, EventUserUpdated)
	poison := Row{ID: uuid.New(), EventType: EventUserDeleted, Payload: json.RawMessage(`nope`), IdempotencyKey: "p"}
	repo := &fakeRepo{claim: []Row{ok, failRow, poison}, failDead: false}
	client := &fakeClient{fn: func(req mcp.UpsertRequest) error {
		if req.IdempotencyKey == failRow.IdempotencyKey {
			return errors.New("boom")
		}
		return nil
	}}
	reg := metrics.NewRegistry()
	w := NewWorker(repo, client, 10, 3, time.Minute, reg)

	require.NoError(t, w.processBatch(context.Background()))

	assert.Equal(t, []uuid.UUID{ok.ID}, repo.doneIDs, "only the successful row is marked done")
	require.Len(t, repo.failCalls, 2, "delivery failure + poison both go through retry path")
	assert.Len(t, client.reqs, 2, "poison never reaches the client")
	assert.Equal(t, uint64(1), reg.OutboxDelivered.Load())
	assert.Equal(t, uint64(2), reg.OutboxFailed.Load())
	assert.Equal(t, uint64(2), reg.OutboxRetried.Load())
	assert.Equal(t, uint64(0), reg.OutboxDead.Load())
}
