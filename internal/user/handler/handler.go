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
	"github.com/williamlabdev/saas-forge/internal/user/domain"
	"github.com/williamlabdev/saas-forge/internal/user/service"
)

// Handler is the HTTP entry point for user APIs (arch-user.md §3).
type Handler struct {
	svc service.UserService
}

// NewHandler returns a user HTTP handler.
func NewHandler(svc service.UserService) *Handler {
	return &Handler{svc: svc}
}

// Routes registers user API routes on a chi router.
func (h *Handler) Routes(r chi.Router) {
	r.Route("/api/v1/users", func(r chi.Router) {
		r.Get("/", h.listUsers)
		r.Post("/", h.createUser)
		r.Get("/me", h.currentUser)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", h.userByID)
			r.Patch("/", h.patchUser)
			r.Delete("/", h.deleteUser)
			r.Patch("/preferences", h.patchPreferences)
		})
	})
}

type createUserRequest struct {
	Username    string             `json:"username" validate:"required,min=3,max=64"`
	Email       string             `json:"email" validate:"required,email,max=254"`
	Password    string             `json:"password" validate:"required,min=8,max=128"`
	DisplayName string             `json:"display_name" validate:"omitempty,max=128"`
	Phone       string             `json:"phone" validate:"omitempty,max=32"`
	Preferences domain.Preferences `json:"preferences" validate:"omitempty"`
}

type patchUserRequest struct {
	DisplayName *string `json:"display_name" validate:"omitempty,max=128"`
	Phone       *string `json:"phone" validate:"omitempty,max=32"`
}

type patchPreferencesRequest struct {
	Merge       bool               `json:"merge"`
	Preferences domain.Preferences `json:"preferences" validate:"required"`
}

func (h *Handler) listUsers(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	result, err := h.svc.List(r.Context(), service.ListInput{
		Limit:  limit,
		Cursor: r.URL.Query().Get("cursor"),
		Status: r.URL.Query().Get("status"),
	})
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSONWithMeta(w, http.StatusOK, result.Items, response.MetaWithPage(result.Page))
}

func (h *Handler) createUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := decodeAndValidate(r, &req); err != nil {
		response.Error(w, err)
		return
	}
	idemKey := r.Header.Get("Idempotency-Key")
	dto, replayed, err := h.svc.Register(r.Context(), service.RegisterInput{
		Username:       req.Username,
		Email:          req.Email,
		Password:       req.Password,
		DisplayName:    req.DisplayName,
		Phone:          req.Phone,
		Preferences:    req.Preferences,
		IdempotencyKey: idemKey,
	})
	if err != nil {
		response.Error(w, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	response.JSON(w, status, dto)
}

func (h *Handler) currentUser(w http.ResponseWriter, r *http.Request) {
	dto, err := h.svc.CurrentUser(r.Context())
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) userByID(w http.ResponseWriter, r *http.Request) {
	id, err := parseUserID(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.ByID(r.Context(), id)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) patchUser(w http.ResponseWriter, r *http.Request) {
	id, err := parseUserID(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, err)
		return
	}
	var req patchUserRequest
	if err := decodeAndValidate(r, &req); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.UpdateProfile(r.Context(), id, service.UpdateProfileInput{
		DisplayName: req.DisplayName,
		Phone:       req.Phone,
	})
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) patchPreferences(w http.ResponseWriter, r *http.Request) {
	id, err := parseUserID(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, err)
		return
	}
	var req patchPreferencesRequest
	if err := decodeAndValidate(r, &req); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.UpdatePreferences(r.Context(), id, service.PreferencesInput{
		Merge:       req.Merge,
		Preferences: req.Preferences,
	})
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, err := parseUserID(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, err)
		return
	}
	if err := h.svc.Delete(r.Context(), id); err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]string{"deleted": id.String()})
}

func parseUserID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, apperrors.Wrap("INVALID_USER_ID", "invalid user id", 400, err)
	}
	return id, nil
}

func decodeAndValidate(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return apperrors.Wrap("INVALID_JSON", "invalid request body", 400, err)
	}
	if dec.More() {
		return apperrors.Wrap("INVALID_JSON", "invalid request body", 400, errors.New("unexpected trailing JSON values"))
	}
	if err := validate.Struct(dst); err != nil {
		return apperrors.Wrap("VALIDATION_FAILED", err.Error(), 400, err)
	}
	return nil
}
