package handler

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/service"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// ADR-024 (2.8a): GET .../referenced-by is a plain param pass-through, same
// shape as listEntries/listMediaAssets — the handler's job is parsing ?type=,
// the id, limit and offset correctly and nothing more; defaulting and the
// 50/max-200 clamp live in the service (clampReferencedByPage).

func TestEntryReferencedBy_OK(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{referencedBy: service.ReferencedByDTO{Total: 3}}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/entries/"+id.String()+"/referenced-by?type=post&limit=10&offset=5", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastTypeName != "post" || svc.lastID != id {
		t.Fatalf("service saw type=%q id=%s, want post/%s", svc.lastTypeName, svc.lastID, id)
	}
	if svc.lastRefLimit != 10 || svc.lastRefOffset != 5 {
		t.Fatalf("limit=%d offset=%d, want 10/5", svc.lastRefLimit, svc.lastRefOffset)
	}
}

// No ?limit=/?offset=: the handler must forward 0, not invent a default — the
// service (clampReferencedByPage) owns defaulting to 50.
func TestEntryReferencedBy_DefaultsLimitOffsetToZero(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/entries/"+id.String()+"/referenced-by?type=post", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastRefLimit != 0 || svc.lastRefOffset != 0 {
		t.Fatalf("limit=%d offset=%d, want 0/0 (handler must not invent a default)", svc.lastRefLimit, svc.lastRefOffset)
	}
}

func TestEntryReferencedBy_InvalidID(t *testing.T) {
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/entries/not-a-uuid/referenced-by?type=post", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastID != uuid.Nil {
		t.Fatalf("an unparseable id still reached the service: %s", svc.lastID)
	}
}

// Every other single-entry route in this router requires ?type= (requireType)
// rather than taking it as a path segment; referenced-by must follow the same
// convention and 400 the same way when it is missing.
func TestEntryReferencedBy_MissingType(t *testing.T) {
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/entries/"+uuid.New().String()+"/referenced-by", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastID != uuid.Nil {
		t.Fatalf("the service was called despite a missing ?type=: id=%s", svc.lastID)
	}
}

func TestEntryReferencedBy_ServiceErrorKeepsItsStatus(t *testing.T) {
	svc := &fakeContentService{err: apperrors.New("CONTENT_AGENT_SCOPE_UNTYPED", "agent credentials cannot use an untyped call", http.StatusForbidden)}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/entries/"+uuid.New().String()+"/referenced-by?type=post", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestMediaReferencedBy_OK(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{referencedBy: service.ReferencedByDTO{Total: 1}}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/media/"+id.String()+"/referenced-by?limit=25&offset=50", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastID != id {
		t.Fatalf("service saw id=%s, want %s", svc.lastID, id)
	}
	if svc.lastRefLimit != 25 || svc.lastRefOffset != 50 {
		t.Fatalf("limit=%d offset=%d, want 25/50", svc.lastRefLimit, svc.lastRefOffset)
	}
}

func TestMediaReferencedBy_InvalidID(t *testing.T) {
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/media/not-a-uuid/referenced-by", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastID != uuid.Nil {
		t.Fatalf("an unparseable id still reached the service: %s", svc.lastID)
	}
}

// A media reverse-lookup is authorized untyped, so an agent credential is
// refused by §4 before row-filtering ever runs (ADR-024 §Known limitations) —
// the handler's only job is to carry that status through unchanged.
func TestMediaReferencedBy_ServiceErrorKeepsItsStatus(t *testing.T) {
	svc := &fakeContentService{err: apperrors.New("CONTENT_AGENT_SCOPE_UNTYPED", "agent credentials cannot use an untyped call", http.StatusForbidden)}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/media/"+uuid.New().String()+"/referenced-by", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// --- DELETE /media/{id}?force= (ADR-024, 2.6a) ------------------------------

func TestDeleteMediaAsset_ForceTrueReachesService(t *testing.T) {
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodDelete, "/api/v1/content/media/"+uuid.New().String()+"?force=true", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !svc.lastForceDelete {
		t.Fatalf("force=true on the query string did not reach the service")
	}
}

// A bare DELETE (no ?force=) must default to false — the in-use check stays on
// unless the caller explicitly opts out.
func TestDeleteMediaAsset_DefaultsForceToFalse(t *testing.T) {
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodDelete, "/api/v1/content/media/"+uuid.New().String(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastForceDelete {
		t.Fatalf("force defaulted to true with no ?force= on the query string")
	}
}

// Anything other than the literal "true" must not be treated as force — a
// typo'd ?force=1 or ?force=yes must still run the in-use check.
func TestDeleteMediaAsset_ForceOnlyRecognizesTheLiteralTrue(t *testing.T) {
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodDelete, "/api/v1/content/media/"+uuid.New().String()+"?force=1", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastForceDelete {
		t.Fatalf("?force=1 was treated as force=true")
	}
}

// --- GET /media?orphan= (ADR-024, 2.6a) -------------------------------------

func TestListMediaAssets_OrphanTrueReachesService(t *testing.T) {
	svc := &fakeContentService{mediaList: service.MediaListResult{Items: []service.MediaAssetDTO{}}}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/media?orphan=true", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !svc.lastMediaList.Orphan {
		t.Fatalf("?orphan=true did not reach the service")
	}
}

func TestListMediaAssets_DefaultsOrphanToFalse(t *testing.T) {
	svc := &fakeContentService{mediaList: service.MediaListResult{Items: []service.MediaAssetDTO{}}}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/media", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastMediaList.Orphan {
		t.Fatalf("orphan defaulted to true with no ?orphan= on the query string")
	}
}
