package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/service"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// The two review-decision routes (ADR-014 Amendment: review decisions). The
// service owns the semantics (reason validation, the queue predicate, the
// notification); this file owns the wire contract — status codes, that the
// body and the If-Match precondition reach the service intact, and that a
// refusal keeps the status the service chose rather than being flattened.

func requestChangesPath(id uuid.UUID) string {
	return "/api/v1/content/entries/" + id.String() + "/review/request-changes?type=post"
}

func reviewDecisionsPath(id uuid.UUID) string {
	return "/api/v1/content/entries/" + id.String() + "/review-decisions?type=post"
}

func decodeReviewDecision(t *testing.T, rec *httptest.ResponseRecorder) service.ReviewDecisionDTO {
	t.Helper()
	var env struct {
		Data service.ReviewDecisionDTO `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	return env.Data
}

// 201, not 200: a decision is a new row in an append-only history, the same
// reasoning ScheduleEntry's own 201 rests on — this creates a resource that
// did not exist a moment ago.
func TestRequestEntryChanges_Created(t *testing.T) {
	id := uuid.New()
	decidedBy := uuid.New()
	decidedAt := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	svc := &fakeContentService{reviewDecisionDTO: service.ReviewDecisionDTO{
		ID: uuid.New(), Decision: "changes_requested", Reason: "typo in the title",
		EntryVersion: 3, DecidedBy: decidedBy, DecidedAt: decidedAt,
	}}

	rec := do(t, svc, http.MethodPost, requestChangesPath(id), `{"reason":"typo in the title"}`)

	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body)
	assert.Equal(t, id, svc.lastID)
	assert.Equal(t, "typo in the title", svc.lastRequestChanges.Reason,
		"the reason must reach the service verbatim; it is the one thing the author reads")

	got := decodeReviewDecision(t, rec)
	assert.Equal(t, "changes_requested", got.Decision)
	assert.Equal(t, 3, got.EntryVersion)
	assert.Equal(t, decidedBy, got.DecidedBy)
}

func TestRequestEntryChanges_InvalidJSON(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodPost, requestChangesPath(uuid.New()), `{bad`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRequestEntryChanges_RejectsUnknownFields(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodPost, requestChangesPath(uuid.New()),
		`{"reason":"no","force":true}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// If-Match, for the same reason RestoreEntry has one: sending an entry back
// against a version the reviewer is no longer looking at is the same lost-
// update shape a stale restore is.
func TestRequestEntryChanges_IfMatchThreadedToService(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{reviewDecisionDTO: service.ReviewDecisionDTO{ID: uuid.New()}}
	req := httptest.NewRequest(http.MethodPost, requestChangesPath(id), bytes.NewBufferString(`{"reason":"no"}`))
	req.Header.Set("If-Match", `"5"`)
	rec := httptest.NewRecorder()
	router(svc).ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body)
	assert.Equal(t, 5, svc.lastRequestChangesIfMatch,
		"If-Match did not reach the service; a decision could land against a version the reviewer never saw")
}

func TestRequestEntryChanges_InvalidIfMatch(t *testing.T) {
	id := uuid.New()
	req := httptest.NewRequest(http.MethodPost, requestChangesPath(id), bytes.NewBufferString(`{"reason":"no"}`))
	req.Header.Set("If-Match", "not-a-number")
	rec := httptest.NewRecorder()
	router(&fakeContentService{}).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// Every status the service can return for this route must survive the
// handler unmangled — 422 for a bad reason, 409 for "not pending" or a
// version conflict, 403 for a caller (human or agent) without content:publish.
// The route asks for the SAME verb PublishEntry does, so a credential that
// gets 403 from /publish must get 403 here too — that parity is the whole
// point of reusing the verb instead of inventing a new one, and this table is
// what would catch the day someone wires the handler to a different check.
func TestRequestEntryChanges_ForwardsTheServiceStatus(t *testing.T) {
	id := uuid.New()
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"forbidden (human without content:publish, or any agent)", apperrors.ErrForbidden, http.StatusForbidden},
		{"reason required", apperrors.New("CONTENT_REVIEW_REASON_REQUIRED", "no", http.StatusUnprocessableEntity), http.StatusUnprocessableEntity},
		{"reason too long", apperrors.New("CONTENT_REVIEW_REASON_TOO_LONG", "no", http.StatusUnprocessableEntity), http.StatusUnprocessableEntity},
		{"not pending", apperrors.New("CONTENT_ENTRY_NOT_PENDING", "no", http.StatusConflict), http.StatusConflict},
		{"version conflict", errValueTakenForTest(), http.StatusConflict},
		{"not found", apperrors.ErrNotFound, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, &fakeContentService{err: tc.err}, http.MethodPost, requestChangesPath(id), `{"reason":"no"}`)
			require.Equal(t, tc.want, rec.Code, "body=%s", rec.Body)
		})
	}
}

func TestListEntryReviewDecisions_OK(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{reviewDecisions: []service.ReviewDecisionDTO{
		{ID: uuid.New(), Decision: "changes_requested", Reason: "second look", EntryVersion: 4},
		{ID: uuid.New(), Decision: "changes_requested", Reason: "first look", EntryVersion: 2},
	}}

	rec := do(t, svc, http.MethodGet, reviewDecisionsPath(id), "")

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body)
	assert.Equal(t, id, svc.lastID)

	var env struct {
		Data []service.ReviewDecisionDTO `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Len(t, env.Data, 2)
	assert.Equal(t, 4, env.Data[0].EntryVersion, "the list must reach the client newest-first as the service ordered it")
}

// An entry never sent back still answers 200 with an empty list — the same
// "empty is not a 404" rule ListEntryRevisions follows, and for the same
// reason: a 404 here would read as "no such entry" to the console.
func TestListEntryReviewDecisions_EmptyIsNotANotFound(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodGet, reviewDecisionsPath(uuid.New()), "")

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body)
	var env struct {
		Data []service.ReviewDecisionDTO `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Empty(t, env.Data)
}

func TestListEntryReviewDecisions_ForwardsForbidden(t *testing.T) {
	rec := do(t, &fakeContentService{err: apperrors.ErrForbidden}, http.MethodGet, reviewDecisionsPath(uuid.New()), "")
	assert.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body)
}

func TestReviewDecisionRoutes_RequireTheEntryID(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodPost,
		"/api/v1/content/entries/not-a-uuid/review/request-changes?type=post", `{"reason":"no"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
