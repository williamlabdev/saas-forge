package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/service"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// The media lifecycle after the initial POST had no HTTP-layer tests: complete,
// read, resolve and delete were exercised only at the service layer. These four
// all follow the same shape (parse the id, call one method, encode), so the
// tests below concentrate on what that shape can get wrong — a bad id reaching
// the service, a status code that lies, and the two GETs answering each other's
// question.

func TestCompleteMediaUpload_OK(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{mediaAsset: adminMedia(id)}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/media/"+id.String()+"/complete", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastID != id {
		t.Fatalf("completed %s, want %s", svc.lastID, id)
	}
}

func TestCompleteMediaUpload_InvalidID(t *testing.T) {
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/media/not-a-uuid/complete", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastID != uuid.Nil {
		t.Fatalf("an unparseable id still reached the service: %s", svc.lastID)
	}
}

// Completing something already complete, or never uploaded, is the service's
// call — the handler's job is to not flatten that answer into a 200 or a 500.
func TestCompleteMediaUpload_ServiceErrorKeepsItsStatus(t *testing.T) {
	svc := &fakeContentService{err: apperrors.New("CONTENT_MEDIA_NOT_UPLOADED", "no object at that key", http.StatusConflict)}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/media/"+uuid.New().String()+"/complete", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// The new mismatch error must surface as 422 with its code, not flatten.
func TestCompleteMediaUpload_BytesMismatchKeepsItsStatus(t *testing.T) {
	svc := &fakeContentService{err: service.ErrMediaBytesMismatch}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/media/"+uuid.New().String()+"/complete", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "CONTENT_MEDIA_BYTES_MISMATCH") {
		t.Fatalf("body must carry the code: %s", rec.Body)
	}
}

func TestGetMediaAsset_OK(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{mediaAsset: adminMedia(id)}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/media/"+id.String(), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastID != id {
		t.Fatalf("read %s, want %s", svc.lastID, id)
	}
	var got struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if got.Data["id"] != id.String() {
		t.Fatalf("body describes %v, want %s", got.Data["id"], id)
	}
}

// TestListMediaAssets_OK pins that the route is a plain param pass-through —
// the service, not the handler, owns validation and defaulting (mirrors
// listEntries).
func TestListMediaAssets_OK(t *testing.T) {
	svc := &fakeContentService{mediaList: service.MediaListResult{Items: []service.MediaAssetDTO{}, Total: 0, Limit: 10, Offset: 5}}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/media?limit=10&offset=5&q=Store&kind=image", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	want := service.ListMediaInput{Query: "Store", Kind: "image", Limit: 10, Offset: 5}
	if svc.lastMediaList != want {
		t.Fatalf("service received %+v, want %+v", svc.lastMediaList, want)
	}
	var got struct {
		Data service.MediaListResult `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if got.Data.Items == nil {
		t.Fatalf("items decoded as null, want []")
	}
}

// TestListMediaAssets_BadKind: the service refuses an unknown `kind` with the
// domain error, and the handler must carry that status and code through
// unchanged rather than flattening it.
func TestListMediaAssets_BadKind(t *testing.T) {
	svc := &fakeContentService{err: apperrors.New("CONTENT_MEDIA_KIND_UNKNOWN", "unknown media kind", http.StatusBadRequest)}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/media?kind=audio", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if got.Error.Code != "CONTENT_MEDIA_KIND_UNKNOWN" {
		t.Fatalf("error code=%q, want CONTENT_MEDIA_KIND_UNKNOWN", got.Error.Code)
	}
}

func TestGetMediaAsset_InvalidID(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodGet, "/api/v1/content/media/not-a-uuid", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestGetMediaAsset_NotFound(t *testing.T) {
	svc := &fakeContentService{err: apperrors.New("CONTENT_MEDIA_NOT_FOUND", "gone", http.StatusNotFound)}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/media/"+uuid.New().String(), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// ADR-005: the API hands out a short-lived signed URL and the client fetches
// the bytes from object storage itself. The assertion is therefore about what
// the response is NOT — no media traffic on this connection, whatever the size.
func TestResolveMediaURL_ReturnsASignedURLNotTheBytes(t *testing.T) {
	id := uuid.New()
	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	svc := &fakeContentService{mediaURL: "https://objects.test/o/abc?sig=xyz", mediaExpires: expires}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/media/"+id.String()+"/url", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Fatalf("Content-Type=%q — this route answers with JSON, never with media", ct)
	}
	var got struct {
		Data struct {
			URL       string `json:"url"`
			ExpiresAt string `json:"expires_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if got.Data.URL != "https://objects.test/o/abc?sig=xyz" {
		t.Fatalf("url=%q", got.Data.URL)
	}
	// The expiry has to reach the caller: a URL whose lifetime is unknowable is
	// one a client must re-request on every use or cache past its death.
	if !strings.HasPrefix(got.Data.ExpiresAt, "2030-01-02T03:04:05") {
		t.Fatalf("expires_at=%q, want the service's expiry", got.Data.ExpiresAt)
	}
}

func TestResolveMediaURL_InvalidID(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodGet, "/api/v1/content/media/not-a-uuid/url", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestResolveMediaURL_ServiceErrorKeepsItsStatus(t *testing.T) {
	svc := &fakeContentService{err: apperrors.New("CONTENT_MEDIA_DISABLED", "object storage is not configured", http.StatusServiceUnavailable)}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/media/"+uuid.New().String()+"/url", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// GET /media/{id} and GET /media/{id}/url are one path segment apart and return
// different things. Wired to the same method, both would look plausible in a
// smoke test: the metadata route would start handing out signed URLs, or the
// URL route would answer with metadata a caller cannot fetch anything with.
func TestMediaMetadataAndURLRoutesDoNotAnswerEachOther(t *testing.T) {
	id := uuid.New()
	newSvc := func() *fakeContentService {
		return &fakeContentService{mediaAsset: adminMedia(id), mediaURL: "https://objects.test/o/abc"}
	}

	meta := do(t, newSvc(), http.MethodGet, "/api/v1/content/media/"+id.String(), "")
	if strings.Contains(meta.Body.String(), "objects.test") {
		t.Fatalf("the metadata route handed out a signed URL: %s", meta.Body)
	}

	signed := do(t, newSvc(), http.MethodGet, "/api/v1/content/media/"+id.String()+"/url", "")
	if !strings.Contains(signed.Body.String(), "objects.test") {
		t.Fatalf("the url route did not answer with a URL: %s", signed.Body)
	}
	if strings.Contains(signed.Body.String(), `"size_bytes"`) {
		t.Fatalf("the url route answered with asset metadata: %s", signed.Body)
	}
}

func TestDeleteMediaAsset_OK(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodDelete, "/api/v1/content/media/"+id.String(), "")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastID != id {
		t.Fatalf("deleted %s, want %s", svc.lastID, id)
	}
}

func TestDeleteMediaAsset_InvalidID(t *testing.T) {
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodDelete, "/api/v1/content/media/not-a-uuid", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastID != uuid.Nil {
		t.Fatalf("an unparseable id still reached the service: %s", svc.lastID)
	}
}

// A delete refused because the asset is still referenced by an entry must keep
// its status: turning it into a 204 tells the caller the file is gone when it
// is not.
func TestDeleteMediaAsset_StillReferenced(t *testing.T) {
	svc := &fakeContentService{err: apperrors.New("CONTENT_MEDIA_IN_USE", "referenced by 3 entries", http.StatusConflict)}
	rec := do(t, svc, http.MethodDelete, "/api/v1/content/media/"+uuid.New().String(), "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// --- renditions (ADR-019) ----------------------------------------------------

func TestResolveMediaURL_PassesPresetThroughVerbatim(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{mediaURL: "https://bucket/signed"}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/media/"+id.String()+"/url?preset=thumb", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastPreset != "thumb" {
		t.Fatalf("preset=%q want thumb", svc.lastPreset)
	}

	// No query: the service must see "" (the original), not a default the
	// handler invented.
	svc = &fakeContentService{mediaURL: "https://bucket/signed"}
	do(t, svc, http.MethodGet, "/api/v1/content/media/"+id.String()+"/url", "")
	if svc.lastPreset != "" {
		t.Fatalf("preset=%q want empty", svc.lastPreset)
	}
}

func TestResolveMediaURL_UnknownPresetKeepsTheServices400(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{err: apperrors.New("CONTENT_MEDIA_PRESET_UNKNOWN", "unknown", http.StatusBadRequest)}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/media/"+id.String()+"/url?preset=huge", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "CONTENT_MEDIA_PRESET_UNKNOWN") {
		t.Fatalf("body=%s", rec.Body)
	}
}

func TestEnqueueMediaVariants_Accepted(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{mediaAsset: adminMedia(id)}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/media/"+id.String()+"/variants", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastID != id || svc.enqueueCalls != 1 {
		t.Fatalf("lastID=%s calls=%d", svc.lastID, svc.enqueueCalls)
	}
	var body struct {
		Data struct {
			ID uuid.UUID `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Data.ID != id {
		t.Fatalf("body=%s err=%v", rec.Body, err)
	}
}

func TestEnqueueMediaVariants_InvalidIDAndServiceStatus(t *testing.T) {
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/media/not-a-uuid/variants", "")
	if rec.Code != http.StatusBadRequest || svc.enqueueCalls != 0 {
		t.Fatalf("code=%d calls=%d", rec.Code, svc.enqueueCalls)
	}

	svc = &fakeContentService{err: apperrors.New("CONTENT_MEDIA_NOT_IMAGE", "not an image", http.StatusUnprocessableEntity)}
	rec = do(t, svc, http.MethodPost, "/api/v1/content/media/"+uuid.New().String()+"/variants", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// --- POST /media/sweep (003-orphan-gc) --------------------------------------

// The handler parses the body and carries the shape through: older_than as a
// duration, limit and dry_run verbatim. Defaulting and clamping are the
// service's job — a zero input here must arrive as a zero input there.
func TestSweepMediaAssets_OK(t *testing.T) {
	svc := &fakeContentService{sweepResult: service.SweepResult{Deleted: 3}}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/media/sweep", `{"older_than":"720h","limit":50,"dry_run":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastSweep.Limit != 50 || !svc.lastSweep.DryRun {
		t.Fatalf("service received %+v", svc.lastSweep)
	}
	if svc.lastSweep.OlderThan != 720*time.Hour {
		t.Fatalf("older_than=%s, want 720h", svc.lastSweep.OlderThan)
	}
	var body struct {
		Data service.SweepResult `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body.Data.Deleted != 3 {
		t.Fatalf("deleted=%d, want 3", body.Data.Deleted)
	}
}

// An empty body sweeps with the service defaults — the operator's plain
// "clean what is old" call.
func TestSweepMediaAssets_EmptyBodyTakesDefaults(t *testing.T) {
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/media/sweep", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastSweep != (service.SweepInput{}) {
		t.Fatalf("service received %+v, want zero input", svc.lastSweep)
	}
}

// A garbage older_than must fail loudly as a 400, never silently mean
// "default" — that silence would sweep with a retention the caller did not ask
// for.
func TestSweepMediaAssets_BadOlderThanIs400(t *testing.T) {
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/media/sweep", `{"older_than":"thirty days"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if got.Error.Code != "CONTENT_SWEEP_OLDER_THAN_INVALID" {
		t.Fatalf("error code=%q", got.Error.Code)
	}
}
