package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/iam/service"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/response"
	"github.com/williamlabdev/saas-forge/internal/pkg/validate"
)

// Handler exposes IAM admin HTTP APIs (protocol translation only).
type Handler struct {
	svc service.IAMAdminService
}

func NewHandler(svc service.IAMAdminService) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) Routes(r chi.Router) {
	r.Route("/api/v1/users/{id}/roles", func(r chi.Router) {
		r.Get("/", h.listRoles)
		r.Put("/", h.assignRole)
		r.Delete("/{role}", h.revokeRole)
	})
}

func (h *Handler) listRoles(w http.ResponseWriter, r *http.Request) {
	userID, err := parseUserID(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, err)
		return
	}
	roles, err := h.svc.ListRolesForUser(r.Context(), userID)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"roles": roles})
}

type assignRoleRequest struct {
	Role string `json:"role" validate:"required,min=1,max=64"`
}

func (h *Handler) assignRole(w http.ResponseWriter, r *http.Request) {
	userID, err := parseUserID(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, err)
		return
	}
	var req assignRoleRequest
	if err := decodeJSON(r, &req); err != nil {
		response.Error(w, err)
		return
	}
	if err := h.svc.AssignRoleByName(r.Context(), userID, req.Role); err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]string{"role": req.Role, "status": "assigned"})
}

func (h *Handler) revokeRole(w http.ResponseWriter, r *http.Request) {
	userID, err := parseUserID(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, err)
		return
	}
	role := chi.URLParam(r, "role")
	if err := h.svc.RevokeRoleByName(r.Context(), userID, role); err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]string{"role": role, "status": "revoked"})
}

func parseUserID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, apperrors.Wrap("INVALID_USER_ID", "invalid user id", 400, err)
	}
	return id, nil
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return apperrors.Wrap("INVALID_JSON", "invalid request body", 400, err)
	}
	if err := validate.Struct(dst); err != nil {
		return apperrors.Wrap("VALIDATION_FAILED", err.Error(), 400, err)
	}
	if dec.More() {
		return apperrors.Wrap("INVALID_JSON", "invalid request body", 400, errors.New("unexpected trailing JSON"))
	}
	return nil
}
