package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Variants are the service's contract with ADR-019: rows appear only for
// uploaded images, the admin sees them, delivery never does, and `?preset=`
// degrades to the original rather than to a 404.

func errCode(err error) string {
	ae, ok := apperrors.As(err)
	if !ok {
		return ""
	}
	return ae.Code
}

func variantRows(repo *memRepo, id uuid.UUID) map[string]*domain.MediaVariant {
	out := map[string]*domain.MediaVariant{}
	for _, v := range repo.variants {
		if v.AssetID == id {
			out[v.Preset] = v
		}
	}
	return out
}

func finishDone(t *testing.T, repo *memRepo, id uuid.UUID, preset, key string) {
	t.Helper()
	found, err := repo.FinishMediaVariant(context.Background(), "t1", id, preset, repository.MediaVariantOutcome{
		State: domain.MediaVariantDone, StorageKey: key, ContentType: "image/jpeg", SizeBytes: 100, WidthPx: 320, HeightPx: 160,
	})
	require.NoError(t, err)
	require.True(t, found)
}

func TestMediaVariants_CompleteUploadEnqueuesOnlyForImages(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")

	img := uploadAsset(t, svc, repo, store, ctx) // image/png
	rows := variantRows(repo, img)
	require.Len(t, rows, 5, "original + four presets")
	for _, p := range append([]string{"original"}, domain.MediaPresetNames()...) {
		require.Contains(t, rows, p)
		assert.Equal(t, domain.MediaVariantPending, rows[p].State, p)
		assert.Zero(t, rows[p].Attempts)
	}
	assert.NotEmpty(t, rows["original"].StorageKey, "the original row is born knowing its key")
	assert.Empty(t, rows["thumb"].StorageKey)

	up, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "application/pdf"})
	require.NoError(t, err)
	store.landBytes(t, repo, up.AssetID, 999, "application/pdf")
	dto, err := svc.CompleteMediaUpload(ctx, up.AssetID)
	require.NoError(t, err)
	assert.Empty(t, variantRows(repo, up.AssetID), "a PDF has no renditions and no rows to say so")
	assert.Nil(t, dto.Variants)
}

func TestMediaVariants_CompleteUploadRowsAndUploadedFlagAreOneWrite(t *testing.T) {
	// A reservation that fails to complete (nothing landed) must leave no
	// variant rows: the two facts "uploaded" and "queued" travel together.
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")
	up, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "image/png"})
	require.NoError(t, err)
	_, err = svc.CompleteMediaUpload(ctx, up.AssetID)
	require.Error(t, err)
	assert.Empty(t, variantRows(repo, up.AssetID))
	_ = store
}

func TestMediaVariants_AdminSeesThemDeliveryNever(t *testing.T) {
	svc, repo, store := newMediaSvc()
	admin := ctxTenant("t1")
	require.NoError(t, seedFileType(t, svc, admin))
	asset := uploadAsset(t, svc, repo, store, admin)
	finishDone(t, repo, asset, "thumb", "t1/k.thumb.jpg")

	dto, err := svc.GetMediaAsset(admin, asset)
	require.NoError(t, err)
	require.Len(t, dto.Variants, 5)
	assert.Equal(t, []string{"original", "thumb", "small", "medium", "large"}, presetOrder(dto.Variants), "fixed order, original first")
	var thumb MediaVariantDTO
	for _, v := range dto.Variants {
		if v.Preset == "thumb" {
			thumb = v
		}
	}
	assert.Equal(t, domain.MediaVariantDone, thumb.State)
	assert.Equal(t, 320, thumb.WidthPx)
	assert.Equal(t, "image/jpeg", thumb.ContentType)

	body, err := json.Marshal(dto)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"variants":[`)
	assert.NotContains(t, string(body), "storage_key", "storage keys are an implementation detail even for the admin")
	assert.NotContains(t, string(body), "t1/k.thumb.jpg")

	// Publish so the delivery credential is allowed to read the asset at all,
	// then prove the read carries no variants — and no new key of any kind.
	e, err := svc.CreateEntry(admin, "doc", mustJSON(t, map[string]any{"title": "T", "cover": asset.String()}))
	require.NoError(t, err)
	_, err = svc.SetEntryStatus(admin, "doc", e.ID, domain.StatusPublished, 0)
	require.NoError(t, err)
	del, err := svc.GetMediaAsset(ctxDelivery("t1"), asset)
	require.NoError(t, err)
	assert.Nil(t, del.Variants)
	assert.ElementsMatch(t, deliveryMediaKeys, jsonKeys(t, del), "the delivery key set is frozen; variants are admin-only")
}

func presetOrder(vs []MediaVariantDTO) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Preset)
	}
	return out
}

func TestMediaVariants_EnqueueResetsRowsAndKeepsKeys(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")
	asset := uploadAsset(t, svc, repo, store, ctx)
	finishDone(t, repo, asset, "thumb", "t1/k.thumb.jpg")
	_, err := repo.FinishMediaVariant(ctx, "t1", asset, "large", repository.MediaVariantOutcome{State: domain.MediaVariantFailed, Error: "boom"})
	require.NoError(t, err)
	_, err = repo.FinishMediaVariant(ctx, "t1", asset, "large", repository.MediaVariantOutcome{State: domain.MediaVariantFailed, Error: "boom"})
	require.NoError(t, err)
	require.Equal(t, 2, variantRows(repo, asset)["large"].Attempts)

	dto, err := svc.EnqueueMediaVariants(ctx, asset)
	require.NoError(t, err)
	require.Len(t, dto.Variants, 5)
	rows := variantRows(repo, asset)
	for p, r := range rows {
		assert.Equal(t, domain.MediaVariantPending, r.State, p)
		assert.Zero(t, r.Attempts, p)
		assert.Empty(t, r.Error, p)
	}
	assert.Equal(t, "t1/k.thumb.jpg", rows["thumb"].StorageKey,
		"the old rendition's key survives the reset so the overwrite lands on the same object and nothing is orphaned")
	for _, v := range dto.Variants {
		assert.Equal(t, domain.MediaVariantPending, v.State, v.Preset)
	}
}

func TestMediaVariants_EnqueueRefusals(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")

	up, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "image/png"})
	require.NoError(t, err)
	_, err = svc.EnqueueMediaVariants(ctx, up.AssetID)
	assert.Equal(t, "CONTENT_MEDIA_NOT_UPLOADED", errCode(err), "a reservation has no bytes to render")

	pdf, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "application/pdf"})
	require.NoError(t, err)
	store.landBytes(t, repo, pdf.AssetID, 10, "application/pdf")
	_, err = svc.CompleteMediaUpload(ctx, pdf.AssetID)
	require.NoError(t, err)
	_, err = svc.EnqueueMediaVariants(ctx, pdf.AssetID)
	assert.Equal(t, "CONTENT_MEDIA_NOT_IMAGE", errCode(err))
	assert.Empty(t, variantRows(repo, pdf.AssetID))

	_, err = svc.EnqueueMediaVariants(ctx, uuid.New())
	require.Error(t, err)

	other := uploadAsset(t, svc, repo, store, ctx)
	_, err = svc.EnqueueMediaVariants(ctxTenant("t2"), other)
	require.Error(t, err, "another tenant's asset is not found, not re-queued")

	_, err = svc.EnqueueMediaVariants(ctxDelivery("t1"), other)
	require.Error(t, err, "a delivery credential cannot queue work")

	noStore, _ := newSvc()
	_, err = noStore.EnqueueMediaVariants(ctx, other)
	assert.Equal(t, ErrMediaDisabled.Code, errCode(err))
}

func TestMediaVariants_ResolvePresetSignsVariantOrFallsBack(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")
	asset := uploadAsset(t, svc, repo, store, ctx)
	original := variantRows(repo, asset)["original"].StorageKey

	// Pending rendition: the original is served, never a 404 or a wait.
	url, _, err := svc.ResolveMediaURL(ctx, asset, "thumb")
	require.NoError(t, err)
	require.NotEmpty(t, url)
	assert.Equal(t, []string{original}, store.signedGet)

	// Done rendition: its own key is signed.
	finishDone(t, repo, asset, "thumb", original+".thumb.jpg")
	_, _, err = svc.ResolveMediaURL(ctx, asset, "thumb")
	require.NoError(t, err)
	assert.Equal(t, original+".thumb.jpg", store.signedGet[len(store.signedGet)-1])

	// A skipped preset (source too small) falls back to the original too.
	_, err = repo.FinishMediaVariant(ctx, "t1", asset, "large", repository.MediaVariantOutcome{State: domain.MediaVariantSkipped, SkipReason: domain.MediaSkipSourceSmaller})
	require.NoError(t, err)
	_, _, err = svc.ResolveMediaURL(ctx, asset, "large")
	require.NoError(t, err)
	assert.Equal(t, original, store.signedGet[len(store.signedGet)-1])

	// No preset: the original, as before this feature existed.
	_, _, err = svc.ResolveMediaURL(ctx, asset, "")
	require.NoError(t, err)
	assert.Equal(t, original, store.signedGet[len(store.signedGet)-1])
}

func TestMediaVariants_ResolveUnknownPresetIs400(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")
	asset := uploadAsset(t, svc, repo, store, ctx)
	for _, bad := range []string{"huge", "original", "Thumb"} {
		_, _, err := svc.ResolveMediaURL(ctx, asset, bad)
		ae, ok := apperrors.As(err)
		require.True(t, ok, bad)
		assert.Equal(t, "CONTENT_MEDIA_PRESET_UNKNOWN", ae.Code, bad)
		assert.Equal(t, 400, ae.HTTPStatus, bad)
		assert.True(t, strings.Contains(ae.Message, "thumb"), "the refusal names the valid presets")
	}
	assert.Empty(t, store.signedGet, "nothing is signed for a request that was refused")
}

func TestMediaVariants_DeliveryPresetKeepsThePublishedGate(t *testing.T) {
	svc, repo, store := newMediaSvc()
	admin := ctxTenant("t1")
	require.NoError(t, seedFileType(t, svc, admin))
	asset := uploadAsset(t, svc, repo, store, admin)
	original := variantRows(repo, asset)["original"].StorageKey
	finishDone(t, repo, asset, "thumb", original+".thumb.jpg")
	del := ctxDelivery("t1")

	_, _, err := svc.ResolveMediaURL(del, asset, "thumb")
	require.Error(t, err, "a preset is not a side door around the published-only gate")
	assert.Empty(t, store.signedGet)

	// Unknown preset on the delivery side: the same 400 — the gate check
	// runs first for a draft, so use a published entry to see the preset
	// refusal at all.
	e, err := svc.CreateEntry(admin, "doc", mustJSON(t, map[string]any{"title": "T", "cover": asset.String()}))
	require.NoError(t, err)
	_, err = svc.SetEntryStatus(admin, "doc", e.ID, domain.StatusPublished, 0)
	require.NoError(t, err)
	_, _, err = svc.ResolveMediaURL(del, asset, "thumb")
	require.NoError(t, err)
	assert.Equal(t, original+".thumb.jpg", store.signedGet[len(store.signedGet)-1])
	_, _, err = svc.ResolveMediaURL(del, asset, "nope")
	assert.Equal(t, "CONTENT_MEDIA_PRESET_UNKNOWN", errCode(err))
}

func TestMediaVariants_DeleteSweepsRenditionObjects(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")
	asset := uploadAsset(t, svc, repo, store, ctx)
	original := variantRows(repo, asset)["original"].StorageKey
	finishDone(t, repo, asset, "thumb", original+".thumb.jpg")
	finishDone(t, repo, asset, "small", original+".small.jpg")

	require.NoError(t, svc.DeleteMediaAsset(ctx, asset, false))
	assert.ElementsMatch(t, []string{original + ".thumb.jpg", original + ".small.jpg", original}, store.deleted,
		"every rendition object goes with the source; pending/skipped rows have nothing to delete")
	assert.Empty(t, variantRows(repo, asset), "rows cascade with the asset")
	_, err := svc.GetMediaAsset(ctx, asset)
	require.Error(t, err)
}

func TestMediaVariants_DTOProjection(t *testing.T) {
	now := time.Now().UTC()
	next := now.Add(time.Minute)
	a := &domain.MediaAsset{ID: uuid.New(), TenantID: "t1", StorageKey: "t1/k", ContentType: "image/png", UploadedAt: &now, CreatedAt: now}
	vs := []*domain.MediaVariant{
		{Preset: "original", State: domain.MediaVariantDone, StorageKey: "t1/k", ContentType: "image/png", SizeBytes: 9, WidthPx: 100, HeightPx: 50, UpdatedAt: now},
		{Preset: "thumb", State: domain.MediaVariantPending, Attempts: 1, Error: "bucket says no", NextAttemptAt: next, UpdatedAt: now},
		{Preset: "large", State: domain.MediaVariantSkipped, SkipReason: domain.MediaSkipSourceSmaller, UpdatedAt: now},
	}
	dto := ProjectMediaAsset(a, adminSubject(), vs...)
	require.Len(t, dto.Variants, 3)
	body, err := json.Marshal(dto)
	require.NoError(t, err)
	var m struct {
		Variants []map[string]any `json:"variants"`
	}
	require.NoError(t, json.Unmarshal(body, &m))
	require.Len(t, m.Variants, 3)
	assert.Equal(t, "done", m.Variants[0]["state"])
	assert.EqualValues(t, 100, m.Variants[0]["width_px"])
	assert.Equal(t, "bucket says no", m.Variants[1]["error"])
	assert.EqualValues(t, 1, m.Variants[1]["attempts"])
	assert.NotEmpty(t, m.Variants[1]["next_attempt_at"])
	assert.Equal(t, "source_smaller", m.Variants[2]["skip_reason"])
	for _, v := range m.Variants {
		_, has := v["storage_key"]
		assert.False(t, has, "storage_key never reaches the wire")
	}

	assert.Nil(t, ProjectMediaAsset(a, deliverySubject(), vs...).Variants)
	assert.ElementsMatch(t, deliveryMediaKeys, jsonKeys(t, ProjectMediaAsset(a, deliverySubject(), vs...)))
}
