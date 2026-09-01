package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Mirror is the acting half: a local copy of the tenant's published content,
// kept current by the events. It is the skeleton a CDN-purge consumer or a
// static-site rebuilder grows out of — those differ from this only in what
// they do after the fetch.
//
// WHY IT FETCHES AT ALL. The event says an entry changed, never what it now
// says (ADR-009/011). So the consumer reads it back through the public
// delivery edge, which serves published content only — a draft cannot leak
// through this path even if the event was emitted for a draft write.
//
// WHY IT KEEPS AN ETAG. The edge's default posture is `public, no-cache` plus
// a strong ETag (ADR-011): cacheable, but revalidate before each use. A
// consumer that sends If-None-Match gets a 304 with no body whenever the
// entry has not actually changed since it last looked — which is most of the
// time, since `updated` fires on writes that do not alter what is published.
type Mirror struct {
	edge   string // base URL of the delivery edge, e.g. http://delivery:4100
	client *http.Client

	mu      sync.Mutex
	entries map[string]mirrored // keyed by tenant/type/id
}

type mirrored struct {
	etag string
	body []byte
}

func NewMirror(edgeBaseURL string) *Mirror {
	return &Mirror{
		edge:    strings.TrimSuffix(edgeBaseURL, "/"),
		client:  &http.Client{Timeout: 10 * time.Second},
		entries: map[string]mirrored{},
	}
}

func key(ev Event) string {
	return ev.TenantID + "/" + ev.ContentType + "/" + ev.EntryID
}

// Apply reacts to one event. Unpublish and delete drop the local copy without
// asking the edge anything — the whole point of ADR-011 is that a takedown
// must not wait for a cache to decide it is stale.
func (m *Mirror) Apply(eventType string, ev Event) {
	switch eventType {
	case "content.entry.unpublished", "content.entry.deleted":
		m.mu.Lock()
		delete(m.entries, key(ev))
		m.mu.Unlock()
		logf("%s %s -> dropped from mirror", eventType, key(ev))
		// A CDN-backed consumer issues its purge HERE, and only here is
		// `DELIVERY_CACHE_MAX_AGE_SECONDS > 0` a defensible posture.
	case "content.entry.published", "content.entry.updated", "content.entry.created":
		if err := m.refresh(context.Background(), ev); err != nil {
			// A real consumer retries with backoff or re-enqueues. It must NOT
			// try to make the sender retry: we already answered 200, and the
			// sender's budget is shared with every other endpoint.
			logf("%s %s -> refresh failed: %v", eventType, key(ev), err)
		}
	default:
		logf("%s: no action (unknown event type)", eventType)
	}
}

// refresh re-reads one entry through the edge, using the stored ETag.
func (m *Mirror) refresh(ctx context.Context, ev Event) error {
	m.mu.Lock()
	prev := m.entries[key(ev)]
	m.mu.Unlock()

	u := fmt.Sprintf("%s/v1/%s/%s/%s", m.edge,
		url.PathEscape(ev.TenantID), url.PathEscape(ev.ContentType), url.PathEscape(ev.EntryID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if prev.etag != "" {
		req.Header.Set("If-None-Match", prev.etag)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		logf("%s -> 304, mirror already current", key(ev))
		return nil
	case http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return err
		}
		m.mu.Lock()
		m.entries[key(ev)] = mirrored{etag: resp.Header.Get("ETag"), body: body}
		m.mu.Unlock()
		logf("%s -> mirrored %d bytes (etag %s)", key(ev), len(body), resp.Header.Get("ETag"))
		return nil
	case http.StatusNotFound:
		// The edge serves published content only. A 404 after a `created` or
		// `updated` event is the NORMAL case for a draft, not an error.
		m.mu.Lock()
		delete(m.entries, key(ev))
		m.mu.Unlock()
		logf("%s -> 404 (not published), dropped from mirror", key(ev))
		return nil
	default:
		return fmt.Errorf("edge answered %d", resp.StatusCode)
	}
}

// Len reports how many entries the mirror holds (used by the tests and by the
// /debug endpoint).
func (m *Mirror) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}
