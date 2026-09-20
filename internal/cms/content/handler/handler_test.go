package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/service"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// adminEntry and adminMedia build DTOs the way production does — through the
// projector. A hand-built literal carries no audience and refuses to marshal
// (service.ProjectEntry's doc has the reasoning), which is the point: a fake
// that could conjure a renderable DTO would be a fake of something the real
// service cannot do.
func adminEntry(id uuid.UUID, typeName, status string) service.EntryDTO {
	return service.ProjectEntry(&domain.ContentType{Name: typeName}, &domain.Entry{ID: id, Status: status}, adminSubject())
}

func adminMedia(id uuid.UUID) service.MediaAssetDTO {
	return service.ProjectMediaAsset(&domain.MediaAsset{ID: id}, adminSubject())
}

func adminSubject() authn.Subject {
	return authn.Subject{UserID: uuid.New(), TenantID: "t", TenantRole: "admin"}
}

// fakeContentService is a programmable ContentService double. Each method
// returns the configured value/err, recording the last args for assertions.
type fakeContentService struct {
	artifact  domain.Artifact
	plan      service.PlanResult
	typeDTO   service.ContentTypeDTO
	typesList []service.ContentTypeDTO
	entryDTO  service.EntryDTO
	listRes   *service.EntryListResult
	err       error

	previewLink service.PreviewLinkDTO

	// schedule (ADR-017). lastSchedule is the decoded body, so a handler test
	// can prove the route passed the caller's action and run_at through
	// verbatim — the two values every error code downstream is about.
	scheduleDTO  service.EntryScheduleDTO
	lastSchedule service.ScheduleEntryInput
	cancelCalls  int

	// revisions (ADR-018). lastRevisionNo is what the route parsed out of the
	// path, and lastRestore is the decoded body, so a handler test can prove the
	// ordinal reached the service verbatim rather than being defaulted by a
	// silently swallowed parse error.
	revisions          []service.EntryRevisionDTO
	revisionDetail     service.EntryRevisionDetailDTO
	revisionDiff       service.EntryRevisionDiffDTO
	restoreResult      service.RestoreEntryResultDTO
	lastRevisionNo     int
	lastRestore        service.RestoreEntryInput
	lastRestoreIfMatch int

	// review decisions (ADR-014 Amendment: review decisions). lastRequestChanges
	// is the decoded body and lastRequestChangesIfMatch the parsed precondition,
	// the same pair RestoreEntryRevision's fields above carry for restore.
	reviewDecisionDTO         service.ReviewDecisionDTO
	reviewDecisions           []service.ReviewDecisionDTO
	lastRequestChanges        service.RequestChangesInput
	lastRequestChangesIfMatch int

	activity          []service.ActivityDTO
	lastActivityInput service.ListActivityInput

	pending          []service.PendingEntryDTO
	lastPendingInput service.ListPendingReviewInput

	search          []service.SearchResultDTO
	lastSearchInput service.SearchInput

	locales          []service.LocaleDTO
	lastLocalesInput service.ListLocalesInput

	attribution         service.EntryAttributionDTO
	lastAttributionType string
	lastAttributionID   uuid.UUID

	// components (ADR-020). lastComponent is the name the route parsed, and
	// the three inputs are what reached the service verbatim.
	componentDTO        service.ComponentDTO
	componentList       []service.ComponentDTO
	lastComponent       string
	lastComponentCreate service.CreateComponentInput
	lastComponentUpdate service.UpdateComponentInput
	lastComponentField  service.FieldInput
	deleteComponentCall int

	// component templates (ADR-020 Amendment 2). lastTemplateName is the name
	// the route parsed out of POST /components/templates/{name}.
	componentTemplateList []service.ComponentTemplateDTO
	lastTemplateName      string

	lastFieldKey    string
	lastForce       bool
	lastRename      service.RenameInput
	lastUpdateType  service.UpdateTypeInput
	lastUpdateField service.UpdateFieldInput

	lastTypeName        string
	lastPayload         json.RawMessage
	lastID              uuid.UUID
	lastList            service.ListEntriesInput
	lastGet             service.GetEntryInput
	lastExpectedVersion int
	lastStatus          string
	lastCreate          service.CreateLocalizedInput
	translations        []service.EntryDTO
	lastContentType     string
	mediaUpload         service.MediaUploadDTO
	mediaAsset          service.MediaAssetDTO
	lastMediaCreate     service.CreateMediaUploadInput
	lastMediaPatch      service.UpdateMediaAssetInput
	mediaURL            string
	mediaList           service.MediaListResult
	lastMediaList       service.ListMediaInput
	// lastForceDelete is DeleteMediaAsset's force flag (ADR-024) — kept apart
	// from lastForce, which schema field delete already owns.
	lastForceDelete bool
	// sweepResult is what POST /media/sweep answers with; lastSweep is the
	// input it received, verbatim — defaulting lives in the service.
	sweepResult service.SweepResult
	lastSweep   service.SweepInput
	// referencedBy backs both EntryReferencedBy and MediaReferencedBy;
	// lastRefLimit/lastRefOffset are whichever one the route under test called
	// most recently reached the service with.
	referencedBy  service.ReferencedByDTO
	lastRefLimit  int
	lastRefOffset int

	webhookCreated    service.WebhookCreatedDTO
	webhookList       []service.WebhookDTO
	lastWebhookCreate service.CreateWebhookInput

	lastArtifact domain.Artifact
	lastPrune    bool
	planCalls    int
	applyCalls   int
	// Schema proposals (ADR-013 §3 step 8).
	proposal         service.SchemaProposalDTO
	ownProposal      service.OwnSchemaProposalDTO
	ownProposalCalls int
	ownProposals     []service.OwnSchemaProposalDTO
	// ownProposalListCalls / queueListCalls tell the two list routes apart; see
	// ListOwnSchemaProposals below.
	ownProposalListCalls int
	queueListCalls       int
	proposals            []service.SchemaProposalDTO
	proposeCalls         int
	approveCalls         int
	rejectCalls          int
	lastProposalID       uuid.UUID
	// lastApproveSteps is the steps argument ApproveSchemaProposal received,
	// verbatim — nil for an absent/null body, distinct from an empty non-nil
	// slice for `{"steps":[]}` (缺口計畫 5.1). A test asserts on this to prove
	// the handler decoded the body rather than the service inventing a default.
	lastApproveSteps []int

	mediaExpires time.Time
	// lastPreset is the ?preset= string ResolveMediaURL received, verbatim —
	// the handler must carry it and not interpret it (ADR-019).
	lastPreset   string
	enqueueCalls int
}

func (f *fakeContentService) CreateContentType(_ context.Context, _ service.CreateTypeInput) (service.ContentTypeDTO, error) {
	return f.typeDTO, f.err
}
func (f *fakeContentService) AddField(_ context.Context, name string, _ service.FieldInput) (service.ContentTypeDTO, error) {
	f.lastTypeName = name
	return f.typeDTO, f.err
}
func (f *fakeContentService) ListContentTypes(context.Context) ([]service.ContentTypeDTO, error) {
	return f.typesList, f.err
}

// Schema mutation. These record what the routes plumbed through — the handler's
// job is URL params, the force flag and the status code; the policy is asserted
// at the service layer and the data migration against real Postgres.
func (f *fakeContentService) UpdateContentType(_ context.Context, name string, in service.UpdateTypeInput) (service.ContentTypeDTO, error) {
	f.lastTypeName, f.lastUpdateType = name, in
	return f.typeDTO, f.err
}
func (f *fakeContentService) RenameContentType(_ context.Context, name string, in service.RenameInput) (service.ContentTypeDTO, error) {
	f.lastTypeName, f.lastRename = name, in
	return f.typeDTO, f.err
}
func (f *fakeContentService) DeleteContentType(_ context.Context, name string) error {
	f.lastTypeName = name
	return f.err
}
func (f *fakeContentService) UpdateField(_ context.Context, name, key string, in service.UpdateFieldInput) (service.ContentTypeDTO, error) {
	f.lastTypeName, f.lastFieldKey, f.lastUpdateField = name, key, in
	return f.typeDTO, f.err
}
func (f *fakeContentService) RenameField(_ context.Context, name, key string, in service.RenameInput) (service.ContentTypeDTO, error) {
	f.lastTypeName, f.lastFieldKey, f.lastRename = name, key, in
	return f.typeDTO, f.err
}
func (f *fakeContentService) DeleteField(_ context.Context, name, key string, force bool) (service.ContentTypeDTO, error) {
	f.lastTypeName, f.lastFieldKey, f.lastForce = name, key, force
	return f.typeDTO, f.err
}
func (f *fakeContentService) CreateComponent(_ context.Context, in service.CreateComponentInput) (service.ComponentDTO, error) {
	f.lastComponentCreate = in
	return f.componentDTO, f.err
}
func (f *fakeContentService) ListComponents(context.Context) ([]service.ComponentDTO, error) {
	return f.componentList, f.err
}
func (f *fakeContentService) ListComponentTemplates(context.Context) ([]service.ComponentTemplateDTO, error) {
	return f.componentTemplateList, f.err
}
func (f *fakeContentService) InstallComponentTemplate(_ context.Context, name string) (service.ComponentDTO, error) {
	f.lastTemplateName = name
	return f.componentDTO, f.err
}
func (f *fakeContentService) GetComponent(_ context.Context, name string) (service.ComponentDTO, error) {
	f.lastComponent = name
	return f.componentDTO, f.err
}
func (f *fakeContentService) UpdateComponent(_ context.Context, name string, in service.UpdateComponentInput) (service.ComponentDTO, error) {
	f.lastComponent, f.lastComponentUpdate = name, in
	return f.componentDTO, f.err
}
func (f *fakeContentService) RenameComponent(_ context.Context, name string, in service.RenameInput) (service.ComponentDTO, error) {
	f.lastComponent, f.lastRename = name, in
	return f.componentDTO, f.err
}
func (f *fakeContentService) DeleteComponent(_ context.Context, name string) error {
	f.lastComponent = name
	f.deleteComponentCall++
	return f.err
}
func (f *fakeContentService) AddComponentField(_ context.Context, name string, in service.FieldInput) (service.ComponentDTO, error) {
	f.lastComponent, f.lastComponentField = name, in
	return f.componentDTO, f.err
}
func (f *fakeContentService) UpdateComponentField(_ context.Context, name, key string, in service.UpdateFieldInput) (service.ComponentDTO, error) {
	f.lastComponent, f.lastFieldKey, f.lastUpdateField = name, key, in
	return f.componentDTO, f.err
}
func (f *fakeContentService) RenameComponentField(_ context.Context, name, key string, in service.RenameInput) (service.ComponentDTO, error) {
	f.lastComponent, f.lastFieldKey, f.lastRename = name, key, in
	return f.componentDTO, f.err
}
func (f *fakeContentService) DeleteComponentField(_ context.Context, name, key string, force bool) (service.ComponentDTO, error) {
	f.lastComponent, f.lastFieldKey, f.lastForce = name, key, force
	return f.componentDTO, f.err
}

func (f *fakeContentService) GetContentType(_ context.Context, name string) (service.ContentTypeDTO, error) {
	f.lastTypeName = name
	return f.typeDTO, f.err
}
func (f *fakeContentService) CreateEntry(_ context.Context, typeName string, payload json.RawMessage) (service.EntryDTO, error) {
	f.lastTypeName, f.lastPayload = typeName, payload
	return f.entryDTO, f.err
}
func (f *fakeContentService) ListEntries(_ context.Context, typeName string, in service.ListEntriesInput) (*service.EntryListResult, error) {
	f.lastTypeName, f.lastList = typeName, in
	return f.listRes, f.err
}
func (f *fakeContentService) GetEntry(_ context.Context, typeName string, id uuid.UUID, in service.GetEntryInput) (service.EntryDTO, error) {
	f.lastTypeName, f.lastID, f.lastGet = typeName, id, in
	return f.entryDTO, f.err
}
func (f *fakeContentService) UpdateEntry(_ context.Context, typeName string, id uuid.UUID, payload json.RawMessage, expectedVersion int) (service.EntryDTO, error) {
	f.lastTypeName, f.lastID, f.lastPayload = typeName, id, payload
	f.lastExpectedVersion = expectedVersion
	return f.entryDTO, f.err
}
func (f *fakeContentService) DeleteEntry(_ context.Context, typeName string, id uuid.UUID) error {
	f.lastTypeName, f.lastID = typeName, id
	return f.err
}
func (f *fakeContentService) CreateLocalizedEntry(_ context.Context, typeName string, in service.CreateLocalizedInput) (service.EntryDTO, error) {
	f.lastTypeName, f.lastPayload, f.lastCreate = typeName, in.Payload, in
	return f.entryDTO, f.err
}
func (f *fakeContentService) ListTranslations(_ context.Context, typeName string, id uuid.UUID) ([]service.EntryDTO, error) {
	f.lastTypeName, f.lastID = typeName, id
	return f.translations, f.err
}
func (f *fakeContentService) CreatePreviewLink(_ context.Context, typeName string, id uuid.UUID) (service.PreviewLinkDTO, error) {
	f.lastTypeName, f.lastID = typeName, id
	return f.previewLink, f.err
}
func (f *fakeContentService) SetEntryStatus(_ context.Context, typeName string, id uuid.UUID, status string, expectedVersion int) (service.EntryDTO, error) {
	f.lastTypeName, f.lastID, f.lastStatus = typeName, id, status
	f.lastExpectedVersion = expectedVersion
	return f.entryDTO, f.err
}

func (f *fakeContentService) ScheduleEntry(_ context.Context, typeName string, id uuid.UUID, in service.ScheduleEntryInput) (service.EntryScheduleDTO, error) {
	f.lastTypeName, f.lastID, f.lastSchedule = typeName, id, in
	return f.scheduleDTO, f.err
}

func (f *fakeContentService) CancelEntrySchedule(_ context.Context, typeName string, id uuid.UUID) error {
	f.lastTypeName, f.lastID = typeName, id
	f.cancelCalls++
	return f.err
}

func (f *fakeContentService) GetEntrySchedule(_ context.Context, typeName string, id uuid.UUID) (service.EntryScheduleDTO, error) {
	f.lastTypeName, f.lastID = typeName, id
	return f.scheduleDTO, f.err
}

func (f *fakeContentService) ListEntryRevisions(_ context.Context, typeName string, id uuid.UUID) ([]service.EntryRevisionDTO, error) {
	f.lastTypeName, f.lastID = typeName, id
	return f.revisions, f.err
}

func (f *fakeContentService) GetEntryRevision(_ context.Context, typeName string, id uuid.UUID, revisionNo int) (service.EntryRevisionDetailDTO, error) {
	f.lastTypeName, f.lastID, f.lastRevisionNo = typeName, id, revisionNo
	return f.revisionDetail, f.err
}

func (f *fakeContentService) DiffEntryRevision(_ context.Context, typeName string, id uuid.UUID, revisionNo int) (service.EntryRevisionDiffDTO, error) {
	f.lastTypeName, f.lastID, f.lastRevisionNo = typeName, id, revisionNo
	return f.revisionDiff, f.err
}

func (f *fakeContentService) RestoreEntryRevision(_ context.Context, typeName string, id uuid.UUID, in service.RestoreEntryInput, expectedVersion int) (service.RestoreEntryResultDTO, error) {
	f.lastTypeName, f.lastID = typeName, id
	f.lastRestore, f.lastRestoreIfMatch = in, expectedVersion
	return f.restoreResult, f.err
}

func (f *fakeContentService) RequestChanges(_ context.Context, typeName string, id uuid.UUID, in service.RequestChangesInput, expectedVersion int) (service.ReviewDecisionDTO, error) {
	f.lastTypeName, f.lastID = typeName, id
	f.lastRequestChanges, f.lastRequestChangesIfMatch = in, expectedVersion
	return f.reviewDecisionDTO, f.err
}

func (f *fakeContentService) ListEntryReviewDecisions(_ context.Context, typeName string, id uuid.UUID) ([]service.ReviewDecisionDTO, error) {
	f.lastTypeName, f.lastID = typeName, id
	return f.reviewDecisions, f.err
}

func (f *fakeContentService) CreateMediaUpload(_ context.Context, in service.CreateMediaUploadInput) (service.MediaUploadDTO, error) {
	f.lastContentType = in.ContentType
	f.lastMediaCreate = in
	return f.mediaUpload, f.err
}
func (f *fakeContentService) UpdateMediaAsset(_ context.Context, id uuid.UUID, in service.UpdateMediaAssetInput) (service.MediaAssetDTO, error) {
	f.lastID = id
	f.lastMediaPatch = in
	return f.mediaAsset, f.err
}
func (f *fakeContentService) CompleteMediaUpload(_ context.Context, id uuid.UUID) (service.MediaAssetDTO, error) {
	f.lastID = id
	return f.mediaAsset, f.err
}
func (f *fakeContentService) GetMediaAsset(_ context.Context, id uuid.UUID) (service.MediaAssetDTO, error) {
	f.lastID = id
	return f.mediaAsset, f.err
}
func (f *fakeContentService) ListMediaAssets(_ context.Context, in service.ListMediaInput) (service.MediaListResult, error) {
	f.lastMediaList = in
	return f.mediaList, f.err
}
func (f *fakeContentService) ResolveMediaURL(_ context.Context, id uuid.UUID, preset string) (string, time.Time, error) {
	f.lastID = id
	f.lastPreset = preset
	if !f.mediaExpires.IsZero() {
		return f.mediaURL, f.mediaExpires, f.err
	}
	return f.mediaURL, time.Now().Add(5 * time.Minute), f.err
}
func (f *fakeContentService) DeleteMediaAsset(_ context.Context, id uuid.UUID, force bool) error {
	f.lastID = id
	f.lastForceDelete = force
	return f.err
}
func (f *fakeContentService) SweepOrphans(_ context.Context, in service.SweepInput) (service.SweepResult, error) {
	f.lastSweep = in
	return f.sweepResult, f.err
}
func (f *fakeContentService) EnqueueMediaVariants(_ context.Context, id uuid.UUID) (service.MediaAssetDTO, error) {
	f.lastID = id
	f.enqueueCalls++
	return f.mediaAsset, f.err
}
func (f *fakeContentService) EntryReferencedBy(_ context.Context, typeName string, id uuid.UUID, limit, offset int) (service.ReferencedByDTO, error) {
	f.lastTypeName, f.lastID = typeName, id
	f.lastRefLimit, f.lastRefOffset = limit, offset
	return f.referencedBy, f.err
}
func (f *fakeContentService) MediaReferencedBy(_ context.Context, id uuid.UUID, limit, offset int) (service.ReferencedByDTO, error) {
	f.lastID = id
	f.lastRefLimit, f.lastRefOffset = limit, offset
	return f.referencedBy, f.err
}

func (f *fakeContentService) Usage(context.Context) (service.UsageDTO, error) {
	return service.UsageDTO{Plan: "free"}, f.err
}

func (f *fakeContentService) ListActivity(_ context.Context, in service.ListActivityInput) ([]service.ActivityDTO, error) {
	f.lastActivityInput = in
	return f.activity, f.err
}

func (f *fakeContentService) ListPendingReview(_ context.Context, in service.ListPendingReviewInput) ([]service.PendingEntryDTO, error) {
	f.lastPendingInput = in
	return f.pending, f.err
}

func (f *fakeContentService) Search(_ context.Context, in service.SearchInput) ([]service.SearchResultDTO, error) {
	f.lastSearchInput = in
	return f.search, f.err
}

func (f *fakeContentService) ListLocales(_ context.Context, in service.ListLocalesInput) ([]service.LocaleDTO, error) {
	f.lastLocalesInput = in
	return f.locales, f.err
}

func (f *fakeContentService) EntryFieldAttribution(_ context.Context, typeName string, id uuid.UUID) (service.EntryAttributionDTO, error) {
	f.lastAttributionType, f.lastAttributionID = typeName, id
	return f.attribution, f.err
}

func (f *fakeContentService) CreateWebhook(_ context.Context, in service.CreateWebhookInput) (service.WebhookCreatedDTO, error) {
	f.lastWebhookCreate = in
	return f.webhookCreated, f.err
}

func (f *fakeContentService) ListWebhooks(context.Context) ([]service.WebhookDTO, error) {
	return f.webhookList, f.err
}

func (f *fakeContentService) DeleteWebhook(_ context.Context, id uuid.UUID) error {
	f.lastID = id
	return f.err
}

func router(svc service.ContentService) http.Handler {
	r := chi.NewRouter()
	NewHandler(svc).Routes(r)
	return r
}

func do(t *testing.T, svc service.ContentService, method, target string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Buffer
	if body == "" {
		rdr = bytes.NewBuffer(nil)
	} else {
		rdr = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	rec := httptest.NewRecorder()
	router(svc).ServeHTTP(rec, req)
	return rec
}

func TestCreateType_OK(t *testing.T) {
	svc := &fakeContentService{typeDTO: service.ContentTypeDTO{ID: uuid.New(), Name: "post"}}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/types", `{"name":"post","label":"Post"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestCreateType_InvalidJSON(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodPost, "/api/v1/content/types", `{bad`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestCreateType_ServiceError(t *testing.T) {
	svc := &fakeContentService{err: apperrors.New("CONFLICT", "exists", 409)}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/types", `{"name":"post"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListTypes_OK(t *testing.T) {
	svc := &fakeContentService{typesList: []service.ContentTypeDTO{{Name: "a"}, {Name: "b"}}}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/types", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestGetType_OK(t *testing.T) {
	svc := &fakeContentService{typeDTO: service.ContentTypeDTO{Name: "post"}}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/types/post", "")
	if rec.Code != http.StatusOK || svc.lastTypeName != "post" {
		t.Fatalf("code=%d name=%s", rec.Code, svc.lastTypeName)
	}
}

func TestAddField_OK(t *testing.T) {
	svc := &fakeContentService{typeDTO: service.ContentTypeDTO{Name: "post"}}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/types/post/fields", `{"key":"title","type":"string"}`)
	if rec.Code != http.StatusCreated || svc.lastTypeName != "post" {
		t.Fatalf("code=%d name=%s", rec.Code, svc.lastTypeName)
	}
}

func TestAddField_InvalidJSON(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodPost, "/api/v1/content/types/post/fields", `{`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestCreateEntry_OK(t *testing.T) {
	svc := &fakeContentService{entryDTO: adminEntry(uuid.New(), "post", "")}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/entries?type=post", `{"title":"hi"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastTypeName != "post" || string(svc.lastPayload) != `{"title":"hi"}` {
		t.Fatalf("type=%s payload=%s", svc.lastTypeName, svc.lastPayload)
	}
}

// ADR-013 §9: the key travels in a header, and the handler is the only thing
// that can carry it from there into the input. A test that posted through `do`
// could not see this at all — `do` sets no headers, so every existing create
// test passes with the field left unwired.
func TestCreateEntry_ForwardsIdempotencyKeyHeader(t *testing.T) {
	svc := &fakeContentService{entryDTO: adminEntry(uuid.New(), "post", "")}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/content/entries?type=post",
		bytes.NewBufferString(`{"title":"hi"}`))
	req.Header.Set("Idempotency-Key", "retry-key-0001")
	rec := httptest.NewRecorder()
	router(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastCreate.IdempotencyKey != "retry-key-0001" {
		t.Fatalf("the key never reached the service: %q", svc.lastCreate.IdempotencyKey)
	}
}

// The absent header must arrive as the empty string — "no promise" — rather than
// as anything the service could mistake for a key.
func TestCreateEntry_NoIdempotencyKeyHeaderMeansNoPromise(t *testing.T) {
	svc := &fakeContentService{entryDTO: adminEntry(uuid.New(), "post", "")}
	do(t, svc, http.MethodPost, "/api/v1/content/entries?type=post", `{"title":"hi"}`)
	if svc.lastCreate.IdempotencyKey != "" {
		t.Fatalf("got a key out of nowhere: %q", svc.lastCreate.IdempotencyKey)
	}
}

func TestCreateEntry_MissingType(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodPost, "/api/v1/content/entries", `{"a":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestCreateEntry_PayloadNotObject(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodPost, "/api/v1/content/entries?type=post", `[1,2,3]`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListEntries_OK(t *testing.T) {
	total := 1
	svc := &fakeContentService{listRes: &service.EntryListResult{Items: []service.EntryDTO{adminEntry(uuid.New(), "post", "")}, Total: &total}}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/entries?type=post&limit=5&offset=2&sort=title:asc&filter=a:eq:1&filter=b:eq:2&cursor=abc", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	if svc.lastList.Cursor != "abc" {
		t.Fatalf("cursor not forwarded to the service: %q", svc.lastList.Cursor)
	}
	if svc.lastList.Limit != 5 || svc.lastList.Offset != 2 || svc.lastList.Sort != "title:asc" || len(svc.lastList.Filters) != 2 {
		t.Fatalf("parsed list input wrong: %+v", svc.lastList)
	}
}

func TestListEntries_MissingType(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodGet, "/api/v1/content/entries", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestGetEntry_OK(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{entryDTO: adminEntry(id, "post", "")}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/entries/"+id.String()+"?type=post", "")
	if rec.Code != http.StatusOK || svc.lastID != id {
		t.Fatalf("code=%d id=%s", rec.Code, svc.lastID)
	}
}

func TestGetEntry_InvalidID(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodGet, "/api/v1/content/entries/not-a-uuid?type=post", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestGetEntry_MissingType(t *testing.T) {
	id := uuid.New()
	rec := do(t, &fakeContentService{}, http.MethodGet, "/api/v1/content/entries/"+id.String(), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestGetEntry_NotFound(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{err: apperrors.ErrNotFound}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/entries/"+id.String()+"?type=post", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
}

// The release screen's per-field attribution (ADR-014 §6, step 4). What is
// checked here is the WIRE SHAPE: a key that has no author must be absent from
// the object rather than present with an empty actor, because absence is what
// the console turns into "unknown" and an empty actor would render as a
// half-written fact.
func TestEntryAttribution_OK(t *testing.T) {
	id := uuid.New()
	principal := uuid.New()
	agentID := "content-bot"
	svc := &fakeContentService{attribution: service.EntryAttributionDTO{
		Fields: map[string]service.FieldAuthorDTO{
			"title": {ActorKind: "human", ActorUserID: &principal},
			"body":  {ActorKind: "agent", ActorUserID: &principal, ActorAgentID: &agentID},
		},
	}}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/entries/"+id.String()+"/attribution?type=post", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastAttributionID != id || svc.lastAttributionType != "post" {
		t.Fatalf("forwarded type=%q id=%s", svc.lastAttributionType, svc.lastAttributionID)
	}
	var env struct {
		Data struct {
			Fields map[string]map[string]any `json:"fields"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v — body=%s", err, rec.Body)
	}
	if got := env.Data.Fields["body"]["actor_agent_id"]; got != agentID {
		t.Fatalf("agent line must name the agent: %#v", env.Data.Fields["body"])
	}
	if got, ok := env.Data.Fields["body"]["actor_user_id"]; !ok || got != principal.String() {
		t.Fatalf("agent line must also name the principal who answers: %#v", env.Data.Fields["body"])
	}
	if _, present := env.Data.Fields["summary"]; present {
		t.Fatalf("an unattributed field must be ABSENT, not an empty actor: %#v", env.Data.Fields)
	}
}

func TestEntryAttribution_MissingType(t *testing.T) {
	id := uuid.New()
	rec := do(t, &fakeContentService{}, http.MethodGet, "/api/v1/content/entries/"+id.String()+"/attribution", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestUpdateEntry_OK(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{entryDTO: adminEntry(id, "post", "")}
	rec := do(t, svc, http.MethodPatch, "/api/v1/content/entries/"+id.String()+"?type=post", `{"title":"x"}`)
	if rec.Code != http.StatusOK || svc.lastID != id {
		t.Fatalf("code=%d id=%s", rec.Code, svc.lastID)
	}
}

func TestUpdateEntry_InvalidID(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodPatch, "/api/v1/content/entries/bad?type=post", `{"a":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestUpdateEntry_IfMatchThreadedToService(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{entryDTO: adminEntry(id, "post", "")}
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/content/entries/"+id.String()+"?type=post", bytes.NewBufferString(`{"title":"x"}`))
	req.Header.Set("If-Match", `"3"`) // ETag-style quoting accepted
	rec := httptest.NewRecorder()
	router(svc).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	if svc.lastExpectedVersion != 3 {
		t.Fatalf("expected version 3 threaded to service, got %d", svc.lastExpectedVersion)
	}
}

func TestUpdateEntry_InvalidIfMatch(t *testing.T) {
	id := uuid.New()
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/content/entries/"+id.String()+"?type=post", bytes.NewBufferString(`{"a":1}`))
	req.Header.Set("If-Match", "not-a-number")
	rec := httptest.NewRecorder()
	router(&fakeContentService{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestUpdateEntry_PayloadNotObject(t *testing.T) {
	id := uuid.New()
	rec := do(t, &fakeContentService{}, http.MethodPatch, "/api/v1/content/entries/"+id.String()+"?type=post", `"str"`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestDeleteEntry_OK(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodDelete, "/api/v1/content/entries/"+id.String()+"?type=post", "")
	if rec.Code != http.StatusNoContent || svc.lastID != id {
		t.Fatalf("code=%d id=%s", rec.Code, svc.lastID)
	}
}

func TestDeleteEntry_MissingType(t *testing.T) {
	id := uuid.New()
	rec := do(t, &fakeContentService{}, http.MethodDelete, "/api/v1/content/entries/"+id.String(), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestDeleteEntry_ServiceError(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{err: errors.New("boom")}
	rec := do(t, svc, http.MethodDelete, "/api/v1/content/entries/"+id.String()+"?type=post", "")
	if rec.Code < 400 {
		t.Fatalf("expected error status, code=%d", rec.Code)
	}
}

func TestPublishEntry_PassesPublishedStatus(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{entryDTO: adminEntry(id, "post", domain.StatusPublished)}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/entries/"+id.String()+"/publish?type=post", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastStatus != domain.StatusPublished {
		t.Fatalf("status=%q want %q", svc.lastStatus, domain.StatusPublished)
	}
	if svc.lastID != id {
		t.Fatalf("id=%s want %s", svc.lastID, id)
	}
}

func TestUnpublishEntry_PassesDraftStatus(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{entryDTO: adminEntry(id, "post", domain.StatusDraft)}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/entries/"+id.String()+"/unpublish?type=post", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastStatus != domain.StatusDraft {
		t.Fatalf("status=%q want %q", svc.lastStatus, domain.StatusDraft)
	}
}

// If-Match must reach the service on a publish too, or a publish could land on
// top of a concurrent content edit without the caller noticing.
func TestPublishEntry_ForwardsIfMatch(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{entryDTO: adminEntry(id, "post", "")}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/content/entries/"+id.String()+"/publish?type=post", nil)
	req.Header.Set("If-Match", `"7"`)
	rec := httptest.NewRecorder()
	router(svc).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastExpectedVersion != 7 {
		t.Fatalf("expectedVersion=%d want 7", svc.lastExpectedVersion)
	}
}

func TestPublishEntry_MissingType(t *testing.T) {
	id := uuid.New()
	rec := do(t, &fakeContentService{}, http.MethodPost, "/api/v1/content/entries/"+id.String()+"/publish", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListEntries_ForwardsStatusQuery(t *testing.T) {
	svc := &fakeContentService{listRes: &service.EntryListResult{}}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/entries?type=post&status=published", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastList.Status != domain.StatusPublished {
		t.Fatalf("status=%q want %q", svc.lastList.Status, domain.StatusPublished)
	}
}

// TestListEntries_ForwardsQuery is ADR-021's `q` parameter: the handler must
// forward it to ListEntriesInput.Query verbatim, the same way it already
// forwards status and filter.
func TestListEntries_ForwardsQuery(t *testing.T) {
	svc := &fakeContentService{listRes: &service.EntryListResult{}}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/entries?type=post&q=unicorn+ranch", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastList.Query != "unicorn ranch" {
		t.Fatalf("query=%q want %q", svc.lastList.Query, "unicorn ranch")
	}
}

// --- cross-type search (ADR-021 §3) ------------------------------------------

// TestSearchContent_OK proves the route reaches searchContent, forwards every
// query parameter to service.SearchInput, and renders the result under
// "items" — the shape the endpoint's contract promises.
func TestSearchContent_OK(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{search: []service.SearchResultDTO{
		{ID: id, ContentType: "post", Locale: "default", Status: domain.StatusDraft, Snippet: "a ranch nearby"},
	}}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/search?q=ranch&locale=default&status=draft&limit=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastSearchInput.Query != "ranch" || svc.lastSearchInput.Locale != "default" ||
		svc.lastSearchInput.Status != domain.StatusDraft || svc.lastSearchInput.Limit != 10 {
		t.Fatalf("search input not forwarded intact: %+v", svc.lastSearchInput)
	}
	var got struct {
		Data struct {
			Items []service.SearchResultDTO `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Data.Items) != 1 || got.Data.Items[0].ID != id || got.Data.Items[0].ContentType != "post" {
		t.Fatalf("items = %+v", got.Data.Items)
	}
}

// TestSearchContent_ErrorPropagates. The service is where `q`'s validation
// lives (required, 200-char cap, status/locale checks) — the handler's job is
// only to relay whatever apperrors code comes back.
func TestSearchContent_ErrorPropagates(t *testing.T) {
	svc := &fakeContentService{err: apperrors.New("CONTENT_SEARCH_QUERY_REQUIRED", "q is required", 400)}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/search", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// --- locale inventory (T2 2.3) -----------------------------------------------

// TestListLocales_OK proves the route reaches listLocales, forwards `type`,
// and renders the result under "items" — the same shape searchContent uses.
func TestListLocales_OK(t *testing.T) {
	svc := &fakeContentService{locales: []service.LocaleDTO{
		{Locale: "default", Entries: 3}, {Locale: "fr", Entries: 1},
	}}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/locales?type=post", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastLocalesInput.Type != "post" {
		t.Fatalf("type not forwarded to the service: %+v", svc.lastLocalesInput)
	}
	var got struct {
		Data struct {
			Items []service.LocaleDTO `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Data.Items) != 2 || got.Data.Items[0].Locale != "default" || got.Data.Items[0].Entries != 3 {
		t.Fatalf("items = %+v", got.Data.Items)
	}
}

// TestListLocales_NoTypeMeansEveryType. Omitting `type` must arrive at the
// service as "", the same "unconfined" convention listEntries' own `locale`
// query uses — not as a zero value the handler mistakes for an error.
func TestListLocales_NoTypeMeansEveryType(t *testing.T) {
	svc := &fakeContentService{locales: []service.LocaleDTO{}}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/locales", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastLocalesInput.Type != "" {
		t.Fatalf("expected no type filter, got %q", svc.lastLocalesInput.Type)
	}
}

// TestListLocales_ErrorPropagates. Authorization and type-lookup errors live
// in the service (ActionContentList, GetContentTypeByName) — the handler's
// job is only to relay whatever apperrors code comes back, matching
// TestSearchContent_ErrorPropagates.
func TestListLocales_ErrorPropagates(t *testing.T) {
	svc := &fakeContentService{err: apperrors.ErrForbidden}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/locales", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// --- copy_from_source (T2 2.3) parsing on POST /entries ---------------------

// TestCreateEntry_ParsesCopyFromSource proves ?copy_from_source=true reaches
// CreateLocalizedInput.CopyFromSource, alongside translation_of — the pair the
// service needs to seed a translation from its source's working copy.
func TestCreateEntry_ParsesCopyFromSource(t *testing.T) {
	srcID := uuid.New()
	svc := &fakeContentService{entryDTO: adminEntry(uuid.New(), "post", "zh-TW")}
	rec := do(t, svc, http.MethodPost,
		"/api/v1/content/entries?type=post&locale=zh-TW&translation_of="+srcID.String()+"&copy_from_source=true",
		`{"title":"翻譯標題"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastCreate.TranslationOf == nil || *svc.lastCreate.TranslationOf != srcID {
		t.Fatalf("translation_of not forwarded: %+v", svc.lastCreate)
	}
	if !svc.lastCreate.CopyFromSource {
		t.Fatalf("copy_from_source=true did not reach the service: %+v", svc.lastCreate)
	}
}

// TestCreateEntry_CopyFromSourceOmittedMeansFalse. No implicit behaviour: an
// ordinary create (or an ordinary translation) without the parameter must
// arrive at the service with CopyFromSource false, exactly as it did before
// this parameter existed.
func TestCreateEntry_CopyFromSourceOmittedMeansFalse(t *testing.T) {
	svc := &fakeContentService{entryDTO: adminEntry(uuid.New(), "post", "")}
	do(t, svc, http.MethodPost, "/api/v1/content/entries?type=post", `{"title":"hi"}`)
	if svc.lastCreate.CopyFromSource {
		t.Fatalf("copy_from_source turned on out of nowhere: %+v", svc.lastCreate)
	}
}

// TestCreateEntry_CopyFromSourceInvalidValue. A non-boolean value is a caller
// error, not something the handler coerces or silently ignores.
func TestCreateEntry_CopyFromSourceInvalidValue(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodPost,
		"/api/v1/content/entries?type=post&copy_from_source=notabool", `{"title":"hi"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// --- media metadata (migration 000022) --------------------------------------

// The reservation endpoint predates the request body: every caller today sends
// ?content_type= and nothing else. Those callers must keep working unchanged, so
// the body is optional and an absent one is not a 400.
func TestCreateMediaUpload_QueryOnlyStillWorks(t *testing.T) {
	svc := &fakeContentService{mediaUpload: service.MediaUploadDTO{AssetID: uuid.New()}}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/media?content_type=image/png", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastContentType != "image/png" {
		t.Fatalf("content_type=%q want image/png", svc.lastContentType)
	}
}

// A body may carry the new declared metadata alongside the type. When both name
// a content type the body wins — it is the more specific, more recent surface,
// and a caller that bothered to send a document meant what it said.
func TestCreateMediaUpload_BodyOverridesQueryAndCarriesMetadata(t *testing.T) {
	svc := &fakeContentService{mediaUpload: service.MediaUploadDTO{AssetID: uuid.New()}}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/media?content_type=image/png",
		`{"content_type":"image/webp","filename":"duck.webp","alt_text":"","width_px":800,"height_px":600}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastContentType != "image/webp" {
		t.Fatalf("content_type=%q want image/webp (the body is the more specific answer)", svc.lastContentType)
	}
	in := svc.lastMediaCreate
	if in.Filename.Value == nil || *in.Filename.Value != "duck.webp" {
		t.Fatalf("filename did not reach the service: %+v", in.Filename)
	}
	if !in.AltText.Set || in.AltText.Value == nil || *in.AltText.Value != "" {
		t.Fatalf(`empty alt_text must arrive as a VALUE, not as an absence: %+v`, in.AltText)
	}
	if in.WidthPx.Value == nil || *in.WidthPx.Value != 800 {
		t.Fatalf("width_px did not reach the service: %+v", in.WidthPx)
	}
}

// An empty body is forgiven; a malformed one is not. "Unreadable" must never
// quietly degrade into "absent", or a caller's typo becomes a silently dropped
// field and the 201 says everything went fine.
func TestCreateMediaUpload_MalformedBodyIsStillRejected(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodPost, "/api/v1/content/media?content_type=image/png", `{bad`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	rec = do(t, &fakeContentService{}, http.MethodPost, "/api/v1/content/media?content_type=image/png", `{"nope":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an unknown key must be a 400, not a dropped field: code=%d body=%s", rec.Code, rec.Body)
	}
}

// The three JSON states have to survive the whole handler, not merely the
// decoder: absent, explicit null and a value must reach the service distinct.
func TestUpdateMediaAsset_ForwardsTheThreeStates(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{mediaAsset: adminMedia(id)}
	rec := do(t, svc, http.MethodPatch, "/api/v1/content/media/"+id.String(),
		`{"alt_text":null,"width_px":800,"height_px":600}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastID != id {
		t.Fatalf("id=%s want %s", svc.lastID, id)
	}
	in := svc.lastMediaPatch
	if in.Filename.Set {
		t.Fatal("a key that was never sent must not arrive Set — that is how a patch blanks a field nobody touched")
	}
	if !in.AltText.Set || in.AltText.Value != nil {
		t.Fatalf("an explicit null must arrive as Set with a nil value: %+v", in.AltText)
	}
	if in.WidthPx.Value == nil || *in.WidthPx.Value != 800 {
		t.Fatalf("width_px did not reach the service: %+v", in.WidthPx)
	}
}

// No If-Match on this route: metadata is not the optimistically-locked payload,
// and the header must not be silently honoured here by inheritance either.
func TestUpdateMediaAsset_NoVersionPrecondition(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{mediaAsset: adminMedia(id)}
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/content/media/"+id.String(),
		bytes.NewBufferString(`{"alt_text":"x"}`))
	req.Header.Set("If-Match", `"7"`)
	rec := httptest.NewRecorder()
	router(svc).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("an If-Match must be ignored, not rejected: code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastExpectedVersion != 0 {
		t.Fatalf("expectedVersion=%d — this route must not carry a version precondition", svc.lastExpectedVersion)
	}
}

func TestUpdateMediaAsset_InvalidID(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodPatch, "/api/v1/content/media/not-a-uuid", `{"alt_text":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

// --- schema mutation ---------------------------------------------------------
//
// The handler's whole job on these six routes is plumbing: which URL params reach
// the service, whether ?force= was consent, and which status code the shape of
// the result earns. The policy lives in the service and the data migration in the
// repository, so nothing here asserts either — a handler test that "proves" a
// refusal only proves the fake was configured to return an error.

// Renaming is a POST to its own sub-resource, not a property on the PATCH. If the
// two ever collapsed into one route this is what would notice.
func TestSchemaMutation_RoutesAreDistinctVerbs(t *testing.T) {
	cases := []struct {
		name   string
		method string
		target string
		body   string
		code   int
	}{
		{"patch type", http.MethodPatch, "/api/v1/content/types/post", `{"label":"Post"}`, http.StatusOK},
		{"rename type", http.MethodPost, "/api/v1/content/types/post/rename", `{"name":"article"}`, http.StatusOK},
		{"delete type", http.MethodDelete, "/api/v1/content/types/post", "", http.StatusNoContent},
		{"patch field", http.MethodPatch, "/api/v1/content/types/post/fields/title", `{"label":"Title"}`, http.StatusOK},
		{"rename field", http.MethodPost, "/api/v1/content/types/post/fields/title/rename", `{"key":"headline"}`, http.StatusOK},
		{"delete field", http.MethodDelete, "/api/v1/content/types/post/fields/title", "", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeContentService{typeDTO: service.ContentTypeDTO{Name: "post"}}
			rec := do(t, svc, tc.method, tc.target, tc.body)
			if rec.Code != tc.code {
				t.Fatalf("code=%d want %d body=%s", rec.Code, tc.code, rec.Body)
			}
			if svc.lastTypeName != "post" {
				t.Fatalf("type name did not reach the service: %q", svc.lastTypeName)
			}
		})
	}
}

// Deleting a FIELD returns 200 with the type, because the resource that changed
// still exists and the caller needs its new state; deleting a TYPE returns 204,
// because there is nothing left to describe. Getting these the same way round
// would either hand back a document for a resource that is gone or force a second
// GET after every field delete.
func TestDeleteShapes_FieldReturnsTypeAndTypeReturnsNothing(t *testing.T) {
	t.Run("field delete is 200 with the type DTO", func(t *testing.T) {
		svc := &fakeContentService{typeDTO: service.ContentTypeDTO{
			Name:   "post",
			Fields: []service.FieldDTO{{Key: "title", Type: domain.FieldTypeString}},
		}}
		rec := do(t, svc, http.MethodDelete, "/api/v1/content/types/post/fields/body", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		var env struct {
			Data service.ContentTypeDTO `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("body is not the success envelope: %v (%s)", err, rec.Body)
		}
		if env.Data.Name != "post" || len(env.Data.Fields) != 1 {
			t.Fatalf("the surviving schema must come back: %+v", env.Data)
		}
	})
	t.Run("type delete is 204 carrying no resource", func(t *testing.T) {
		svc := &fakeContentService{typeDTO: service.ContentTypeDTO{Name: "post"}}
		rec := do(t, svc, http.MethodDelete, "/api/v1/content/types/post", "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("code=%d", rec.Code)
		}
		// Asserted against the EXISTING delete routes rather than against "no
		// bytes at all": response.JSON always writes the envelope, so every 204 in
		// this codebase carries `data: null`. That is a platform-wide convention
		// (see deleteEntry / deleteMediaAsset / the ticket handler), and this
		// route matching it is the property worth pinning here — a divergence
		// would be the bug, not the shared shape.
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("body is not the success envelope: %v (%s)", err, rec.Body)
		}
		if s := string(env.Data); s != "null" && s != "" {
			t.Fatalf("a 204 must not describe a resource, got data=%s", s)
		}

		id := uuid.New()
		peer := do(t, &fakeContentService{}, http.MethodDelete, "/api/v1/content/entries/"+id.String()+"?type=post", "")
		if peer.Code != rec.Code || peer.Body.Len() != rec.Body.Len() {
			t.Fatalf("type delete (%d, %d bytes) diverges from entry delete (%d, %d bytes)",
				rec.Code, rec.Body.Len(), peer.Code, peer.Body.Len())
		}
	})
}

// The {key} param is a second path variable on routes that already have {name}.
// Getting the two crossed is the kind of bug that renames the wrong thing and
// still returns 200.
func TestFieldRoutes_PlumbBothURLParams(t *testing.T) {
	t.Run("patch", func(t *testing.T) {
		svc := &fakeContentService{typeDTO: service.ContentTypeDTO{Name: "post"}}
		rec := do(t, svc, http.MethodPatch, "/api/v1/content/types/post/fields/body",
			`{"label":"Body","required":true,"enum_values":["a","b"]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		if svc.lastTypeName != "post" || svc.lastFieldKey != "body" {
			t.Fatalf("name=%q key=%q — the two path params are crossed or dropped", svc.lastTypeName, svc.lastFieldKey)
		}
		in := svc.lastUpdateField
		if in.Label == nil || *in.Label != "Body" {
			t.Fatalf("label did not reach the service: %+v", in.Label)
		}
		if in.Required == nil || !*in.Required {
			t.Fatalf("required did not reach the service: %+v", in.Required)
		}
		if in.EnumValues == nil || len(*in.EnumValues) != 2 {
			t.Fatalf("enum_values did not reach the service: %+v", in.EnumValues)
		}
		// Absent keys must stay absent. A pointer that arrived non-nil for a key
		// nobody sent would make every PATCH a full overwrite.
		if in.Type != nil || in.Multiple != nil || in.RelationEntity != nil {
			t.Fatalf("keys that were never sent must arrive nil: %+v", in)
		}
	})
	t.Run("rename", func(t *testing.T) {
		svc := &fakeContentService{typeDTO: service.ContentTypeDTO{Name: "post"}}
		rec := do(t, svc, http.MethodPost, "/api/v1/content/types/post/fields/body/rename", `{"key":"content"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		if svc.lastFieldKey != "body" {
			t.Fatalf("the field being renamed must come from the URL, got %q", svc.lastFieldKey)
		}
		if svc.lastRename.Key != "content" {
			t.Fatalf("the NEW key must come from the body, got %q", svc.lastRename.Key)
		}
	})
	t.Run("delete", func(t *testing.T) {
		svc := &fakeContentService{typeDTO: service.ContentTypeDTO{Name: "post"}}
		if rec := do(t, svc, http.MethodDelete, "/api/v1/content/types/post/fields/body", ""); rec.Code != http.StatusOK {
			t.Fatalf("code=%d", rec.Code)
		}
		if svc.lastFieldKey != "body" {
			t.Fatalf("key=%q", svc.lastFieldKey)
		}
	})
}

// The immutable properties are DECLARED on UpdateFieldInput so the service can
// refuse them by name. That only works if the decoder actually forwards them —
// a handler that dropped them would turn a named 422 into a silent no-op, which
// is the outcome ADR-006 Am.1b ruled against.
func TestUpdateField_ForwardsImmutablePropsSoTheServiceCanRefuseThemByName(t *testing.T) {
	svc := &fakeContentService{typeDTO: service.ContentTypeDTO{Name: "post"}}
	rec := do(t, svc, http.MethodPatch, "/api/v1/content/types/post/fields/body",
		`{"type":"number","multiple":true,"relation_entity":"author"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	in := svc.lastUpdateField
	if in.Type == nil || *in.Type != "number" {
		t.Fatalf("`type` must reach the service to be refused BY NAME: %+v", in.Type)
	}
	if in.Multiple == nil || !*in.Multiple {
		t.Fatalf("`multiple` must reach the service: %+v", in.Multiple)
	}
	if in.RelationEntity == nil || *in.RelationEntity != "author" {
		t.Fatalf("`relation_entity` must reach the service: %+v", in.RelationEntity)
	}
}

// Anything other than the literal "true" is not consent. Accepting "1", "yes" or
// a bare ?force would make a typo in a script look like an approval for an
// operation that rewrites every entry of the type.
func TestDeleteField_ForceParsing(t *testing.T) {
	cases := []struct {
		query string
		want  bool
	}{
		{"", false},
		{"?force=true", true},
		{"?force=false", false},
		{"?force=1", false},
		{"?force=yes", false},
		{"?force=TRUE", false},
		{"?force=True", false},
		{"?force", false},
		{"?force=", false},
		{"?force=true&force=false", true}, // first value wins, and it said true
	}
	for _, tc := range cases {
		t.Run("force"+tc.query, func(t *testing.T) {
			svc := &fakeContentService{typeDTO: service.ContentTypeDTO{Name: "post"}}
			rec := do(t, svc, http.MethodDelete, "/api/v1/content/types/post/fields/body"+tc.query, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			if svc.lastForce != tc.want {
				t.Fatalf("force=%v want %v for query %q", svc.lastForce, tc.want, tc.query)
			}
		})
	}
}

func TestSchemaMutation_InvalidJSONIs400(t *testing.T) {
	for _, tc := range []struct{ method, target string }{
		{http.MethodPatch, "/api/v1/content/types/post"},
		{http.MethodPost, "/api/v1/content/types/post/rename"},
		{http.MethodPatch, "/api/v1/content/types/post/fields/body"},
		{http.MethodPost, "/api/v1/content/types/post/fields/body/rename"},
	} {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			if rec := do(t, &fakeContentService{}, tc.method, tc.target, `{bad`); rec.Code != http.StatusBadRequest {
				t.Fatalf("code=%d", rec.Code)
			}
		})
	}
}

// The service's codes are what the client keys off; the handler must render them
// rather than flattening every schema failure into one status.
func TestSchemaMutation_ServiceErrorStatusIsPreserved(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		method string
		target string
		body   string
		code   int
	}{
		{"field has data", apperrors.New("CONTENT_FIELD_HAS_DATA", "x", 409),
			http.MethodDelete, "/api/v1/content/types/post/fields/body", "", http.StatusConflict},
		{"type immutable", apperrors.New("CONTENT_FIELD_TYPE_IMMUTABLE", "x", 422),
			http.MethodPatch, "/api/v1/content/types/post/fields/body", `{"type":"number"}`, http.StatusUnprocessableEntity},
		{"type has entries", apperrors.New("CONTENT_TYPE_HAS_ENTRIES", "x", 409),
			http.MethodDelete, "/api/v1/content/types/post", "", http.StatusConflict},
		{"unknown field", apperrors.ErrNotFound,
			http.MethodPost, "/api/v1/content/types/post/fields/body/rename", `{"key":"c"}`, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, &fakeContentService{err: tc.err}, tc.method, tc.target, tc.body)
			if rec.Code != tc.code {
				t.Fatalf("code=%d want %d body=%s", rec.Code, tc.code, rec.Body)
			}
		})
	}
}

// A failed delete must not answer 204: the route writes the status only after the
// service returns nil, or a refusal reads as a success to every client.
func TestDeleteType_FailureIsNot204(t *testing.T) {
	rec := do(t, &fakeContentService{err: apperrors.New("CONTENT_TYPE_HAS_ENTRIES", "x", 409)},
		http.MethodDelete, "/api/v1/content/types/post", "")
	if rec.Code == http.StatusNoContent {
		t.Fatal("a refused delete answered 204 — the client would believe the type is gone")
	}
}

func (f *fakeContentService) ExportSchema(context.Context) (domain.Artifact, error) {
	return f.artifact, f.err
}

// Plan and apply record WHICH of them ran, not just their arguments: the two
// routes differ only in the method they dispatch to, so a swapped wiring would
// let a plan request write the schema and every argument assertion would still
// pass.
func (f *fakeContentService) PlanSchema(_ context.Context, art domain.Artifact, prune bool) (service.PlanResult, error) {
	f.planCalls++
	f.lastArtifact, f.lastPrune = art, prune
	return f.plan, f.err
}

func (f *fakeContentService) ApplySchema(_ context.Context, art domain.Artifact, prune bool) (service.PlanResult, error) {
	f.applyCalls++
	f.lastArtifact, f.lastPrune = art, prune
	return f.plan, f.err
}

// --- schema proposals (ADR-013 §3 step 8) ------------------------------------

func (f *fakeContentService) ProposeSchema(_ context.Context, art domain.Artifact, prune bool) (service.SchemaProposalDTO, error) {
	f.proposeCalls++
	f.lastArtifact, f.lastPrune = art, prune
	return f.proposal, f.err
}

func (f *fakeContentService) ListSchemaProposals(_ context.Context) ([]service.SchemaProposalDTO, error) {
	f.queueListCalls++
	return f.proposals, f.err
}

// ownProposalListCalls is counted for the reason ownProposalCalls is: the two
// list routes differ only in a path segment, and a handler wired to the queue
// would still answer 200 with an enveloped body. Which METHOD was reached is
// the whole assertion.
func (f *fakeContentService) ListOwnSchemaProposals(_ context.Context) ([]service.OwnSchemaProposalDTO, error) {
	f.ownProposalListCalls++
	return f.ownProposals, f.err
}

func (f *fakeContentService) GetSchemaProposal(_ context.Context, id uuid.UUID) (service.SchemaProposalDTO, error) {
	f.lastProposalID = id
	return f.proposal, f.err
}

// ownProposalCalls is counted separately from lastProposalID so a test can tell
// which of the two reads a route reached: both take the same id, and a handler
// wired to the wrong one would otherwise pass every assertion about the id.
func (f *fakeContentService) GetOwnSchemaProposal(_ context.Context, id uuid.UUID) (service.OwnSchemaProposalDTO, error) {
	f.ownProposalCalls++
	f.lastProposalID = id
	return f.ownProposal, f.err
}

func (f *fakeContentService) ApproveSchemaProposal(_ context.Context, id uuid.UUID, steps []int) (service.PlanResult, error) {
	f.approveCalls++
	f.lastProposalID = id
	f.lastApproveSteps = steps
	return f.plan, f.err
}

func (f *fakeContentService) RejectSchemaProposal(_ context.Context, id uuid.UUID) error {
	f.rejectCalls++
	f.lastProposalID = id
	return f.err
}

// --- preview links (ADR-006) -------------------------------------------------

// The response body IS a bearer credential, so the transport rules that apply to
// it are not the ones that apply to content. Both halves are asserted here
// because both are properties of THIS endpoint, not of the service beneath it:
// the service hands back a token either way.
func TestCreatePreviewLink_IsCreatedAndNeverStored(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{previewLink: service.PreviewLinkDTO{
		Token: "tok", ExpiresAt: time.Now().Add(30 * time.Minute), EntryID: id, Type: "post",
	}}

	rec := do(t, svc, http.MethodPost, "/api/v1/content/entries/"+id.String()+"/preview-link?type=post", "")

	// 201: the link did not exist before this request.
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s, want 201", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q, want \"private, no-store\" — this body is a credential", got)
	}
	if svc.lastID != id || svc.lastTypeName != "post" {
		t.Fatalf("service saw %s/%s, want post/%s", svc.lastTypeName, svc.lastID, id)
	}
}

// The type is required here as on every other entry route. Without it the
// service would be asked to mint against a type name of "", and the failure
// would surface as a confusing 404 rather than the missing parameter it is.
func TestCreatePreviewLink_RequiresType(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodPost,
		"/api/v1/content/entries/"+uuid.New().String()+"/preview-link", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s, want 400", rec.Code, rec.Body)
	}
}

// ADR-006 Amendment 6: ?populate= is parsed exactly like ?fields= — repeated
// parameters and CSV within each, flattened into one list. It reaches the
// service unvalidated on purpose: which keys are relations, which of them this
// credential may read, and which audience may ask at all (Amendment 7) are the
// service's answers, not the router's.
func TestListEntries_ParsesPopulate(t *testing.T) {
	svc := &fakeContentService{listRes: &service.EntryListResult{}}
	rec := do(t, svc, http.MethodGet,
		"/api/v1/content/entries?type=post&populate=author,tags&populate=cover", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	want := []string{"author", "tags", "cover"}
	if got := svc.lastList.Populate; len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("populate parsed as %q, want %q", got, want)
	}
}

// The single-entry route takes it too, and this is the assertion that holds the
// GetEntryInput plumbing: the handler used to have nothing to pass, so a
// signature that silently dropped the parameter would compile.
func TestGetEntry_ParsesPopulate(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{entryDTO: adminEntry(id, "post", "")}
	rec := do(t, svc, http.MethodGet,
		"/api/v1/content/entries/"+id.String()+"?type=post&populate=author,tags", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := svc.lastGet.Populate; len(got) != 2 || got[0] != "author" || got[1] != "tags" {
		t.Fatalf("populate parsed as %q", got)
	}
}

// Absent means absent. An empty non-nil slice would make guardPopulateAudience
// see "a populate was requested" on every read that never asked for one — a 403
// on every preview read, and a `related` key on every admin response that never
// asked for one, which is the byte-for-byte compatibility ADR-006 Amendment 6
// bought with `omitempty`.
func TestPopulate_AbsentParameterStaysEmpty(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{listRes: &service.EntryListResult{}, entryDTO: adminEntry(id, "post", "")}
	if rec := do(t, svc, http.MethodGet, "/api/v1/content/entries?type=post", ""); rec.Code != http.StatusOK {
		t.Fatalf("list code=%d", rec.Code)
	}
	if len(svc.lastList.Populate) != 0 {
		t.Fatalf("list populate=%q, want empty", svc.lastList.Populate)
	}
	if rec := do(t, svc, http.MethodGet, "/api/v1/content/entries/"+id.String()+"?type=post", ""); rec.Code != http.StatusOK {
		t.Fatalf("get code=%d", rec.Code)
	}
	if len(svc.lastGet.Populate) != 0 {
		t.Fatalf("get populate=%q, want empty", svc.lastGet.Populate)
	}
}

// --- schema proposal approval body (缺口計畫 5.1) ----------------------------

// The three bodies that must all mean "approve everything" — an absent body,
// an explicit JSON null, and a body with no "steps" key at all — must reach
// the service as the SAME nil, not as three different shapes it has to
// special-case. Distinct from `{"steps":[]}` below, which is a real (if
// useless) selection and must NOT collapse into this nil.
func TestApproveSchemaProposal_AbsentNullAndEmptyObjectAllForwardNilSteps(t *testing.T) {
	id := uuid.New()
	for _, tc := range []struct {
		name string
		body string
	}{
		{"absent body", ""},
		{"explicit null", `{"steps":null}`},
		{"no steps key", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeContentService{}
			rec := do(t, svc, http.MethodPost, "/api/v1/content/schema/proposals/"+id.String()+"/approve", tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			if svc.approveCalls != 1 {
				t.Fatalf("approveCalls=%d, want 1", svc.approveCalls)
			}
			if svc.lastProposalID != id {
				t.Fatalf("proposal id=%s want %s", svc.lastProposalID, id)
			}
			if svc.lastApproveSteps != nil {
				t.Fatalf("steps=%v, want nil (approve everything)", svc.lastApproveSteps)
			}
		})
	}
}

// A real selection, including the empty-slice one the service turns into a
// 422: the handler's job is only to forward what the body said, verbatim.
func TestApproveSchemaProposal_StepsSelectionIsForwardedVerbatim(t *testing.T) {
	id := uuid.New()

	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/schema/proposals/"+id.String()+"/approve", `{"steps":[0,2,3]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := svc.lastApproveSteps; len(got) != 3 || got[0] != 0 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("steps=%v, want [0 2 3]", got)
	}

	svc2 := &fakeContentService{}
	rec = do(t, svc2, http.MethodPost, "/api/v1/content/schema/proposals/"+id.String()+"/approve", `{"steps":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc2.lastApproveSteps == nil || len(svc2.lastApproveSteps) != 0 {
		t.Fatalf("steps=%v, want a non-nil EMPTY slice — that is what distinguishes it from no body at all", svc2.lastApproveSteps)
	}
}

// Malformed JSON must never quietly decay into "no steps" — a typo turning
// into "approve everything" would be the worst possible failure mode for this
// endpoint. The service must not even be called.
func TestApproveSchemaProposal_MalformedBodyIsRejectedBeforeTheService(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/schema/proposals/"+id.String()+"/approve", `{bad`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.approveCalls != 0 {
		t.Fatalf("approveCalls=%d, want 0 — the service must not run on unreadable JSON", svc.approveCalls)
	}
}

// An invalid path parameter is refused before either JSON decoding or the
// service call — a garbled id is not a proposal question.
func TestApproveSchemaProposal_InvalidProposalIDIsRejectedBeforeTheService(t *testing.T) {
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/schema/proposals/not-a-uuid/approve", `{"steps":[0]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.approveCalls != 0 {
		t.Fatalf("approveCalls=%d, want 0 — an invalid id must never reach the service", svc.approveCalls)
	}
}
