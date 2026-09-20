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

// The four revision routes (ADR-018). The service owns the semantics; this file
// owns the wire contract — that each route exists at the path the handbook
// documents, that the ordinal and the body reach the service intact, and that a
// refusal keeps the status the service chose rather than being flattened.

func revPath(id uuid.UUID, suffix string) string {
	return "/api/v1/content/entries/" + id.String() + "/revisions" + suffix + "?type=post"
}

func restorePath(id uuid.UUID) string {
	return "/api/v1/content/entries/" + id.String() + "/restore?type=post"
}

func TestListEntryRevisions_OK(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{revisions: []service.EntryRevisionDTO{
		{RevisionNo: 2, Version: 9, PublishedAt: time.Now().UTC()},
		{RevisionNo: 1, Version: 4, PublishedAt: time.Now().UTC()},
	}}

	rec := do(t, svc, http.MethodGet, revPath(id, ""), "")

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body)
	assert.Equal(t, id, svc.lastID)
	assert.Equal(t, "post", svc.lastTypeName)

	var env struct {
		Data []service.EntryRevisionDTO `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Len(t, env.Data, 2)
	assert.Equal(t, 2, env.Data[0].RevisionNo, "the list must reach the client newest-first as the service ordered it")
}

// An entry that has never been published still answers 200 with an empty list.
// A 404 here would conflate "no releases yet" with "no such entry", and the
// console renders the two completely differently.
func TestListEntryRevisions_EmptyIsNotANotFound(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodGet, revPath(uuid.New(), ""), "")

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body)
	var env struct {
		Data []service.EntryRevisionDTO `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Empty(t, env.Data)
}

func TestGetEntryRevision_OK(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{revisionDetail: service.EntryRevisionDetailDTO{
		EntryRevisionDTO: service.EntryRevisionDTO{RevisionNo: 3, Version: 11},
		Data:             json.RawMessage(`{"title":"march"}`),
	}}

	rec := do(t, svc, http.MethodGet, revPath(id, "/3"), "")

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body)
	assert.Equal(t, 3, svc.lastRevisionNo, "the ordinal in the path never reached the service")

	var env struct {
		Data service.EntryRevisionDetailDTO `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.JSONEq(t, `{"title":"march"}`, string(env.Data.Data),
		"the detail route exists to carry the payload and it did not arrive")
}

func TestDiffEntryRevision_OK(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{revisionDiff: service.EntryRevisionDiffDTO{
		RevisionNo:   2,
		RevisionData: json.RawMessage(`{"title":"old"}`),
		Data:         json.RawMessage(`{"title":"new"}`),
		ChangedKeys:  []string{"title"},
		DroppedKeys:  []string{},
	}}

	rec := do(t, svc, http.MethodGet, revPath(id, "/2/diff"), "")

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body)
	assert.Equal(t, 2, svc.lastRevisionNo)
	var env struct {
		Data service.EntryRevisionDiffDTO `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Equal(t, []string{"title"}, env.Data.ChangedKeys)
}

// The ordinal is a positive integer and nothing else. These are malformed
// requests, not missing rows: answering 404 would send a caller who typed
// nonsense off to look for a record, and `0` in particular must not be allowed
// to reach a service that would then have to re-check it.
func TestRevisionRoutes_RejectAMalformedOrdinal(t *testing.T) {
	for _, raw := range []string{"0", "-1", "abc", "1.5"} {
		t.Run(raw, func(t *testing.T) {
			svc := &fakeContentService{}
			rec := do(t, svc, http.MethodGet, revPath(uuid.New(), "/"+raw), "")
			require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body)
			assert.Zero(t, svc.lastRevisionNo, "a malformed ordinal still reached the service")
		})
	}
}

// A restore is a POST ON THE ENTRY, not on the revision, because it writes the
// entry. 200 and not 201: nothing was created — an existing document changed.
func TestRestoreEntry_OK(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{restoreResult: service.RestoreEntryResultDTO{
		Entry: adminEntry(id, "post", ""),
		RestoredFrom: service.RestoredFromDTO{
			RevisionNo: 4, PublishedAt: time.Now().UTC(), DroppedKeys: []string{"legacy"},
		},
	}}

	rec := do(t, svc, http.MethodPost, restorePath(id), `{"revision_no":4}`)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body)
	assert.Equal(t, 4, svc.lastRestore.RevisionNo, "the body never reached the service")
	assert.Equal(t, id, svc.lastID)

	var env struct {
		Data service.RestoreEntryResultDTO `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Equal(t, 4, env.Data.RestoredFrom.RevisionNo)
	assert.Equal(t, []string{"legacy"}, env.Data.RestoredFrom.DroppedKeys,
		"the response must tell the editor what the restore discarded")
}

func TestRestoreEntry_InvalidJSON(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodPost, restorePath(uuid.New()), `{bad`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// If-Match on a restore, for the same reason it exists on a PATCH: a restore
// replaces the whole working copy, so landing it on a document that moved
// underneath the editor is the most destructive version of the lost-update
// problem this header exists to prevent.
func TestRestoreEntry_IfMatchThreadedToService(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{restoreResult: service.RestoreEntryResultDTO{Entry: adminEntry(id, "post", "")}}
	req := httptest.NewRequest(http.MethodPost, restorePath(id), bytes.NewBufferString(`{"revision_no":1}`))
	req.Header.Set("If-Match", `"7"`)
	rec := httptest.NewRecorder()
	router(svc).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body)
	assert.Equal(t, 7, svc.lastRestoreIfMatch, "If-Match did not reach the service; a restore could overwrite a newer draft")
}

func TestRestoreEntry_InvalidIfMatch(t *testing.T) {
	id := uuid.New()
	req := httptest.NewRequest(http.MethodPost, restorePath(id), bytes.NewBufferString(`{"revision_no":1}`))
	req.Header.Set("If-Match", "not-a-number")
	rec := httptest.NewRecorder()
	router(&fakeContentService{}).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// Every one of these routes forwards the service's own status. A handler that
// swallowed the code would turn "this revision was purged by retention" into a
// 500 and an editor into a bug report.
func TestRevisionRoutes_ForwardTheServiceStatus(t *testing.T) {
	id := uuid.New()
	cases := []struct {
		name   string
		err    error
		method string
		target string
		body   string
		want   int
	}{
		{"list forbidden", apperrors.ErrForbidden, http.MethodGet, revPath(id, ""), "", http.StatusForbidden},
		{"get purged", errRevisionNotFoundForTest(), http.MethodGet, revPath(id, "/1"), "", http.StatusNotFound},
		{"diff purged", errRevisionNotFoundForTest(), http.MethodGet, revPath(id, "/1/diff"), "", http.StatusNotFound},
		{"restore purged", errRevisionNotFoundForTest(), http.MethodPost, restorePath(id), `{"revision_no":1}`, http.StatusNotFound},
		{"restore conflict", errValueTakenForTest(), http.MethodPost, restorePath(id), `{"revision_no":1}`, http.StatusConflict},
		{"restore forbidden", apperrors.ErrForbidden, http.MethodPost, restorePath(id), `{"revision_no":1}`, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, &fakeContentService{err: tc.err}, tc.method, tc.target, tc.body)
			assert.Equal(t, tc.want, rec.Code, "body=%s", rec.Body)
		})
	}
}

func errRevisionNotFoundForTest() error {
	return apperrors.New("CONTENT_REVISION_NOT_FOUND", "this entry has no such published revision", http.StatusNotFound)
}

func errValueTakenForTest() error {
	return apperrors.New("CONTENT_FIELD_VALUE_TAKEN", "value is already used by another entry", http.StatusConflict)
}
