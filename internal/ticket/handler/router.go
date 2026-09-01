package handler

import (
	"github.com/go-chi/chi/v5"
)

// Routes mounts the ticket endpoints onto the given chi router.
// Mount this from the application router (e.g. internal/platform/router.go)
// by calling ticketH.Routes(r) alongside the other domain handlers.
func (h *Handler) Routes(r chi.Router) {
	r.Route("/api/v1/tickets", func(r chi.Router) {
		r.Get("/", h.list)
		r.Post("/", h.create)
		r.Get("/{id}", h.get)
		r.Put("/{id}", h.update)
		r.Delete("/{id}", h.delete)
	})
}
