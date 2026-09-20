package handler

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/williamlabdev/saas-forge/internal/auth/service"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/response"
)

// AgentCredentialHandler exposes the agent-credential lifecycle (ADR-013,
// ruled 2026-08-06): mint, list, revoke.
//
// It lives under /api/v1/auth because a credential is an auth object, but it is
// a separate handler from the login endpoints because it is the only part of
// this route group that requires an authenticated, ROLE-BEARING caller. The
// login endpoints are reachable by definition without one.
type AgentCredentialHandler struct {
	svc service.AgentCredentialService
}

func NewAgentCredentialHandler(svc service.AgentCredentialService) *AgentCredentialHandler {
	return &AgentCredentialHandler{svc: svc}
}

func (h *AgentCredentialHandler) Routes(r chi.Router) {
	r.Route("/api/v1/auth/agent-tokens", func(r chi.Router) {
		r.Post("/", h.issue)
		r.Get("/", h.list)
		r.Delete("/{id}", h.revoke)
	})
}

type issueAgentTokenRequest struct {
	AgentID string `json:"agent_id" validate:"required"`
	// Same reasoning as AllowedTypes below: absent and empty must produce the
	// one refusal the service owns, and the set of legal values depends on the
	// CALLER's role, which a struct tag cannot see.
	TenantRole string `json:"tenant_role"`
	// No `validate:"required"` on the slice: an ABSENT field and an EMPTY list
	// must produce the same refusal with the same message, and the service is
	// where that single answer lives. A validator tag here would answer the
	// absent case in a different voice and leave `[]` to the service.
	AllowedTypes []string `json:"allowed_types"`
}

func (h *AgentCredentialHandler) issue(w http.ResponseWriter, r *http.Request) {
	var req issueAgentTokenRequest
	if err := decodeJSON(r, &req); err != nil {
		response.Error(w, err)
		return
	}
	// The caller's own token, passed through to be downgraded. See
	// AgentCredentialService.Issue for why the raw string and not the Subject.
	dto, err := h.svc.Issue(r.Context(), bearerToken(r), service.IssueAgentCredentialInput{
		AgentID:      req.AgentID,
		TenantRole:   req.TenantRole,
		AllowedTypes: req.AllowedTypes,
	})
	if err != nil {
		response.Error(w, err)
		return
	}
	// 201: this created something that outlives the request, which is the whole
	// difference between this endpoint and /auth/login.
	response.JSON(w, http.StatusCreated, dto)
}

func (h *AgentCredentialHandler) list(w http.ResponseWriter, r *http.Request) {
	creds, err := h.svc.List(r.Context())
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, creds)
}

func (h *AgentCredentialHandler) revoke(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, apperrors.New("VALIDATION_FAILED", "credential id must be a uuid", 400))
		return
	}
	if err := h.svc.Revoke(r.Context(), id); err != nil {
		response.Error(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// bearerToken returns the raw credential the caller presented, or "" when the
// request authenticated some other way (dev headers) or not at all.
func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
}
