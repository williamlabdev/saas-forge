package service

import (
	"context"
	"encoding/json"
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
	signedPost []objectstore.UploadConstraints
	signedGet  []string
	deleted    []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string]objectstore.ObjectInfo{}}
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
	return nil
}

// landBytes simulates the client's direct-to-storage PUT succeeding.
func (f *fakeStore) landBytes(t *testing.T, repo *memRepo, assetID uuid.UUID, size int64, ct string) {
	t.Helper()
	for _, a := range repo.assets {
		if a.ID == assetID {
			f.objects[a.StorageKey] = objectstore.ObjectInfo{Size: size, ContentType: ct}
			return
		}
	}
	t.Fatalf("asset %s not found", assetID)
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

// Size and content type come from storage, never from the caller's claim.
func TestMedia_MetadataComesFromStorage(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")

	up, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "image/png"})
	require.NoError(t, err)
	// The client claimed png; what actually landed is a large jpeg.
	store.landBytes(t, repo, up.AssetID, 9_000_000, "image/jpeg")

	done, err := svc.CompleteMediaUpload(ctx, up.AssetID)
	require.NoError(t, err)
	require.EqualValues(t, 9_000_000, done.SizeBytes)
	require.Equal(t, "image/jpeg", done.ContentType, "storage is the source of truth, not the client")

	// Re-read rather than trusting the returned DTO: the DTO is assembled
	// separately from the write, so asserting only on it would pass even if the
	// persisted row kept the client's claim.
	stored, err := svc.GetMediaAsset(ctx, up.AssetID)
	require.NoError(t, err)
	require.EqualValues(t, 9_000_000, stored.SizeBytes, "the PERSISTED size must come from storage")
	require.Equal(t, "image/jpeg", stored.ContentType, "the PERSISTED content type must come from storage")
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

	_, _, err = svc.ResolveMediaURL(ctx, up.AssetID)
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
	_, _, err = svc.ResolveMediaURL(del, asset)
	require.Error(t, err, "media of an unpublished entry must not be served")

	// The admin can still preview it.
	url, _, err := svc.ResolveMediaURL(admin, asset)
	require.NoError(t, err)
	require.NotEmpty(t, url)

	// Publish → now it resolves for delivery too.
	_, err = svc.SetEntryStatus(admin, "doc", e.ID, domain.StatusPublished, 0)
	require.NoError(t, err)
	url, expires, err := svc.ResolveMediaURL(del, asset)
	require.NoError(t, err)
	require.NotEmpty(t, url)
	require.True(t, expires.After(time.Now().UTC()), "a signed URL must carry an expiry")

	// Unpublish → access is withdrawn again.
	_, err = svc.SetEntryStatus(admin, "doc", e.ID, domain.StatusDraft, 0)
	require.NoError(t, err)
	_, _, err = svc.ResolveMediaURL(del, asset)
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

	_, _, err = svc.ResolveMediaURL(ctxDelivery("t1"), asset)
	require.NoError(t, err)

	// Clear the file field in the working copy only.
	_, err = svc.UpdateEntry(admin, "doc", e.ID, json.RawMessage(`{"cover":""}`), 0)
	require.NoError(t, err)

	_, _, err = svc.ResolveMediaURL(ctxDelivery("t1"), asset)
	require.NoError(t, err, "the published snapshot still references the asset, so the live page must keep working")

	// Publishing the removal is what actually retires the asset.
	_, err = svc.SetEntryStatus(admin, "doc", e.ID, domain.StatusPublished, 0)
	require.NoError(t, err)

	_, _, err = svc.ResolveMediaURL(ctxDelivery("t1"), asset)
	require.Error(t, err, "once the removal is published, nothing published references the asset")
}

func TestMedia_DeleteRemovesBytes(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")
	require.NoError(t, seedFileType(t, svc, ctx))
	asset := uploadAsset(t, svc, repo, store, ctx)

	require.NoError(t, svc.DeleteMediaAsset(ctx, asset))
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
	_, _, bytesErr := svc.ResolveMediaURL(del, asset)
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
	_, _, err = svc.ResolveMediaURL(del, asset)
	require.NoError(t, err)
	dto, err := svc.GetMediaAsset(del, asset)
	require.NoError(t, err, "once the bytes are public the metadata may be too")
	require.Equal(t, asset, dto.ID)
}
