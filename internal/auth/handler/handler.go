package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/williamlabdev/saas-forge/internal/auth/service"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/metrics"
	"github.com/williamlabdev/saas-forge/internal/pkg/ratelimit"
	"github.com/williamlabdev/saas-forge/internal/pkg/requestctx"
	"github.com/williamlabdev/saas-forge/internal/pkg/response"
	"github.com/williamlabdev/saas-forge/internal/pkg/validate"
)

// Handler exposes auth HTTP APIs.
type Handler struct {
	svc      service.AuthService
	limiter  *ratelimit.IPLimiter
	registry *metrics.Registry
}

func NewHandler(svc service.AuthService, limiter *ratelimit.IPLimiter, registry *metrics.Registry) *Handler {
	return &Handler{svc: svc, limiter: limiter, registry: registry}
}

func (h *Handler) Routes(r chi.Router) {
	r.Route("/api/v1/auth", func(r chi.Router) {
		r.Post("/login", h.login)
		r.Post("/refresh", h.refresh)
		r.Post("/switch-tenant", h.switchTenant)
		r.Post("/logout", h.logout)
	})
}

type loginRequest struct {
	Email    string `json:"email" validate:"required,email"`
	Password string `json:"password" validate:"required,min=8"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token" validate:"required"`
}

type switchTenantRequest struct {
	Tenant string `json:"tenant" validate:"required"`
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if h.limiter != nil && !h.limiter.Allow(requestctx.ClientIP(r)) {
		h.incRateLimited()
		response.Error(w, service.ErrRateLimited)
		return
	}
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		response.Error(w, err)
		return
	}
	ctx := requestctx.WithMeta(r.Context(), requestctx.Meta{
		ClientIP:  requestctx.ClientIP(r),
		UserAgent: r.UserAgent(),
	})
	tokens, err := h.svc.Login(ctx, service.LoginInput(req))
	if err != nil {
		h.incLoginFailure()
		response.Error(w, err)
		return
	}
	h.incLoginSuccess()
	response.JSON(w, http.StatusOK, tokens)
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	if h.limiter != nil && !h.limiter.Allow(requestctx.ClientIP(r)) {
		h.incRateLimited()
		response.Error(w, service.ErrRateLimited)
		return
	}
	var req refreshRequest
	if err := decodeJSON(r, &req); err != nil {
		response.Error(w, err)
		return
	}
	ctx := requestctx.WithMeta(r.Context(), requestctx.Meta{
		ClientIP:  requestctx.ClientIP(r),
		UserAgent: r.UserAgent(),
	})
	tokens, err := h.svc.Refresh(ctx, req.RefreshToken)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, tokens)
}

// switchTenant re-issues tokens for another tenant the caller belongs to (D5).
// Requires an authenticated subject — the JWT middleware injects it; there is
// no refresh token in the request, so the current session simply gains a new
// token pair scoped to the target tenant.
func (h *Handler) switchTenant(w http.ResponseWriter, r *http.Request) {
	// Same throttle as login/refresh: this endpoint also mints token pairs.
	if h.limiter != nil && !h.limiter.Allow(requestctx.ClientIP(r)) {
		h.incRateLimited()
		response.Error(w, service.ErrRateLimited)
		return
	}
	sub, ok := authn.SubjectFromContext(r.Context())
	if !ok {
		response.Error(w, apperrors.ErrUnauthorized)
		return
	}
	var req switchTenantRequest
	if err := decodeJSON(r, &req); err != nil {
		response.Error(w, err)
		return
	}
	ctx := requestctx.WithMeta(r.Context(), requestctx.Meta{
		ClientIP:  requestctx.ClientIP(r),
		UserAgent: r.UserAgent(),
	})
	tokens, err := h.svc.SwitchTenant(ctx, sub.UserID, req.Tenant)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, tokens)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	_ = decodeJSON(r, &req)
	ctx := requestctx.WithMeta(r.Context(), requestctx.Meta{
		ClientIP:  requestctx.ClientIP(r),
		UserAgent: r.UserAgent(),
	})
	if err := h.svc.Logout(ctx, req.RefreshToken); err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) incLoginSuccess() {
	if h.registry != nil {
		h.registry.AuthLoginSuccess.Add(1)
	}
}

func (h *Handler) incLoginFailure() {
	if h.registry != nil {
		h.registry.AuthLoginFailure.Add(1)
	}
}

func (h *Handler) incRateLimited() {
	if h.registry != nil {
		h.registry.AuthRateLimited.Add(1)
	}
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
