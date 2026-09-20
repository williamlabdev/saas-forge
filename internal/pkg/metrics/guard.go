package metrics

import (
	"net/http"
	"strings"
)

// BearerGuard requires Authorization: Bearer <token> when token is non-empty.
// Empty token disables guard (local dev / internal network only).
func BearerGuard(token string) func(http.Handler) http.Handler {
	if strings.TrimSpace(token) == "" {
		return func(next http.Handler) http.Handler { return next }
	}
	want := "Bearer " + token
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != want {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"metrics: unauthorized"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
