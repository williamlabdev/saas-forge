package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Search is the cross-type endpoint (ADR-021 §3). These tests cover the
// service method itself — authorization, validation, DTO/snippet assembly and
// ordering — as the repository-level SearchEntries and the pg_trgm query it
// runs are covered separately against a real Postgres in the integration
// suite (SnippetFor's own edge cases, including CJK rune-safety, live in
// search_test.go).

// TestSearch_FindsAcrossContentTypes is the endpoint's whole reason for being:
// a term that exists in two different content types must surface both, each
// correctly labelled with its own type's slug.
func TestSearch_FindsAcrossContentTypes(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")
	seedTitledType(t, svc, owner, "page")

	_, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "unicorn ranch", "body": ""}))
	require.NoError(t, err)
	_, err = svc.CreateEntry(owner, "page", mustJSON(t, map[string]any{"title": "about the ranch", "body": ""}))
	require.NoError(t, err)
	_, err = svc.CreateEntry(owner, "page", mustJSON(t, map[string]any{"title": "unrelated", "body": ""}))
	require.NoError(t, err)

	rows, err := svc.Search(owner, SearchInput{Query: "ranch"})
	require.NoError(t, err)
	require.Len(t, rows, 2, "both entries mentioning 'ranch' must be found, across both types")

	types := map[string]bool{}
	for _, r := range rows {
		types[r.ContentType] = true
		assert.Contains(t, strings.ToLower(r.Snippet), "ranch")
	}
	assert.True(t, types["post"])
	assert.True(t, types["page"])
}

// TestSearch_MultiTermIsAnd. Two terms must both be present in the same row —
// this is what distinguishes "ranch" AND "unicorn" from an OR that would also
// return the entry mentioning only one of them.
func TestSearch_MultiTermIsAnd(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")

	_, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "unicorn ranch", "body": ""}))
	require.NoError(t, err)
	_, err = svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "unicorn parade", "body": ""}))
	require.NoError(t, err)

	rows, err := svc.Search(owner, SearchInput{Query: "unicorn ranch"})
	require.NoError(t, err)
	require.Len(t, rows, 1, "only the entry containing BOTH terms should match")
	assert.Contains(t, strings.ToLower(rows[0].Snippet), "ranch")
}

// TestSearch_OrdersNewestEditedFirst. ADR-021 §3 orders by updated_at desc.
func TestSearch_OrdersNewestEditedFirst(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")

	first, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "match one", "body": ""}))
	require.NoError(t, err)
	second, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "match two", "body": ""}))
	require.NoError(t, err)
	// Touch the first entry again so it becomes the most recently updated,
	// proving the order is by edit time and not by creation/insertion order.
	_, err = svc.UpdateEntry(owner, "post", first.ID, mustJSON(t, map[string]any{"title": "match one", "body": "revised"}), 0)
	require.NoError(t, err)

	rows, err := svc.Search(owner, SearchInput{Query: "match"})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, first.ID, rows[0].ID, "re-edited entry must sort first")
	assert.Equal(t, second.ID, rows[1].ID)
}

// TestSearch_TenantScoped mirrors TestReleaseQueueIsTenantScoped: the service
// takes the tenant from the subject, there is no parameter to override it.
func TestSearch_TenantScoped(t *testing.T) {
	svc, _ := newSvc()
	one := ctxRole("t1", "owner")
	two := ctxRole("t2", "owner")
	seedTitledType(t, svc, one, "post")
	seedTitledType(t, svc, two, "post")

	_, err := svc.CreateEntry(one, "post", mustJSON(t, map[string]any{"title": "tenant one secret", "body": ""}))
	require.NoError(t, err)

	rows, err := svc.Search(two, SearchInput{Query: "secret"})
	require.NoError(t, err)
	assert.Empty(t, rows, "tenant two must not find tenant one's content")
}

// TestSearch_ViewerCanSearch documents the deliberate deviation from
// ListPendingReview: Search reuses ActionContentList (viewer included) rather
// than a narrower verb, per the task's literal "same read-authorization as
// existing list endpoints".
func TestSearch_ViewerCanSearch(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")
	_, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "findable", "body": ""}))
	require.NoError(t, err)

	viewer := ctxRole("t1", "viewer")
	rows, err := svc.Search(viewer, SearchInput{Query: "findable"})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

// TestSearch_DeliveryCredentialRefused. Delivery already has its own per-type
// `q` reading published_search_text; this cross-type endpoint is a staff
// surface and refuses PublicDelivery outright, same as ListPendingReview.
func TestSearch_DeliveryCredentialRefused(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")
	_, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "findable", "body": ""}))
	require.NoError(t, err)

	_, err = svc.Search(ctxDelivery("t1"), SearchInput{Query: "findable"})
	require.Error(t, err)
	assert.Equal(t, "FORBIDDEN", codeOf(t, err))
}

// TestSearch_PreviewCredentialRefused. A preview subject is a delivery
// subject with PreviewEntryID set (audienceFor, dto.go) — it is caught by the
// same PublicDelivery check, not a separate preview guard; see search.go's
// comment on Search for why a dedicated guardPreviewCollection call would be
// unreachable here.
func TestSearch_PreviewCredentialRefused(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxTenant("t1")
	e := seedEntry(t, svc, owner)

	_, err := svc.Search(ctxPreview("t1", e.ID), SearchInput{Query: "a"})
	require.Error(t, err)
	assert.Equal(t, "FORBIDDEN", codeOf(t, err))
}

// TestSearch_RejectsEmptyQuery. Unlike ListEntries' `q` (where "" simply
// leaves search out of the predicate), an empty query has no per-type list to
// fall back to being cheaper than — Search exists only to search.
func TestSearch_RejectsEmptyQuery(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")

	_, err := svc.Search(owner, SearchInput{Query: ""})
	require.Error(t, err)
	assert.Equal(t, "CONTENT_SEARCH_QUERY_REQUIRED", codeOf(t, err))

	_, err = svc.Search(owner, SearchInput{Query: "   "})
	require.Error(t, err, "whitespace-only must be treated the same as empty")
	assert.Equal(t, "CONTENT_SEARCH_QUERY_REQUIRED", codeOf(t, err))
}

// TestSearch_RejectsOverlongQuery. Same 200-character cap as ListEntries' `q`.
func TestSearch_RejectsOverlongQuery(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")

	_, err := svc.Search(owner, SearchInput{Query: strings.Repeat("a", 201)})
	require.Error(t, err)
	assert.Equal(t, "CONTENT_SEARCH_QUERY_TOO_LONG", codeOf(t, err))
}

// TestSearch_RejectsUnknownStatus.
func TestSearch_RejectsUnknownStatus(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")

	_, err := svc.Search(owner, SearchInput{Query: "a", Status: "archived"})
	require.Error(t, err)
	assert.Equal(t, "CONTENT_STATUS_INVALID", codeOf(t, err))
}

// TestSearch_RejectsInvalidLocale.
func TestSearch_RejectsInvalidLocale(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")

	_, err := svc.Search(owner, SearchInput{Query: "a", Locale: "!!!"})
	require.Error(t, err)
	assert.Equal(t, "CONTENT_LOCALE_INVALID", codeOf(t, err))
}

// TestSearch_NoMatchReturnsEmptySlice, not nil — the handler marshals this
// straight into {"items": ...} and a nil slice would ship `null` instead of
// `[]`.
func TestSearch_NoMatchReturnsEmptySlice(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")
	_, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "findable", "body": ""}))
	require.NoError(t, err)

	rows, err := svc.Search(owner, SearchInput{Query: "no-such-term-anywhere"})
	require.NoError(t, err)
	require.NotNil(t, rows)
	assert.Empty(t, rows)
}
