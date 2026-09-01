package service

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Client-declared media metadata at the SERVICE layer.
//
// Division of labour, because this file could easily be written to lie: the
// in-memory repo cannot enforce a CHECK constraint, so nothing here asserts what
// the database will accept. What it asserts is REFUSALS, which fire before any
// repository call and are therefore independent of the fake's fidelity — plus
// pure decode/patch behaviour, which touches no repository at all. Every limit
// is proved against a real Postgres in
// repository/media_metadata_integration_test.go.
//
// The refusal tests deliberately target an id that does NOT exist. If a
// validation ever moved after the repository call the assertion would come back
// ErrNotFound instead of the 422, so "rejected before we touch storage" is
// something these tests measure rather than assume.

func mustDecodePatch(t *testing.T, body string) UpdateMediaAssetInput {
	t.Helper()
	var in UpdateMediaAssetInput
	require.NoError(t, json.Unmarshal([]byte(body), &in))
	return in
}

// The three states a JSON key can be in, and the reason Optional exists: a plain
// *string cannot tell the first two apart, and for a tri-state column that
// difference decides whether a PATCH clears a value nobody asked it to clear.
func TestOptional_DistinguishesAbsentFromExplicitNull(t *testing.T) {
	absent := mustDecodePatch(t, `{"filename":"a.png"}`)
	require.False(t, absent.AltText.Set, "a key that was never sent must not be Set")

	null := mustDecodePatch(t, `{"alt_text":null}`)
	require.True(t, null.AltText.Set, "an explicit null IS a instruction; encoding/json still calls UnmarshalJSON for it")
	require.Nil(t, null.AltText.Value)

	empty := mustDecodePatch(t, `{"alt_text":""}`)
	require.True(t, empty.AltText.Set)
	require.NotNil(t, empty.AltText.Value, `"" is a value, not an absence — it means "decorative"`)
	require.Equal(t, "", *empty.AltText.Value)
}

// The patch a repository receives must name only the fields the caller sent.
// This is the property that stops an alt-text correction from blanking a
// filename, and it is checkable without any repository at all.
func TestMediaMetadata_PatchNamesOnlyTheFieldsThatWereSent(t *testing.T) {
	p, err := mustDecodePatch(t, `{"alt_text":"a duck"}`).toPatch()
	require.NoError(t, err)
	require.True(t, p.SetAltText)
	require.False(t, p.SetFilename, "a filename that was never mentioned must not be written")
	require.False(t, p.SetDimensions)

	// An explicit null is an instruction to clear, and must reach the repository
	// as "set this column, to NULL" rather than as "leave it alone".
	p, err = mustDecodePatch(t, `{"filename":null}`).toPatch()
	require.NoError(t, err)
	require.True(t, p.SetFilename, "an explicit null must still be a write")
	require.Nil(t, p.Filename)

	// Empty alt text survives as a value, not as an absence.
	p, err = mustDecodePatch(t, `{"alt_text":""}`).toPatch()
	require.NoError(t, err)
	require.True(t, p.SetAltText)
	require.NotNil(t, p.AltText)
	require.Equal(t, "", *p.AltText, `"" must reach the column; it is how an editor says "decorative"`)

	// Both dimensions nulled together is a legal reset.
	p, err = mustDecodePatch(t, `{"width_px":null,"height_px":null}`).toPatch()
	require.NoError(t, err)
	require.True(t, p.SetDimensions)
	require.Nil(t, p.WidthPx)
	require.Nil(t, p.HeightPx)
}

// A patch naming nothing is a legitimate request from a client that diffed its
// state and found no change. It must not be an error, and it must not be turned
// into a write of four NULLs.
func TestMediaMetadata_EmptyPatchWritesNothing(t *testing.T) {
	p, err := mustDecodePatch(t, `{}`).toPatch()
	require.NoError(t, err)
	require.True(t, p.IsEmpty())
}

func TestMediaMetadata_RefusalsHappenBeforeAnyRepositoryCall(t *testing.T) {
	svc, repo := newSvc()
	ctx := ctxTenant("t1")
	// Nothing with this id exists. A refusal that reached the repository would
	// come back as 404, so a 422 here proves the validation ran first.
	ghost := uuid.New()

	cases := []struct {
		name string
		body string
		want *apperrors.AppError
	}{
		{"empty filename is not a filename; null is how you clear it",
			`{"filename":""}`, ErrMediaFilenameInvalid},
		{"a forward slash makes a declared name read as a path",
			`{"filename":"../../etc/passwd"}`, ErrMediaFilenameInvalid},
		{"a backslash is the same hazard on the other platform",
			`{"filename":"dir\\file.png"}`, ErrMediaFilenameInvalid},
		{"a newline is header injection the day this reaches Content-Disposition",
			"{\"filename\":\"a\\nb.png\"}", ErrMediaFilenameInvalid},
		{"a filename one character over the column limit",
			`{"filename":"` + strings.Repeat("a", domain.MaxFilenameLen+1) + `"}`, ErrMediaFilenameInvalid},
		{"alt text one character over the column limit",
			`{"alt_text":"` + strings.Repeat("a", domain.MaxAltTextLen+1) + `"}`, ErrMediaAltTextTooLong},
		{"a width with no height reserves no layout space",
			`{"width_px":800}`, ErrMediaDimensionsIncomplete},
		{"a height with no width, symmetrically",
			`{"height_px":600}`, ErrMediaDimensionsIncomplete},
		{"clearing one dimension while setting the other",
			`{"width_px":800,"height_px":null}`, ErrMediaDimensionsIncomplete},
		{"zero is not a picture",
			`{"width_px":0,"height_px":600}`, ErrMediaDimensionOutOfRange},
		{"a dimension past the ceiling",
			`{"width_px":800,"height_px":` + strconv.Itoa(domain.MaxImageDimension+1) + `}`, ErrMediaDimensionOutOfRange},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.UpdateMediaAsset(ctx, ghost, mustDecodePatch(t, tc.body))
			require.ErrorIs(t, err, tc.want)
			require.NotErrorIs(t, err, apperrors.ErrNotFound,
				"a 404 here means the validation ran AFTER the repository lookup")
			require.Empty(t, repo.assets, "a refused patch must not create anything")
		})
	}
}

// The reservation endpoint must reject exactly what the patch endpoint rejects.
// Two validators for one set of columns is how a create surface and a patch
// surface come to disagree about what a legal filename is.
func TestMediaMetadata_ReservationSharesTheSameValidator(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")

	_, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{
		ContentType:        "image/png",
		MediaMetadataInput: mustDecodePatch(t, `{"filename":"a/b.png"}`).MediaMetadataInput,
	})
	require.ErrorIs(t, err, ErrMediaFilenameInvalid)
	require.Empty(t, repo.assets, "a refused reservation must not leave a row behind")
	require.Empty(t, store.signedPost, "nothing may be signed for a refused reservation")

	_, err = svc.CreateMediaUpload(ctx, CreateMediaUploadInput{
		ContentType:        "image/png",
		MediaMetadataInput: mustDecodePatch(t, `{"width_px":800}`).MediaMetadataInput,
	})
	require.ErrorIs(t, err, ErrMediaDimensionsIncomplete)
	require.Empty(t, repo.assets)
}

// A delivery credential is read-only, and this is the chokepoint that says so.
func TestMediaMetadata_DeliveryCannotPatch(t *testing.T) {
	svc, repo, store := newMediaSvc()
	admin := ctxTenant("t1")
	asset := uploadAsset(t, svc, repo, store, admin)

	_, err := svc.UpdateMediaAsset(ctxDelivery("t1"), asset, mustDecodePatch(t, `{"alt_text":"x"}`))
	require.ErrorIs(t, err, apperrors.ErrForbidden, "delivery credentials are read-only")
}

// deliveryMediaKeys is the EXACT key set a delivery credential received from
// GET /media/{id} before migration 000022 existed. It is written out rather than
// derived so that widening the public surface has to be a deliberate edit to
// this list — organon OD-007 forbids new delivery-surface work, and a column
// added upstream must not be able to reach the public by inheritance.
var deliveryMediaKeys = []string{"id", "content_type", "size_bytes", "uploaded", "uploaded_at", "created_at"}

func TestMedia_DeliveryDTOKeySetIsFrozen(t *testing.T) {
	name, alt := "duck.png", ""
	w, h := 800, 600
	now := time.Now().UTC()
	a := &domain.MediaAsset{
		ID: uuid.New(), TenantID: "t1", StorageKey: "t1/k",
		ContentType: "image/png", SizeBytes: 4242,
		UploadedAt: &now, CreatedAt: now,
		Filename: &name, AltText: &alt, WidthPx: &w, HeightPx: &h,
	}

	// The admin view FIRST. Without this the delivery assertion below would pass
	// against a DTO that simply never carries the fields for anyone, which is a
	// vacuous green rather than a proof of audience separation.
	adminKeys := jsonKeys(t, ProjectMediaAsset(a, adminSubject()))
	for _, k := range []string{"filename", "alt_text", "width_px", "height_px"} {
		require.Contains(t, adminKeys, k, "the admin view must expose %s, or the delivery test proves nothing", k)
	}
	// Specifically: alt_text = "" is a VALUE and has to survive to the wire, or
	// the tri-state collapses at the JSON boundary having been preserved
	// everywhere else.
	var adminBody map[string]any
	require.NoError(t, json.Unmarshal(mustMarshal(t, ProjectMediaAsset(a, adminSubject())), &adminBody))
	require.Equal(t, "", adminBody["alt_text"], `an editor's "this is decorative" must not vanish as an empty value`)

	require.ElementsMatch(t, deliveryMediaKeys, jsonKeys(t, ProjectMediaAsset(a, deliverySubject())),
		"the delivery media response is frozen (OD-007): adding a key here is a governance decision, not a refactor")
}

// The same separation through the real read path, so the audience argument is
// proved where it is actually chosen and not only in the DTO helper.
func TestMedia_DeliveryGetOmitsDeclaredMetadata(t *testing.T) {
	svc, repo, store := newMediaSvc()
	admin := ctxTenant("t1")
	require.NoError(t, seedFileType(t, svc, admin))
	asset := uploadAsset(t, svc, repo, store, admin)

	_, err := svc.UpdateMediaAsset(admin, asset, mustDecodePatch(t, `{"filename":"duck.png","alt_text":"a duck","width_px":800,"height_px":600}`))
	require.NoError(t, err)

	e, err := svc.CreateEntry(admin, "doc", mustJSON(t, map[string]any{"title": "T", "cover": asset.String()}))
	require.NoError(t, err)
	_, err = svc.SetEntryStatus(admin, "doc", e.ID, domain.StatusPublished, 0)
	require.NoError(t, err)

	// Admin first: the field is genuinely there to be leaked.
	got, err := svc.GetMediaAsset(admin, asset)
	require.NoError(t, err)
	require.NotNil(t, got.Filename)
	require.Equal(t, "duck.png", *got.Filename)
	require.NotNil(t, got.AltText)
	require.NotNil(t, got.WidthPx)

	// The delivery credential can reach this asset — the entry referencing it is
	// published — so a nil here is the audience split, not the publication gate.
	pub, err := svc.GetMediaAsset(ctxDelivery("t1"), asset)
	require.NoError(t, err, "a published asset must still be readable, or this test proves the wrong thing")
	require.Nil(t, pub.Filename)
	require.Nil(t, pub.AltText)
	require.Nil(t, pub.WidthPx)
	require.Nil(t, pub.HeightPx)
}

func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(mustMarshal(t, v), &m))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
