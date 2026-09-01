package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/response"
	"github.com/williamlabdev/saas-forge/internal/pkg/validate"
	"github.com/williamlabdev/saas-forge/internal/platformops/service"
)

type Handler struct {
	svc         service.PlatformAppService
	console     service.PlatformConsoleService
	tenantAdmin service.TenantAdminService
}

func NewHandler(svc service.PlatformAppService, console service.PlatformConsoleService, tenantAdmin service.TenantAdminService) *Handler {
	return &Handler{svc: svc, console: console, tenantAdmin: tenantAdmin}
}

func (h *Handler) Routes(r chi.Router) {
	r.Route("/api/v1/platform", func(r chi.Router) {
		r.Route("/apps", func(r chi.Router) {
			r.Get("/", h.list)
			r.Post("/", h.create)
			r.Patch("/{id}/status", h.updateStatus)
		})
		// Tenant billing administration (TKT-R4b PR3).
		r.Put("/tenants/{slug}/plan", h.setTenantPlan)
		r.Get("/billing/summary", h.billingSummary)
		r.Get("/billing/invoices", h.listInvoices)
		r.Get("/staff", h.listStaff)
		r.Post("/staff", h.createStaff)
		r.Get("/alerts", h.listAlerts)
		r.Get("/reports/summary", h.reportsSummary)
	})
}

type setTenantPlanRequest struct {
	Plan string `json:"plan" validate:"required,min=1,max=100"`
}

func (h *Handler) setTenantPlan(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	var req setTenantPlanRequest
	if err := decodeJSON(r, &req); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.tenantAdmin.SetPlan(r.Context(), slug, req.Plan)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	res, err := h.svc.List(r.Context(), service.ListInput{
		Query:  r.URL.Query().Get("q"),
		Status: r.URL.Query().Get("status"),
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, res)
}

type createRequest struct {
	Name     string `json:"name" validate:"required,min=1,max=200"`
	TenantID string `json:"tenant_id" validate:"required,min=1,max=100"`
	Owner    string `json:"owner" validate:"omitempty,max=200"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := decodeJSON(r, &req); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.Create(r.Context(), service.CreateInput(req))
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusCreated, dto)
}

type updateStatusRequest struct {
	Status string `json:"status" validate:"required"`
}

func (h *Handler) updateStatus(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, apperrors.Wrap("VALIDATION_FAILED", "invalid id", 400, err))
		return
	}
	var req updateStatusRequest
	if err := decodeJSON(r, &req); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.UpdateStatus(r.Context(), service.UpdateStatusInput{
		ID:     id,
		Status: req.Status,
	})
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return apperrors.Wrap("INVALID_JSON", "invalid request body", 400, err)
	}
	if dec.More() {
		return apperrors.Wrap("INVALID_JSON", "invalid request body", 400, errors.New("unexpected trailing JSON"))
	}
	if err := validate.Struct(dst); err != nil {
		return apperrors.Wrap("VALIDATION_FAILED", err.Error(), 400, err)
	}
	return nil
}
