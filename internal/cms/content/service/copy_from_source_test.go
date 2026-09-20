package service

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// copy_from_source (T2 2.3) seeds a new translation from its source entry's
// working copy, with the body payload's fields overlaid on top — see
// CreateLocalizedInput.CopyFromSource for the exact semantics and why an
// empty body payload alone (no flag) must NOT trigger this.

// TestLocale_CopyFromSourceOverridesFields is the override semantics: a field
// the caller sent wins, a field the caller left out is carried over from the
// source, and both survive the same validation an ordinary create runs.
func TestLocale_CopyFromSourceOverridesFields(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)
	en, err := svc.CreateEntry(ctx, "order",
		mustJSON(t, map[string]any{"title": "Full Title", "amount": 42.0, "state": "new"}))
	require.NoError(t, err)

	zh, err := svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload:        mustJSON(t, map[string]any{"title": "翻譯標題"}),
		Locale:         "zh-TW",
		TranslationOf:  &en.ID,
		CopyFromSource: true,
	})
	require.NoError(t, err)

	var data map[string]any
	require.NoError(t, json.Unmarshal(zh.Data, &data))
	assert.Equal(t, "翻譯標題", data["title"], "the body payload's field must override the source's")
	assert.Equal(t, 42.0, data["amount"], "a field absent from the body payload must be copied from the source")
	assert.Equal(t, "new", data["state"], "a field absent from the body payload must be copied from the source")
}

// TestLocale_CopyFromSourceExplicitNullClearsField is merge-PATCH semantics
// (RFC 7386), not "absent means inherit, present means override": a key the
// body OMITS is carried over from the source (proven above), but a key the
// body sends with an explicit JSON `null` must be CLEARED, not silently kept
// at the source's value — `mergeShallowJSON` overlays overlay's keys onto the
// base as-is, `null` included, and the unmarshalled Go value for a JSON
// `null` is `nil`, indistinguishable from "never set" to validatePayload
// (validate.go: `!present || v == nil`) — which is exactly why this needs its
// own test: a merge that special-cased `null` to mean "skip, keep base" would
// make the two indistinguishable from a caller's own JSON, and there would be
// no way to ever unset a field while copying the rest.
func TestLocale_CopyFromSourceExplicitNullClearsField(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)
	en, err := svc.CreateEntry(ctx, "order",
		mustJSON(t, map[string]any{"title": "Full Title", "amount": 42.0, "state": "new"}))
	require.NoError(t, err)

	zh, err := svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload:        json.RawMessage(`{"title":"翻譯標題","amount":null}`),
		Locale:         "zh-TW",
		TranslationOf:  &en.ID,
		CopyFromSource: true,
	})
	require.NoError(t, err)

	var data map[string]any
	require.NoError(t, json.Unmarshal(zh.Data, &data))
	assert.Equal(t, "翻譯標題", data["title"], "an ordinary override still works alongside an explicit null")
	amount, present := data["amount"]
	assert.True(t, present, "an explicitly-nulled key must still be present in the result, not dropped")
	assert.Nil(t, amount, "amount:null in the body must clear the source's 42.0, not keep it")
	assert.Equal(t, "new", data["state"], "a key the body never mentioned at all is still inherited from the source")
}

// TestLocale_CopyFromSourceEmptyBodyCopiesWhole. An empty body payload with
// the flag set is "copy everything" — the base with nothing to override.
func TestLocale_CopyFromSourceEmptyBodyCopiesWhole(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)
	en, err := svc.CreateEntry(ctx, "order",
		mustJSON(t, map[string]any{"title": "Full Title", "amount": 42.0, "state": "new"}))
	require.NoError(t, err)

	zh, err := svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload:        json.RawMessage(`{}`),
		Locale:         "zh-TW",
		TranslationOf:  &en.ID,
		CopyFromSource: true,
	})
	require.NoError(t, err)

	var data map[string]any
	require.NoError(t, json.Unmarshal(zh.Data, &data))
	assert.Equal(t, "Full Title", data["title"])
	assert.Equal(t, 42.0, data["amount"])
	assert.Equal(t, "new", data["state"])
}

// TestLocale_CopyFromSourceWithoutFlagDoesNotCopy is the "no implicit
// behaviour" half: an empty body payload WITHOUT copy_from_source must fail
// exactly as an ordinary create with an empty payload does (missing the
// required "title"), never silently duplicate the source.
func TestLocale_CopyFromSourceWithoutFlagDoesNotCopy(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	en := seedEntry(t, svc, ctx)

	_, err := svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload:       json.RawMessage(`{}`),
		Locale:        "zh-TW",
		TranslationOf: &en.ID,
		// CopyFromSource left false.
	})
	require.Error(t, err)
	assert.Equal(t, "CONTENT_FIELD_REQUIRED", codeOf(t, err), "an empty payload must fail validation, not silently copy the source")
}

// TestLocale_CopyFromSourceSourceNotFound. copy_from_source names the same
// translation_of id GetEntry already resolves — an id that does not exist in
// this tenant/type is refused the same 404, whether or not the flag is set.
func TestLocale_CopyFromSourceSourceNotFound(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	seedEntry(t, svc, ctx)

	missing := uuid.New()
	_, err := svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload:        json.RawMessage(`{"title":"x"}`),
		Locale:         "fr",
		TranslationOf:  &missing,
		CopyFromSource: true,
	})
	require.Error(t, err)
	assert.Equal(t, "NOT_FOUND", codeOf(t, err))
}

// TestLocale_CopyFromSourceForeignTenantRejected. Naming another tenant's
// entry as the copy source must not leak its payload — refused the same way
// TestLocale_TranslationOfForeignEntryRejected refuses the plain case.
func TestLocale_CopyFromSourceForeignTenantRejected(t *testing.T) {
	svc, _ := newSvc()
	other := seedEntry(t, svc, ctxTenant("t2"))

	_, err := svc.CreateContentType(ctxTenant("t1"), orderTypeInput())
	require.NoError(t, err)
	_, err = svc.CreateLocalizedEntry(ctxTenant("t1"), "order", CreateLocalizedInput{
		Payload:        json.RawMessage(`{"title":"a"}`),
		Locale:         "en",
		TranslationOf:  &other.ID,
		CopyFromSource: true,
	})
	require.Error(t, err, "copy_from_source must not resolve a source across tenants")
	assert.Equal(t, "NOT_FOUND", codeOf(t, err))
}

// TestLocale_CopyFromSourceRequiresTranslationOf. There is no source to copy
// from without translation_of — refused as a caller error, not silently
// ignored.
func TestLocale_CopyFromSourceRequiresTranslationOf(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	if _, err := svc.CreateContentType(ctx, orderTypeInput()); err != nil {
		t.Fatalf("create type: %v", err)
	}

	_, err := svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload:        json.RawMessage(`{"title":"a"}`),
		CopyFromSource: true,
	})
	require.Error(t, err)
	assert.Equal(t, "CONTENT_COPY_FROM_SOURCE_REQUIRES_TRANSLATION_OF", codeOf(t, err))
}
