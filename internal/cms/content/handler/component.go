package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/williamlabdev/saas-forge/internal/cms/content/service"
	"github.com/williamlabdev/saas-forge/internal/pkg/response"
)

// --- components (ADR-020) ---------------------------------------------------
//
// A component is addressed by NAME, like a type, and its sub-fields by key.
// The verbs mirror the type verbs one for one — including the rename-is-its-
// own-verb rule and the ?force=true consent on delete — so a caller who knows
// /types needs to learn nothing new, and so the two surfaces cannot drift.

func (h *Handler) createComponent(w http.ResponseWriter, r *http.Request) {
	var in service.CreateComponentInput
	if err := decodeJSON(r, &in); err != nil {
		response.Error(w, err)
		return
	}
	ctx := service.WithUsageWarnings(r.Context())
	dto, err := h.svc.CreateComponent(ctx, in)
	if err != nil {
		response.Error(w, err)
		return
	}
	setUsageWarningHeader(w, ctx)
	response.JSON(w, http.StatusCreated, dto)
}

func (h *Handler) listComponents(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListComponents(r.Context())
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, list)
}

// listComponentTemplates and installComponentTemplate are the built-in
// component templates (ADR-020 Amendment 2). Registered ahead of
// GET/POST /components/{name} in router.go — see that file for why the
// registration order does not actually matter to chi, and
// component_template_test.go for the proof.
func (h *Handler) listComponentTemplates(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListComponentTemplates(r.Context())
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string][]service.ComponentTemplateDTO{"templates": list})
}

func (h *Handler) installComponentTemplate(w http.ResponseWriter, r *http.Request) {
	dto, err := h.svc.InstallComponentTemplate(r.Context(), chi.URLParam(r, "name"))
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusCreated, dto)
}

func (h *Handler) getComponent(w http.ResponseWriter, r *http.Request) {
	dto, err := h.svc.GetComponent(r.Context(), chi.URLParam(r, "name"))
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) updateComponent(w http.ResponseWriter, r *http.Request) {
	var in service.UpdateComponentInput
	if err := decodeJSON(r, &in); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.UpdateComponent(r.Context(), chi.URLParam(r, "name"), in)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) deleteComponent(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteComponent(r.Context(), chi.URLParam(r, "name")); err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusNoContent, nil)
}

func (h *Handler) addComponentField(w http.ResponseWriter, r *http.Request) {
	var in service.FieldInput
	if err := decodeJSON(r, &in); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.AddComponentField(r.Context(), chi.URLParam(r, "name"), in)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusCreated, dto)
}

func (h *Handler) updateComponentField(w http.ResponseWriter, r *http.Request) {
	var in service.UpdateFieldInput
	if err := decodeJSON(r, &in); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.UpdateComponentField(r.Context(), chi.URLParam(r, "name"), chi.URLParam(r, "key"), in)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) renameComponent(w http.ResponseWriter, r *http.Request) {
	var in service.RenameInput
	if err := decodeJSON(r, &in); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.RenameComponent(r.Context(), chi.URLParam(r, "name"), in)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) renameComponentField(w http.ResponseWriter, r *http.Request) {
	var in service.RenameInput
	if err := decodeJSON(r, &in); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.RenameComponentField(r.Context(), chi.URLParam(r, "name"), chi.URLParam(r, "key"), in)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) deleteComponentField(w http.ResponseWriter, r *http.Request) {
	// The literal "true" and nothing else — the same consent rule as deleteField.
	force := r.URL.Query().Get("force") == "true"
	dto, err := h.svc.DeleteComponentField(r.Context(), chi.URLParam(r, "name"), chi.URLParam(r, "key"), force)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}
