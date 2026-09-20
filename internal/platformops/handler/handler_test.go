package handler

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/platformops/service"
)

type fakeAppSvc struct {
	listRes *service.ListResult
	dto     service.PlatformAppDTO
	err     error
}

func (f *fakeAppSvc) List(context.Context, service.ListInput) (*service.ListResult, error) {
	return f.listRes, f.err
}
func (f *fakeAppSvc) Create(context.Context, service.CreateInput) (service.PlatformAppDTO, error) {
	return f.dto, f.err
}
func (f *fakeAppSvc) UpdateStatus(context.Context, service.UpdateStatusInput) (service.PlatformAppDTO, error) {
	return f.dto, f.err
}

type fakeConsoleSvc struct {
	billing  service.BillingSummaryDTO
	invoices []service.InvoiceDTO
	staff    []service.StaffMemberDTO
	staffDTO service.StaffMemberDTO
	alerts   []service.AlertDTO
	reports  service.ReportsSummaryDTO
	err      error
}

func (f *fakeConsoleSvc) GetBillingSummary(context.Context) (service.BillingSummaryDTO, error) {
	return f.billing, f.err
}
func (f *fakeConsoleSvc) ListInvoices(context.Context, int) ([]service.InvoiceDTO, error) {
	return f.invoices, f.err
}
func (f *fakeConsoleSvc) ListStaff(context.Context) ([]service.StaffMemberDTO, error) {
	return f.staff, f.err
}
func (f *fakeConsoleSvc) CreateStaff(context.Context, service.CreateStaffInput) (service.StaffMemberDTO, error) {
	return f.staffDTO, f.err
}
func (f *fakeConsoleSvc) ListAlerts(context.Context, int) ([]service.AlertDTO, error) {
	return f.alerts, f.err
}
func (f *fakeConsoleSvc) GetReportsSummary(context.Context) (service.ReportsSummaryDTO, error) {
	return f.reports, f.err
}

type fakeTenantAdmin struct {
	dto service.TenantPlanDTO
	err error
}

func (f *fakeTenantAdmin) SetPlan(_ context.Context, slug, plan string) (service.TenantPlanDTO, error) {
	if f.err != nil {
		return service.TenantPlanDTO{}, f.err
	}
	if f.dto == (service.TenantPlanDTO{}) {
		return service.TenantPlanDTO{TenantID: slug, Plan: plan}, nil
	}
	return f.dto, nil
}

func srv(app service.PlatformAppService, console service.PlatformConsoleService) http.Handler {
	return srvWithTenantAdmin(app, console, &fakeTenantAdmin{})
}

func srvWithTenantAdmin(app service.PlatformAppService, console service.PlatformConsoleService, ta service.TenantAdminService) http.Handler {
	r := chi.NewRouter()
	NewHandler(app, console, ta).Routes(r)
	return r
}

func req(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestList_OK(t *testing.T) {
	h := srv(&fakeAppSvc{listRes: &service.ListResult{Items: []service.PlatformAppDTO{{Name: "a"}}, Total: 1}}, &fakeConsoleSvc{})
	rec := req(t, h, http.MethodGet, "/api/v1/platform/apps/?limit=5&offset=1&status=active&q=x", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestList_ServiceError(t *testing.T) {
	h := srv(&fakeAppSvc{err: apperrors.ErrForbidden}, &fakeConsoleSvc{})
	rec := req(t, h, http.MethodGet, "/api/v1/platform/apps/", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestCreate_OK(t *testing.T) {
	h := srv(&fakeAppSvc{dto: service.PlatformAppDTO{ID: uuid.New(), Name: "App"}}, &fakeConsoleSvc{})
	rec := req(t, h, http.MethodPost, "/api/v1/platform/apps/", `{"name":"App","tenant_id":"t1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestCreate_InvalidJSON(t *testing.T) {
	rec := req(t, srv(&fakeAppSvc{}, &fakeConsoleSvc{}), http.MethodPost, "/api/v1/platform/apps/", `{`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestCreate_ValidationFails(t *testing.T) {
	// missing tenant_id -> validate.Struct fails in decodeJSON
	rec := req(t, srv(&fakeAppSvc{}, &fakeConsoleSvc{}), http.MethodPost, "/api/v1/platform/apps/", `{"name":"App"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestUpdateStatus_OK(t *testing.T) {
	id := uuid.New()
	h := srv(&fakeAppSvc{dto: service.PlatformAppDTO{ID: id}}, &fakeConsoleSvc{})
	rec := req(t, h, http.MethodPatch, "/api/v1/platform/apps/"+id.String()+"/status", `{"status":"paused"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestUpdateStatus_InvalidID(t *testing.T) {
	rec := req(t, srv(&fakeAppSvc{}, &fakeConsoleSvc{}), http.MethodPatch, "/api/v1/platform/apps/not-a-uuid/status", `{"status":"paused"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestUpdateStatus_InvalidJSON(t *testing.T) {
	id := uuid.New()
	rec := req(t, srv(&fakeAppSvc{}, &fakeConsoleSvc{}), http.MethodPatch, "/api/v1/platform/apps/"+id.String()+"/status", `{`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestBillingSummary_OK(t *testing.T) {
	h := srv(&fakeAppSvc{}, &fakeConsoleSvc{billing: service.BillingSummaryDTO{PlanName: "Pro"}})
	rec := req(t, h, http.MethodGet, "/api/v1/platform/billing/summary", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestBillingSummary_Error(t *testing.T) {
	h := srv(&fakeAppSvc{}, &fakeConsoleSvc{err: apperrors.ErrUnauthorized})
	rec := req(t, h, http.MethodGet, "/api/v1/platform/billing/summary", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListInvoices_OK(t *testing.T) {
	h := srv(&fakeAppSvc{}, &fakeConsoleSvc{invoices: []service.InvoiceDTO{{ID: "i1"}}})
	rec := req(t, h, http.MethodGet, "/api/v1/platform/billing/invoices?limit=3", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListInvoices_Error(t *testing.T) {
	h := srv(&fakeAppSvc{}, &fakeConsoleSvc{err: errors.New("boom")})
	rec := req(t, h, http.MethodGet, "/api/v1/platform/billing/invoices", "")
	if rec.Code < 400 {
		t.Fatalf("expected error, code=%d", rec.Code)
	}
}

func TestListStaff_OK(t *testing.T) {
	h := srv(&fakeAppSvc{}, &fakeConsoleSvc{staff: []service.StaffMemberDTO{{Name: "Ada"}}})
	rec := req(t, h, http.MethodGet, "/api/v1/platform/staff", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListStaff_Error(t *testing.T) {
	h := srv(&fakeAppSvc{}, &fakeConsoleSvc{err: errors.New("boom")})
	rec := req(t, h, http.MethodGet, "/api/v1/platform/staff", "")
	if rec.Code < 400 {
		t.Fatalf("expected error, code=%d", rec.Code)
	}
}

func TestCreateStaff_OK(t *testing.T) {
	h := srv(&fakeAppSvc{}, &fakeConsoleSvc{staffDTO: service.StaffMemberDTO{ID: "s1", Email: "a@b.com"}})
	rec := req(t, h, http.MethodPost, "/api/v1/platform/staff", `{"email":"a@b.com","role":"admin"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestCreateStaff_InvalidJSON(t *testing.T) {
	rec := req(t, srv(&fakeAppSvc{}, &fakeConsoleSvc{}), http.MethodPost, "/api/v1/platform/staff", `{`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestCreateStaff_ValidationFails(t *testing.T) {
	rec := req(t, srv(&fakeAppSvc{}, &fakeConsoleSvc{}), http.MethodPost, "/api/v1/platform/staff", `{"email":"not-an-email","role":"admin"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestListAlerts_OK(t *testing.T) {
	h := srv(&fakeAppSvc{}, &fakeConsoleSvc{alerts: []service.AlertDTO{{ID: "a1"}}})
	rec := req(t, h, http.MethodGet, "/api/v1/platform/alerts?limit=2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestReportsSummary_OK(t *testing.T) {
	h := srv(&fakeAppSvc{}, &fakeConsoleSvc{reports: service.ReportsSummaryDTO{ActiveApps: 3}})
	rec := req(t, h, http.MethodGet, "/api/v1/platform/reports/summary", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestReportsSummary_Error(t *testing.T) {
	h := srv(&fakeAppSvc{}, &fakeConsoleSvc{err: errors.New("boom")})
	rec := req(t, h, http.MethodGet, "/api/v1/platform/reports/summary", "")
	if rec.Code < 400 {
		t.Fatalf("expected error, code=%d", rec.Code)
	}
}
