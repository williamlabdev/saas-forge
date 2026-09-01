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
	"github.com/williamlabdev/saas-forge/internal/ticket/service"
)

type Handler struct {
	svc service.TicketService
}

func NewHandler(svc service.TicketService) *Handler {
	return &Handler{svc: svc}
}

type createRequest struct {
	Name string `json:"name" validate:"required,min=1,max=200"`
}

type updateRequest struct {
	Name   string `json:"name" validate:"required,min=1,max=200"`
	Status string `json:"status" validate:"required"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	res, err := h.svc.List(r.Context(), service.ListInput{Limit: limit, Offset: offset})
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, res)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.GetByID(r.Context(), id)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
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

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	var req updateRequest
	if err := decodeJSON(r, &req); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.Update(r.Context(), service.UpdateInput{
		ID:     id,
		Name:   req.Name,
		Status: req.Status,
	})
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	if err := h.svc.Delete(r.Context(), id); err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusNoContent, nil)
}

func parseID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, apperrors.Wrap("INVALID_ID", "invalid id", 400, err)
	}
	return id, nil
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
