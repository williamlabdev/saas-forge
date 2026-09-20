// Package handler exposes tenant membership HTTP APIs (TKT-R1 PR-invite).
package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/response"
	"github.com/williamlabdev/saas-forge/internal/pkg/validate"
	"github.com/williamlabdev/saas-forge/internal/tenant/service"
)

type Handler struct {
	svc service.TenantService
}

func NewHandler(svc service.TenantService) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) Routes(r chi.Router) {
	r.Route("/api/v1/tenants", func(r chi.Router) {
		r.Post("/invites", h.createInvite)
		r.Post("/invites/accept", h.acceptInvite)
		// --- 2.2c: member management (ADR-022) --------------------------
		r.Get("/members", h.listMembers)
		r.Patch("/members/{user_id}", h.updateMemberRole)
		r.Delete("/members/{user_id}", h.removeMember)
		r.Get("/invites", h.listInvites)
		r.Delete("/invites/{id}", h.revokeInvite)
	})
}

type acceptInviteRequest struct {
	Token string `json:"token" validate:"required"`
}

func (h *Handler) createInvite(w http.ResponseWriter, r *http.Request) {
	var req service.CreateInviteInput
	if err := decodeJSON(r, &req); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.CreateInvite(r.Context(), req)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusCreated, dto)
}

func (h *Handler) acceptInvite(w http.ResponseWriter, r *http.Request) {
	var req acceptInviteRequest
	if err := decodeJSON(r, &req); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.AcceptInvite(r.Context(), req.Token)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

// --- 2.2c: member management (ADR-022) --------------------------------------

func (h *Handler) listMembers(w http.ResponseWriter, r *http.Request) {
	dto, err := h.svc.ListMembers(r.Context())
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) updateMemberRole(w http.ResponseWriter, r *http.Request) {
	targetUserID, err := parseUUIDParam(r, "user_id")
	if err != nil {
		response.Error(w, err)
		return
	}
	var req service.UpdateMemberRoleInput
	if err := decodeJSON(r, &req); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.UpdateMemberRole(r.Context(), targetUserID, req)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	targetUserID, err := parseUUIDParam(r, "user_id")
	if err != nil {
		response.Error(w, err)
		return
	}
	if err := h.svc.RemoveMember(r.Context(), targetUserID); err != nil {
		response.Error(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listInvites(w http.ResponseWriter, r *http.Request) {
	dto, err := h.svc.ListInvites(r.Context())
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) revokeInvite(w http.ResponseWriter, r *http.Request) {
	inviteID, err := parseUUIDParam(r, "id")
	if err != nil {
		response.Error(w, err)
		return
	}
	if err := h.svc.RevokeInvite(r.Context(), inviteID); err != nil {
		response.Error(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseUUIDParam(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, apperrors.Wrap("INVALID_ID", "invalid "+name, 400, err)
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
