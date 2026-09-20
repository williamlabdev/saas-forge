package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ListLocales is GET /api/v1/content/locales (T2 2.3): the one inventory read
// missing that stops a console from drawing a locale switcher — see locales.go
// for why its audience and refusal shape mirror Search exactly.

// TestListLocales_DistinctAndCounted proves the core shape: one row per
// distinct locale in the tenant, each carrying how many entries use it,
// ordered by locale.
func TestListLocales_DistinctAndCounted(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxTenant("t1")
	en := seedEntry(t, svc, owner) // default locale
	_, err := svc.CreateLocalizedEntry(owner, "order", CreateLocalizedInput{
		Payload: json.RawMessage(`{"title":"a"}`), Locale: "fr", TranslationOf: &en.ID,
	})
	require.NoError(t, err)
	_, err = svc.CreateLocalizedEntry(owner, "order", CreateLocalizedInput{
		Payload: json.RawMessage(`{"title":"b"}`), Locale: "fr",
	})
	require.NoError(t, err)

	rows, err := svc.ListLocales(owner, ListLocalesInput{})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "default", rows[0].Locale, "locale order must be ascending")
	assert.Equal(t, 1, rows[0].Entries)
	assert.Equal(t, "fr", rows[1].Locale)
	assert.Equal(t, 2, rows[1].Entries, "two independent 'fr' entries, one a translation and one standalone")
}

// TestListLocales_FiltersByType. `type` narrows the count to one content
// type — a tenant with locales spread unevenly across types must not have one
// type's language mix bleed into another's count.
func TestListLocales_FiltersByType(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")
	_, err := svc.CreateContentType(owner, orderTypeInput())
	require.NoError(t, err)

	_, err = svc.CreateEntry(owner, "post", json.RawMessage(`{"title":"a","body":""}`))
	require.NoError(t, err)
	_, err = svc.CreateLocalizedEntry(owner, "post", CreateLocalizedInput{
		Payload: json.RawMessage(`{"title":"b","body":""}`), Locale: "fr",
	})
	require.NoError(t, err)
	_, err = svc.CreateEntry(owner, "order", json.RawMessage(`{"title":"c"}`))
	require.NoError(t, err)

	all, err := svc.ListLocales(owner, ListLocalesInput{})
	require.NoError(t, err)
	require.Len(t, all, 2, "default and fr, across both types")

	postOnly, err := svc.ListLocales(owner, ListLocalesInput{Type: "post"})
	require.NoError(t, err)
	require.Len(t, postOnly, 2)
	orderOnly, err := svc.ListLocales(owner, ListLocalesInput{Type: "order"})
	require.NoError(t, err)
	require.Len(t, orderOnly, 1)
	assert.Equal(t, "default", orderOnly[0].Locale)
	assert.Equal(t, 1, orderOnly[0].Entries)
}

// TestListLocales_UnknownTypeRejected. A `type` naming no content type is a
// 404, the same shape GetContentType gives every other endpoint that resolves
// a type slug — not an empty, silently-narrowed result.
func TestListLocales_UnknownTypeRejected(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxTenant("t1")
	seedEntry(t, svc, owner)

	_, err := svc.ListLocales(owner, ListLocalesInput{Type: "no-such-type"})
	require.Error(t, err)
}

// TestListLocales_TenantScoped mirrors TestSearch_TenantScoped: the count is
// per tenant, with no parameter able to override it.
func TestListLocales_TenantScoped(t *testing.T) {
	svc, _ := newSvc()
	one := ctxTenant("t1")
	two := ctxTenant("t2")
	seedEntry(t, svc, one)

	rows, err := svc.ListLocales(two, ListLocalesInput{})
	require.NoError(t, err)
	assert.Empty(t, rows, "tenant two must not see tenant one's locales")
}

// TestListLocales_DeliveryCredentialRefused. Same second layer as Search: a
// public delivery (or preview) credential has no use for a language inventory
// spanning draft and published content, and this endpoint does not split the
// two the way ListEntries' Status does.
func TestListLocales_DeliveryCredentialRefused(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxTenant("t1")
	seedEntry(t, svc, owner)

	_, err := svc.ListLocales(ctxDelivery("t1"), ListLocalesInput{})
	require.Error(t, err)
	assert.Equal(t, "FORBIDDEN", codeOf(t, err))
}

// TestListLocales_PreviewCredentialRefused. A preview subject is a delivery
// subject with PreviewEntryID set (audienceFor, dto.go) — caught by the same
// PublicDelivery check as the plain delivery case above.
func TestListLocales_PreviewCredentialRefused(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxTenant("t1")
	e := seedEntry(t, svc, owner)

	_, err := svc.ListLocales(ctxPreview("t1", e.ID), ListLocalesInput{})
	require.Error(t, err)
	assert.Equal(t, "FORBIDDEN", codeOf(t, err))
}

// TestListLocales_NoEntriesReturnsEmptySlice, not nil — the handler marshals
// this straight into {"items": ...} and a nil slice would ship `null`.
func TestListLocales_NoEntriesReturnsEmptySlice(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")

	rows, err := svc.ListLocales(owner, ListLocalesInput{})
	require.NoError(t, err)
	require.NotNil(t, rows)
	assert.Empty(t, rows)
}

// TestListLocales_ViewerCanList, matching Search's own deviation from
// ListPendingReview: ActionContentList includes the viewer role.
func TestListLocales_ViewerCanList(t *testing.T) {
	svc, _ := newSvc()
	seedEntry(t, svc, ctxTenant("t1"))

	viewer := ctxRole("t1", "viewer")
	rows, err := svc.ListLocales(viewer, ListLocalesInput{})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}
