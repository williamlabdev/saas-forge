package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	"github.com/williamlabdev/saas-forge/internal/pkg/objectstore"
)

// fakeStore records what was signed and lets a test pretend an upload landed.
//
// It deliberately does NOT enforce the upload constraints it is handed: a real
// server would, and a fake that also did would hide whether the service checks
// anything itself. Tests assert on signedWith instead.
type fakeStore struct {
	objects    map[string]objectstore.ObjectInfo
	blobs      map[string][]byte
	signedPost []objectstore.UploadConstraints
	signedGet  []string
	deleted    []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string]objectstore.ObjectInfo{}, blobs: map[string][]byte{}}
}

func (f *fakeStore) PresignPost(_ context.Context, key string, _ time.Duration, c objectstore.UploadConstraints) (objectstore.PresignedUpload, error) {
	f.signedPost = append(f.signedPost, c)
	return objectstore.PresignedUpload{
		URL:    "https://storage.example/" + key,
		Fields: map[string]string{"key": key, "Content-Type": c.ContentType},
	}, nil
}

func (f *fakeStore) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	f.signedGet = append(f.signedGet, key)
	return "https://storage.example/get/" + key, nil
}

func (f *fakeStore) Stat(_ context.Context, key string) (objectstore.ObjectInfo, error) {
	info, ok := f.objects[key]
	if !ok {
		return objectstore.ObjectInfo{}, objectstore.ErrNotFound
	}
	return info, nil
}

func (f *fakeStore) Delete(_ context.Context, key string) error {
	f.deleted = append(f.deleted, key)
	delete(f.objects, key)
	delete(f.blobs, key)
	return nil
}

// Get and Put exist on the interface for the transform worker (ADR-019).
// Get is additionally called by CompleteMediaUpload for exactly one sniff
// window (512 bytes): a request handler moving bytes for delivery is what
// ADR-005 forbids, and the sniff read is the documented exception — bounded,
// read-only, and gated behind sniffEnforced. Put must still never be called
// on the request path.
func (f *fakeStore) Get(_ context.Context, key string) (io.ReadCloser, objectstore.ObjectInfo, error) {
	blob, ok := f.blobs[key]
	if !ok {
		return nil, objectstore.ObjectInfo{}, errors.New("fakeStore: no blob landed for " + key)
	}
	return io.NopCloser(bytes.NewReader(blob)), f.objects[key], nil
}

func (f *fakeStore) Put(context.Context, string, io.Reader, int64, string) error {
	return errors.New("fakeStore: Put called on the request path")
}

// landBytes simulates the client's direct-to-storage PUT succeeding, with
// honest bytes: the blob is synthesized from the landed content type, so it
// sniffs as what Stat claims. For lying bytes (spoofing tests), use landBody.
func (f *fakeStore) landBytes(t *testing.T, repo *memRepo, assetID uuid.UUID, size int64, ct string) {
	t.Helper()
	f.landBody(t, repo, assetID, size, ct, cannedBlob(ct))
}

// landBody simulates a PUT whose bytes need not match the stored content
// type — the spoofing shape: Stat echoes the declaration, the blob is the
// fact. Pass nil body for an empty (0-byte) object.
func (f *fakeStore) landBody(t *testing.T, repo *memRepo, assetID uuid.UUID, size int64, ct string, body []byte) {
	t.Helper()
	for _, a := range repo.assets {
		if a.ID == assetID {
			f.objects[a.StorageKey] = objectstore.ObjectInfo{Size: size, ContentType: ct}
			f.blobs[a.StorageKey] = body
			return
		}
	}
	t.Fatalf("asset %s not found", assetID)
}

// cannedBlob returns minimal magic-prefixed bytes that sniff as ct. Only the
// signature matters — the sniff window never decodes.
func cannedBlob(ct string) []byte {
	switch ct {
	case "image/png":
		return append(append([]byte{}, headPNG...), make([]byte, 64)...)
	case "image/jpeg":
		return append(append([]byte{}, headJPEG...), make([]byte, 64)...)
	case "image/gif":
		return append(append([]byte{}, headGIF...), make([]byte, 64)...)
	case "image/webp":
		return append(append([]byte{}, headWebP...), make([]byte, 64)...)
	case "image/avif":
		return append(append([]byte{}, headAVIF...), make([]byte, 64)...)
	case "application/pdf":
		return append(append([]byte{}, headPDF...), make([]byte, 64)...)
	case "video/mp4":
		return append([]byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'm', 'p', '4', '2'}, make([]byte, 64)...)
	case "text/html":
		return append([]byte{}, headHTML...)
	default:
		return []byte{}
	}
}

func newMediaSvc() (ContentService, *memRepo, *fakeStore) {
	repo := &memRepo{}
	store := newFakeStore()
	svc := NewContentServiceWithDelivery(repo, authz.NewAllowAllAuthorizer(), staticPlan(Quota{}), NewDeliveryCounter())
	return WithMediaStore(svc, store), repo, store
}

// uploadAsset runs the full reserve → land → complete cycle.
func uploadAsset(t *testing.T, svc ContentService, repo *memRepo, store *fakeStore, ctx context.Context) uuid.UUID {
	t.Helper()
	up, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "image/png"})
	require.NoError(t, err)
	require.NotEmpty(t, up.UploadURL)
	store.landBytes(t, repo, up.AssetID, 1234, "image/png")
	done, err := svc.CompleteMediaUpload(ctx, up.AssetID)
	require.NoError(t, err)
	require.True(t, done.Uploaded)
	return up.AssetID
}

// A reservation is not a file: completing without an object must fail, and the
// asset must stay unusable.
func TestMedia_CompleteWithoutUploadIsRejected(t *testing.T) {
	svc, _, _ := newMediaSvc()
	ctx := ctxTenant("t1")

	up, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "image/png"})
	require.NoError(t, err)

	_, err = svc.CompleteMediaUpload(ctx, up.AssetID)
	require.Error(t, err, "no bytes landed — completing must fail")

	got, err := svc.GetMediaAsset(ctx, up.AssetID)
	require.NoError(t, err)
	require.False(t, got.Uploaded)
}

// Size comes from storage, never from the caller's claim. Type must now ALSO
// match the declaration (strict, 2026-09-14): this test previously landed jpeg
// bytes under a png reservation and expected success — that shape is now
// ErrMediaBytesMismatch (see TestMedia_CrossFormatIsRejected below).
func TestMedia_MetadataComesFromStorage(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")

	up, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "image/jpeg"})
	require.NoError(t, err)
	// What actually landed is a large jpeg; the client declared jpeg too.
	store.landBytes(t, repo, up.AssetID, 9_000_000, "image/jpeg")

	done, err := svc.CompleteMediaUpload(ctx, up.AssetID)
	require.NoError(t, err)
	require.EqualValues(t, 9_000_000, done.SizeBytes)
	require.Equal(t, "image/jpeg", done.ContentType, "storage is the source of truth, not the client")

	stored, err := svc.GetMediaAsset(ctx, up.AssetID)
	require.NoError(t, err)
	require.EqualValues(t, 9_000_000, stored.SizeBytes, "the PERSISTED size must come from storage")
	require.Equal(t, "image/jpeg", stored.ContentType, "the PERSISTED content type must come from storage")
}

// The spoofing shape: declared png, Stat echoing png, but HTML bytes.
// Must be refused with the mismatch code and discarded, not stored.
func TestMedia_SpoofedImageIsRejectedAndDiscarded(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")

	up, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "image/png"})
	require.NoError(t, err)
	store.landBody(t, repo, up.AssetID, 1234, "image/png", headHTML)

	_, err = svc.CompleteMediaUpload(ctx, up.AssetID)
	require.ErrorIs(t, err, ErrMediaBytesMismatch)

	require.Len(t, store.deleted, 1, "the spoofed object must be removed, not left as orphan storage")
	_, err = svc.GetMediaAsset(ctx, up.AssetID)
	require.Error(t, err, "the row must go too, or completion can be retried forever")
}

// Same family is no excuse: png-declared jpeg bytes are refused (strict).
func TestMedia_CrossFormatIsRejected(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")

	up, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "image/png"})
	require.NoError(t, err)
	store.landBody(t, repo, up.AssetID, 1234, "image/png", cannedBlob("image/jpeg"))

	_, err = svc.CompleteMediaUpload(ctx, up.AssetID)
	require.ErrorIs(t, err, ErrMediaBytesMismatch)
	require.Len(t, store.deleted, 1)
}

// A 0-byte object has no signature, so it can never match an image claim.
func TestMedia_EmptyObjectIsRejected(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")

	up, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "image/png"})
	require.NoError(t, err)
	store.landBody(t, repo, up.AssetID, 0, "image/png", nil)

	_, err = svc.CompleteMediaUpload(ctx, up.AssetID)
	require.ErrorIs(t, err, ErrMediaBytesMismatch)
}

// Non-images keep declared-only checks (spec P2): a real PDF completes, and
// even lying bytes under a pdf declaration are not this feature's problem.
func TestMedia_PdfKeepsDeclaredBehavior(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")

	up, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "application/pdf"})
	require.NoError(t, err)
	store.landBody(t, repo, up.AssetID, 999, "application/pdf", cannedBlob("application/pdf"))

	done, err := svc.CompleteMediaUpload(ctx, up.AssetID)
	require.NoError(t, err)
	require.True(t, done.Uploaded)
}

// AVIF is whitelisted but opaque to the stdlib sniffer: enforcing byte
// equality would refuse every legitimate AVIF, so it keeps declared-only
// checks (research D4, probed TestSniff_AVIFProbe).
func TestMedia_AvifSkipsSniff(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")

	up, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "image/avif"})
	require.NoError(t, err)
	store.landBody(t, repo, up.AssetID, 999, "image/avif", cannedBlob("image/avif"))

	done, err := svc.CompleteMediaUpload(ctx, up.AssetID)
	require.NoError(t, err)
	require.True(t, done.Uploaded)
}

// The limits must travel WITH the signature. If they were only checked on
// completion the bytes would already be stored, and an upload that is never
// completed would never be checked at all.
func TestMedia_UploadIsSignedWithLimits(t *testing.T) {
	svc, _, store := newMediaSvc()
	ctx := ctxTenant("t1")

	up, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "image/png"})
	require.NoError(t, err)

	require.Len(t, store.signedPost, 1)
	require.EqualValues(t, MaxUploadBytes, store.signedPost[0].MaxBytes,
		"the signed form must carry the size limit; storage is what enforces it")
	require.Equal(t, "image/png", store.signedPost[0].ContentType,
		"the signed form must pin the content type")
	require.EqualValues(t, MaxUploadBytes, up.MaxBytes, "the client is told the limit up front")
	require.NotEmpty(t, up.Fields, "a POST policy is useless without its form fields")
}

// A type outside the whitelist is refused before anything is reserved.
func TestMedia_RejectsDisallowedDeclaredType(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")

	_, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "application/x-msdownload"})
	require.ErrorIs(t, err, ErrMediaTypeNotAllowed)
	require.Empty(t, store.signedPost, "nothing may be signed for a refused type")
	require.Empty(t, repo.assets, "a refused type must not leave a reservation row behind")
}

// SVG is excluded on purpose — it is a scriptable document, not an inert image.
func TestMedia_RejectsSVG(t *testing.T) {
	svc, _, _ := newMediaSvc()
	_, err := svc.CreateMediaUpload(ctxTenant("t1"), CreateMediaUploadInput{ContentType: "image/svg+xml"})
	require.ErrorIs(t, err, ErrMediaTypeNotAllowed, "svg can carry script; it is not whitelisted")
}

// Parameters and casing must not be a way past the whitelist.
func TestMedia_ContentTypeIsNormalized(t *testing.T) {
	svc, _, store := newMediaSvc()
	ctx := ctxTenant("t1")

	up, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "  IMAGE/PNG; charset=utf-8 "})
	require.NoError(t, err)
	require.Equal(t, "image/png", store.signedPost[0].ContentType,
		"the signed policy must pin the normalized type")

	stored, err := svc.GetMediaAsset(ctx, up.AssetID)
	require.NoError(t, err)
	require.Equal(t, "image/png", stored.ContentType, "the PERSISTED type must be normalized")
}

// An object over the limit must not become referenceable, and must not be left
// sitting in the bucket either.
func TestMedia_OversizedUploadIsRejectedAndDiscarded(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")

	up, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "image/png"})
	require.NoError(t, err)
	// The signed policy should have stopped this; assume a server that did not.
	store.landBytes(t, repo, up.AssetID, MaxUploadBytes+1, "image/png")

	_, err = svc.CompleteMediaUpload(ctx, up.AssetID)
	require.ErrorIs(t, err, ErrMediaTooLarge)

	require.Len(t, store.deleted, 1, "the oversized object must be removed, not left as orphan storage")
	_, err = svc.GetMediaAsset(ctx, up.AssetID)
	require.Error(t, err, "the row must go too, or completion can be retried forever")
}

// What actually landed is what counts: a whitelisted declaration must not let a
// non-whitelisted object through.
func TestMedia_DisallowedLandedTypeIsRejectedAndDiscarded(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")

	up, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "image/png"})
	require.NoError(t, err)
	store.landBytes(t, repo, up.AssetID, 1234, "text/html")

	_, err = svc.CompleteMediaUpload(ctx, up.AssetID)
	require.ErrorIs(t, err, ErrMediaTypeNotAllowed,
		"the declared type is a claim; the stored object is the fact")

	require.Len(t, store.deleted, 1)
	_, err = svc.GetMediaAsset(ctx, up.AssetID)
	require.Error(t, err)
}

// The boundary itself: exactly at the limit is allowed.
func TestMedia_ExactlyAtLimitIsAccepted(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")

	up, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "image/png"})
	require.NoError(t, err)
	store.landBytes(t, repo, up.AssetID, MaxUploadBytes, "image/png")

	done, err := svc.CompleteMediaUpload(ctx, up.AssetID)
	require.NoError(t, err, "the limit is inclusive")
	require.True(t, done.Uploaded)
}

// A reservation has no bytes, so there is nothing to sign — for anyone.
func TestMedia_ResolveRejectsUnuploadedAsset(t *testing.T) {
	svc, _, store := newMediaSvc()
	ctx := ctxTenant("t1")

	up, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "image/png"})
	require.NoError(t, err)

	_, _, err = svc.ResolveMediaURL(ctx, up.AssetID, "")
	require.Error(t, err, "an asset with no uploaded bytes must not be signed")
	require.Empty(t, store.signedGet, "nothing may be signed for a reservation")
}

// An entry may only reference an asset whose bytes landed.
func TestMedia_EntryCannotReferenceUnuploadedAsset(t *testing.T) {
	svc, _, _ := newMediaSvc()
	ctx := ctxTenant("t1")
	require.NoError(t, seedFileType(t, svc, ctx))

	up, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "image/png"})
	require.NoError(t, err)

	_, err = svc.CreateEntry(ctx, "doc", mustJSON(t, map[string]any{"title": "T", "cover": up.AssetID.String()}))
	require.Error(t, err, "a reservation is not a file")
}

func TestMedia_EntryCannotReferenceForeignAsset(t *testing.T) {
	svc, repo, store := newMediaSvc()
	other := ctxTenant("t2")
	require.NoError(t, seedFileType(t, svc, other))
	foreign := uploadAsset(t, svc, repo, store, other)

	mine := ctxTenant("t1")
	require.NoError(t, seedFileType(t, svc, mine))
	_, err := svc.CreateEntry(mine, "doc", mustJSON(t, map[string]any{"title": "T", "cover": foreign.String()}))
	require.Error(t, err, "an asset from another tenant must not resolve")
}

// The core gate: bytes are readable through a delivery credential only while a
// published entry points at them.
func TestMedia_DeliveryOnlyWhenReferencedByPublished(t *testing.T) {
	svc, repo, store := newMediaSvc()
	admin := ctxTenant("t1")
	require.NoError(t, seedFileType(t, svc, admin))
	asset := uploadAsset(t, svc, repo, store, admin)

	e, err := svc.CreateEntry(admin, "doc", mustJSON(t, map[string]any{"title": "T", "cover": asset.String()}))
	require.NoError(t, err)

	del := ctxDelivery("t1")

	// Draft entry → the bytes are not public.
	_, _, err = svc.ResolveMediaURL(del, asset, "")
	require.Error(t, err, "media of an unpublished entry must not be served")

	// The admin can still preview it.
	url, _, err := svc.ResolveMediaURL(admin, asset, "")
	require.NoError(t, err)
	require.NotEmpty(t, url)

	// Publish → now it resolves for delivery too.
	_, err = svc.SetEntryStatus(admin, "doc", e.ID, domain.StatusPublished, 0)
	require.NoError(t, err)
	url, expires, err := svc.ResolveMediaURL(del, asset, "")
	require.NoError(t, err)
	require.NotEmpty(t, url)
	require.True(t, expires.After(time.Now().UTC()), "a signed URL must carry an expiry")

	// Unpublish → access is withdrawn again.
	_, err = svc.SetEntryStatus(admin, "doc", e.ID, domain.StatusDraft, 0)
	require.NoError(t, err)
	_, _, err = svc.ResolveMediaURL(del, asset, "")
	require.Error(t, err, "unpublishing must withdraw access to the bytes")
}

// Dropping the reference withdraws public access at PUBLISH time, not at save.
// The draft losing an image says nothing about what the public is being served:
// delivery is still rendering the published snapshot, which still references the
// asset. Revoking on save would break a live page on an unreleased edit
// (ADR-006). Withdrawal is correct only once the removal is actually published.
func TestMedia_UnreferencingWithdrawsAccessOnPublish(t *testing.T) {
	svc, repo, store := newMediaSvc()
	admin := ctxTenant("t1")
	require.NoError(t, seedFileType(t, svc, admin))
	asset := uploadAsset(t, svc, repo, store, admin)

	e, err := svc.CreateEntry(admin, "doc", mustJSON(t, map[string]any{"title": "T", "cover": asset.String()}))
	require.NoError(t, err)
	_, err = svc.SetEntryStatus(admin, "doc", e.ID, domain.StatusPublished, 0)
	require.NoError(t, err)

	_, _, err = svc.ResolveMediaURL(ctxDelivery("t1"), asset, "")
	require.NoError(t, err)

	// Clear the file field in the working copy only.
	_, err = svc.UpdateEntry(admin, "doc", e.ID, json.RawMessage(`{"cover":""}`), 0)
	require.NoError(t, err)

	_, _, err = svc.ResolveMediaURL(ctxDelivery("t1"), asset, "")
	require.NoError(t, err, "the published snapshot still references the asset, so the live page must keep working")

	// Publishing the removal is what actually retires the asset.
	_, err = svc.SetEntryStatus(admin, "doc", e.ID, domain.StatusPublished, 0)
	require.NoError(t, err)

	_, _, err = svc.ResolveMediaURL(ctxDelivery("t1"), asset, "")
	require.Error(t, err, "once the removal is published, nothing published references the asset")
}

func TestMedia_DeleteRemovesBytes(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")
	require.NoError(t, seedFileType(t, svc, ctx))
	asset := uploadAsset(t, svc, repo, store, ctx)

	require.NoError(t, svc.DeleteMediaAsset(ctx, asset, false))
	require.Len(t, store.deleted, 1, "the stored object must be removed, not just the row")
	_, err := svc.GetMediaAsset(ctx, asset)
	require.Error(t, err)
}

// Without a store the endpoints answer 501 rather than failing obscurely.
func TestMedia_DisabledWithoutStore(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	_, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "image/png"})
	require.ErrorIs(t, err, ErrMediaDisabled)
}

// A delivery credential must not be able to reserve uploads.
func TestMedia_DeliveryCannotUpload(t *testing.T) {
	svc, _, _ := newMediaSvc()
	_, err := svc.CreateMediaUpload(ctxDelivery("t1"), CreateMediaUploadInput{ContentType: "image/png"})
	require.Error(t, err, "delivery credentials are read-only")
}

// The admin media library: a reservation with no bytes must not appear, kind
// and q must narrow the same way the repository does, total must count the
// whole filtered set rather than just the page returned, an out-of-range
// limit must clamp rather than error, and a list item must carry no variants
// — the console fetches those per-asset through GetMediaAsset instead.
func TestListMediaAssets(t *testing.T) {
	svc, repo, _ := newMediaSvc()
	ctx := ctxTenant("t1")

	now := time.Now().UTC()
	png := "image/png"
	storefront := "Storefront.png"
	brochureName := "brochure.pdf"
	uploaded1 := now.Add(-time.Minute)
	uploaded2 := now.Add(-2 * time.Minute)

	repo.assets = append(repo.assets,
		&domain.MediaAsset{
			ID: uuid.New(), TenantID: "t1", StorageKey: "t1/a", ContentType: png,
			Filename: &storefront, UploadedAt: &uploaded1, CreatedAt: uploaded1,
		},
		&domain.MediaAsset{
			ID: uuid.New(), TenantID: "t1", StorageKey: "t1/b", ContentType: "application/pdf",
			Filename: &brochureName, UploadedAt: &uploaded2, CreatedAt: uploaded2,
		},
		// A bare reservation: no UploadedAt, so it must never surface here.
		&domain.MediaAsset{
			ID: uuid.New(), TenantID: "t1", StorageKey: "t1/c", ContentType: png,
			CreatedAt: now,
		},
	)

	t.Run("excludes unuploaded reservations", func(t *testing.T) {
		res, err := svc.ListMediaAssets(ctx, ListMediaInput{Limit: 20})
		require.NoError(t, err)
		require.Equal(t, 2, res.Total, "the reservation must not count")
		require.Len(t, res.Items, 2)
	})

	t.Run("kind filters by content type", func(t *testing.T) {
		res, err := svc.ListMediaAssets(ctx, ListMediaInput{Kind: "image", Limit: 20})
		require.NoError(t, err)
		require.Equal(t, 1, res.Total)
		require.Len(t, res.Items, 1)
		require.Equal(t, storefront, *res.Items[0].Filename)
	})

	t.Run("an unknown kind is rejected", func(t *testing.T) {
		_, err := svc.ListMediaAssets(ctx, ListMediaInput{Kind: "spreadsheet"})
		require.ErrorIs(t, err, ErrMediaKindUnknown)
	})

	t.Run("q matches the filename case-insensitively", func(t *testing.T) {
		res, err := svc.ListMediaAssets(ctx, ListMediaInput{Query: "STORE", Limit: 20})
		require.NoError(t, err)
		require.Equal(t, 1, res.Total)
		require.Equal(t, storefront, *res.Items[0].Filename)
	})

	t.Run("total counts the filtered set, not just the returned page", func(t *testing.T) {
		res, err := svc.ListMediaAssets(ctx, ListMediaInput{Limit: 1})
		require.NoError(t, err)
		require.Equal(t, 2, res.Total, "total must reflect both matching assets")
		require.Len(t, res.Items, 1, "but only one page's worth comes back")
		require.Equal(t, 1, res.Limit)
		require.Equal(t, 0, res.Offset)
	})

	t.Run("an out-of-range limit clamps to the default instead of erroring", func(t *testing.T) {
		res, err := svc.ListMediaAssets(ctx, ListMediaInput{Limit: 500})
		require.NoError(t, err)
		require.Equal(t, 20, res.Limit)

		res, err = svc.ListMediaAssets(ctx, ListMediaInput{Limit: -1})
		require.NoError(t, err)
		require.Equal(t, 20, res.Limit)
	})

	t.Run("a negative offset clamps to zero", func(t *testing.T) {
		res, err := svc.ListMediaAssets(ctx, ListMediaInput{Offset: -5})
		require.NoError(t, err)
		require.Equal(t, 0, res.Offset)
	})

	t.Run("list items carry no variants", func(t *testing.T) {
		res, err := svc.ListMediaAssets(ctx, ListMediaInput{Limit: 20})
		require.NoError(t, err)
		for _, item := range res.Items {
			require.Empty(t, item.Variants, "a list item must not carry variants — GetMediaAsset is the per-asset path")
		}
	})

	// A public delivery credential has never been able to enumerate the
	// collection — the edge resolves media by id, never by listing.
	t.Run("a delivery credential is refused", func(t *testing.T) {
		_, err := svc.ListMediaAssets(ctxDelivery("t1"), ListMediaInput{})
		require.Error(t, err)
	})
}

func seedFileType(t *testing.T, svc ContentService, ctx context.Context) error {
	t.Helper()
	_, err := svc.CreateContentType(ctx, CreateTypeInput{
		Name:  "doc",
		Label: "Doc",
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "cover", Type: domain.FieldTypeFile},
		},
	})
	return err
}

// The metadata endpoint must agree with the bytes endpoint. It used to skip the
// publication gate entirely, so for the same asset and the same credential
// /media/{id}/url answered 404 while /media/{id} answered 200 with size and
// content type — defeating the anti-oracle design ResolveMediaURL was written
// with (its 404-not-403 comment says so explicitly).
func TestMedia_DeliveryMetadataFollowsTheSameGateAsTheBytes(t *testing.T) {
	svc, repo, store := newMediaSvc()
	admin := ctxTenant("t1")
	require.NoError(t, seedFileType(t, svc, admin))
	asset := uploadAsset(t, svc, repo, store, admin)

	e, err := svc.CreateEntry(admin, "doc", mustJSON(t, map[string]any{"title": "T", "cover": asset.String()}))
	require.NoError(t, err)
	del := ctxDelivery("t1")

	// Draft entry: bytes refused, so metadata must be refused too — and refused
	// the same way a nonexistent asset is, or the refusal itself confirms it.
	_, _, bytesErr := svc.ResolveMediaURL(del, asset, "")
	require.Error(t, bytesErr)
	_, metaErr := svc.GetMediaAsset(del, asset)
	require.Error(t, metaErr, "metadata must not outlive the gate on the bytes")
	_, missingErr := svc.GetMediaAsset(del, uuid.New())
	require.Equal(t, missingErr.Error(), metaErr.Error(),
		"an unreferenced asset must be indistinguishable from one that does not exist")

	// The admin reads their own tenant's metadata regardless — this restricts
	// one audience, it does not remove the endpoint.
	_, err = svc.GetMediaAsset(admin, asset)
	require.NoError(t, err)

	// Publish → metadata opens up alongside the bytes.
	_, err = svc.SetEntryStatus(admin, "doc", e.ID, domain.StatusPublished, 0)
	require.NoError(t, err)
	_, _, err = svc.ResolveMediaURL(del, asset, "")
	require.NoError(t, err)
	dto, err := svc.GetMediaAsset(del, asset)
	require.NoError(t, err, "once the bytes are public the metadata may be too")
	require.Equal(t, asset, dto.ID)
}
