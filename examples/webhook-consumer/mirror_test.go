package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testEvent() Event {
	return Event{TenantID: "acme", EntryID: "e1", ContentType: "article", Locale: "default"}
}

// fakeEdge stands in for the delivery edge, recording what the consumer asked
// for so the ETag behaviour can be asserted rather than assumed.
type fakeEdge struct {
	etag     string
	status   int
	body     string
	calls    atomic.Int32
	lastINM  string
	lastPath string
}

func (f *fakeEdge) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		f.lastINM = r.Header.Get("If-None-Match")
		f.lastPath = r.URL.Path
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		if f.lastINM != "" && f.lastINM == f.etag {
			w.Header().Set("ETag", f.etag)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", f.etag)
		w.Header().Set("Cache-Control", "public, no-cache")
		_, _ = w.Write([]byte(f.body))
	}))
}

func TestPublishedEventMirrorsTheEntry(t *testing.T) {
	edge := &fakeEdge{etag: `"v1"`, body: `{"id":"e1","data":{"title":"hi"}}`}
	srv := edge.server()
	defer srv.Close()

	m := NewMirror(srv.URL)
	require.NoError(t, m.refresh(context.Background(), testEvent()))
	assert.Equal(t, 1, m.Len())
	assert.Equal(t, "/v1/acme/article/e1", edge.lastPath)
	assert.Empty(t, edge.lastINM, "nothing mirrored yet, so no validator to send")
}

func TestSecondRefreshRevalidatesWithTheStoredEtag(t *testing.T) {
	// This is the reason ADR-011 ships a strong ETag with `no-cache`: the
	// consumer stays exactly current without re-downloading unchanged bodies.
	edge := &fakeEdge{etag: `"v1"`, body: `{"id":"e1"}`}
	srv := edge.server()
	defer srv.Close()

	m := NewMirror(srv.URL)
	require.NoError(t, m.refresh(context.Background(), testEvent()))
	require.NoError(t, m.refresh(context.Background(), testEvent()))

	assert.Equal(t, int32(2), edge.calls.Load())
	assert.Equal(t, `"v1"`, edge.lastINM, "the stored ETag must be offered back")
	assert.Equal(t, 1, m.Len())
}

func TestUnpublishDropsTheCopyWithoutAskingTheEdge(t *testing.T) {
	// The takedown must not depend on a cache agreeing that it is stale —
	// that is the failure ADR-011 exists to fix.
	edge := &fakeEdge{etag: `"v1"`, body: `{"id":"e1"}`}
	srv := edge.server()
	defer srv.Close()

	m := NewMirror(srv.URL)
	require.NoError(t, m.refresh(context.Background(), testEvent()))
	before := edge.calls.Load()

	m.Apply("content.entry.unpublished", testEvent())
	assert.Equal(t, 0, m.Len())
	assert.Equal(t, before, edge.calls.Load(), "no fetch should be needed to take content down")
}

func TestDeleteDropsTheCopyToo(t *testing.T) {
	edge := &fakeEdge{etag: `"v1"`, body: `{"id":"e1"}`}
	srv := edge.server()
	defer srv.Close()

	m := NewMirror(srv.URL)
	require.NoError(t, m.refresh(context.Background(), testEvent()))
	// Deletion events carry no locale — the row is gone by then.
	ev := testEvent()
	ev.Locale = ""
	m.Apply("content.entry.deleted", ev)
	assert.Equal(t, 0, m.Len())
}

func TestNotFoundMeansDraftAndIsNotAnError(t *testing.T) {
	// `created` and `updated` fire for drafts too, and the edge serves
	// published content only. Treating that 404 as a failure would make every
	// draft save look like an outage.
	edge := &fakeEdge{status: http.StatusNotFound}
	srv := edge.server()
	defer srv.Close()

	m := NewMirror(srv.URL)
	assert.NoError(t, m.refresh(context.Background(), testEvent()))
	assert.Equal(t, 0, m.Len())
}

func TestEdgeFailureIsReportedAndLeavesTheMirrorAlone(t *testing.T) {
	edge := &fakeEdge{etag: `"v1"`, body: `{"id":"e1"}`}
	srv := edge.server()
	defer srv.Close()
	m := NewMirror(srv.URL)
	require.NoError(t, m.refresh(context.Background(), testEvent()))

	edge.status = http.StatusInternalServerError
	err := m.refresh(context.Background(), testEvent())
	assert.Error(t, err)
	assert.Equal(t, 1, m.Len(), "a failed refresh must not silently empty the mirror")
}

func TestUnknownEventTypeDoesNothing(t *testing.T) {
	edge := &fakeEdge{etag: `"v1"`, body: `{}`}
	srv := edge.server()
	defer srv.Close()
	m := NewMirror(srv.URL)
	m.Apply("content.entry.archived", testEvent())
	assert.Equal(t, 0, m.Len())
	assert.Equal(t, int32(0), edge.calls.Load())
}
