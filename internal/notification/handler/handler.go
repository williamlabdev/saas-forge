package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/notification/service"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/response"
	"github.com/williamlabdev/saas-forge/internal/pkg/validate"
)

type Handler struct {
	svc service.NotificationService
}

func NewHandler(svc service.NotificationService) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) Routes(r chi.Router) {
	r.Route("/api/v1/notifications", func(r chi.Router) {
		r.Get("/", h.list)
		r.Post("/", h.create)
		r.Get("/unread-count", h.unreadCount)
		r.Post("/read-all", h.readAll)
		r.Patch("/{id}/read", h.markRead)
	})
}

type createRequest struct {
	Title string `json:"title" validate:"required,min=1,max=200"`
	Body  string `json:"body" validate:"required,min=1,max=2000"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	unread, _ := strconv.ParseBool(r.URL.Query().Get("unread"))
	items, err := h.svc.ListMine(r.Context(), limit, unread)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"items": items})
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

func (h *Handler) unreadCount(w http.ResponseWriter, r *http.Request) {
	count, err := h.svc.UnreadCount(r.Context())
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"count": count})
}

func (h *Handler) markRead(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, apperrors.ErrNotFound)
		return
	}
	dto, err := h.svc.MarkRead(r.Context(), id)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) readAll(w http.ResponseWriter, r *http.Request) {
	count, err := h.svc.MarkAllRead(r.Context())
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"count": count})
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
