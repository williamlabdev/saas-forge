package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/service"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// The three schedule routes (ADR-017). The service's own tests own the
// semantics; what is checked here is the wire contract — status codes, that the
// body reaches the service verbatim, and that a refusal keeps the status the
// service chose instead of being flattened into a 500.

// decodeSchedule unwraps the success envelope. Reading rec.Body straight into
// the DTO decodes into nothing and silently passes every field assertion, which
// is worth a helper.
func decodeSchedule(t *testing.T, rec *httptest.ResponseRecorder) service.EntryScheduleDTO {
	t.Helper()
	var env struct {
		Data service.EntryScheduleDTO `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	return env.Data
}

func schedulePath(id uuid.UUID) string {
	return "/api/v1/content/entries/" + id.String() + "/schedule?type=post"
}

func TestScheduleEntry_Created(t *testing.T) {
	id := uuid.New()
	runAt := time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
	svc := &fakeContentService{scheduleDTO: service.EntryScheduleDTO{
		ID: uuid.New(), Action: domain.ScheduleActionPublish, RunAt: runAt,
		PinnedVersion: 4, State: domain.ScheduleStatePending,
	}}

	rec := do(t, svc, http.MethodPost, schedulePath(id),
		`{"action":"publish","run_at":"2026-12-01T09:00:00Z"}`)

	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body)
	// 201, not 200: this creates a resource that did not exist and now has its
	// own GET and DELETE.
	assert.Equal(t, domain.ScheduleActionPublish, svc.lastSchedule.Action)
	// The timestamp is forwarded as the caller wrote it. The service is the only
	// place that decides what a usable time is, and a handler that pre-parsed it
	// would answer with its own error code for half the malformed cases.
	assert.Equal(t, "2026-12-01T09:00:00Z", svc.lastSchedule.RunAt)
	assert.Equal(t, id, svc.lastID)

	assert.Equal(t, 4, decodeSchedule(t, rec).PinnedVersion)
}

func TestScheduleEntry_RejectsUnknownFields(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodPost, schedulePath(uuid.New()),
		`{"action":"publish","run_at":"2026-12-01T09:00:00Z","force":true}`)
	// DisallowUnknownFields, like every other content POST: a caller who
	// misspells a key must be told, not silently obeyed in part.
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// A refusal from the authorization layer must arrive as 403. This is the route
// half of ADR-014 §1's gate: scheduling a publish asks for content:publish, so
// a caller without it is refused at the door rather than at 3am on Friday.
func TestScheduleEntry_ForbiddenSurvivesTheHandler(t *testing.T) {
	svc := &fakeContentService{err: apperrors.ErrForbidden}
	rec := do(t, svc, http.MethodPost, schedulePath(uuid.New()),
		`{"action":"publish","run_at":"2026-12-01T09:00:00Z"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body)
}

func TestScheduleEntry_PropagatesTheServiceErrorCode(t *testing.T) {
	for code, status := range map[string]int{
		"CONTENT_SCHEDULE_ACTION_INVALID": http.StatusUnprocessableEntity,
		"CONTENT_SCHEDULE_TIME_INVALID":   http.StatusUnprocessableEntity,
		"CONTENT_SCHEDULE_NOOP":           http.StatusUnprocessableEntity,
		"CONTENT_SCHEDULE_EXISTS":         http.StatusConflict,
	} {
		t.Run(code, func(t *testing.T) {
			svc := &fakeContentService{err: apperrors.New(code, "no", status)}
			rec := do(t, svc, http.MethodPost, schedulePath(uuid.New()),
				`{"action":"publish","run_at":"2026-12-01T09:00:00Z"}`)
			require.Equal(t, status, rec.Code, "body=%s", rec.Body)
			assert.Contains(t, rec.Body.String(), code,
				"the code is what a client branches on; a status alone cannot tell EXISTS from NOOP")
		})
	}
}

func TestCancelEntrySchedule_NoContent(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{}

	rec := do(t, svc, http.MethodDelete, schedulePath(id), "")

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, 1, svc.cancelCalls)
	assert.Equal(t, id, svc.lastID)
}

func TestCancelEntrySchedule_NotFound(t *testing.T) {
	svc := &fakeContentService{err: apperrors.New("CONTENT_SCHEDULE_NOT_FOUND", "none", http.StatusNotFound)}
	rec := do(t, svc, http.MethodDelete, schedulePath(uuid.New()), "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestGetEntrySchedule_OK(t *testing.T) {
	svc := &fakeContentService{scheduleDTO: service.EntryScheduleDTO{
		ID: uuid.New(), Action: domain.ScheduleActionUnpublish,
		State: domain.ScheduleStateDone, PinnedVersion: 2,
	}}
	rec := do(t, svc, http.MethodGet, schedulePath(uuid.New()), "")
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body)

	// A terminal schedule is a 200, not a 404: "it already ran" and "there was
	// never one" are different answers and the editor needs to tell them apart.
	assert.Equal(t, domain.ScheduleStateDone, decodeSchedule(t, rec).State)
}

func TestGetEntrySchedule_NotFound(t *testing.T) {
	svc := &fakeContentService{err: apperrors.New("CONTENT_SCHEDULE_NOT_FOUND", "none", http.StatusNotFound)}
	rec := do(t, svc, http.MethodGet, schedulePath(uuid.New()), "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestScheduleRoutes_RequireTheEntryID(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodPost,
		"/api/v1/content/entries/not-a-uuid/schedule?type=post",
		`{"action":"publish","run_at":"2026-12-01T09:00:00Z"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
