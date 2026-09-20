package handler

import (
	"github.com/go-chi/chi/v5"
)

// Routes mounts the __domain__ endpoints onto the given chi router.
// Mount this from the application router (e.g. internal/platform/router.go)
// by calling __domain__H.Routes(r) alongside the other domain handlers.
func (h *Handler) Routes(r chi.Router) {
	r.Route("/api/v1/__domains__", func(r chi.Router) {
		r.Get("/", h.list)
		r.Post("/", h.create)
		r.Get("/{id}", h.get)
		r.Put("/{id}", h.update)
		r.Delete("/{id}", h.delete)
	})
}
