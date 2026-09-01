package handler

import (
	"net/http"
	"strconv"

	"github.com/williamlabdev/saas-forge/internal/pkg/response"
	"github.com/williamlabdev/saas-forge/internal/platformops/service"
)

func (h *Handler) billingSummary(w http.ResponseWriter, r *http.Request) {
	dto, err := h.console.GetBillingSummary(r.Context())
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) listInvoices(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := h.console.ListInvoices(r.Context(), limit)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"items": rows})
}

func (h *Handler) listStaff(w http.ResponseWriter, r *http.Request) {
	rows, err := h.console.ListStaff(r.Context())
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"items": rows})
}

type createStaffRequest struct {
	Email string `json:"email" validate:"required,email,max=200"`
	Role  string `json:"role" validate:"required,max=50"`
	Name  string `json:"name" validate:"omitempty,max=200"`
}

func (h *Handler) createStaff(w http.ResponseWriter, r *http.Request) {
	var req createStaffRequest
	if err := decodeJSON(r, &req); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.console.CreateStaff(r.Context(), service.CreateStaffInput(req))
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusCreated, dto)
}

func (h *Handler) listAlerts(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := h.console.ListAlerts(r.Context(), limit)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"items": rows})
}

func (h *Handler) reportsSummary(w http.ResponseWriter, r *http.Request) {
	dto, err := h.console.GetReportsSummary(r.Context())
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}
