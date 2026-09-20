package authn

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// Middleware extracts a verified subject from request headers.
// Production: replace with JWT validation at gateway; domain services only trust injected context.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawID := strings.TrimSpace(r.Header.Get("X-User-Id"))
		if rawID == "" {
			next.ServeHTTP(w, r)
			return
		}

		userID, err := uuid.Parse(rawID)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}

		sub := Subject{
			UserID:      userID,
			Roles:       parseRoles(r.Header.Get("X-User-Roles")),
			TenantID:    strings.TrimSpace(r.Header.Get("X-Tenant-Id")),
			TenantRole:  strings.TrimSpace(r.Header.Get("X-Tenant-Role")),
			Region:      strings.TrimSpace(r.Header.Get("X-User-Region")),
			MFAVerified: strings.EqualFold(r.Header.Get("X-Mfa-Verified"), "true"),
		}
		next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), sub)))
	})
}

func parseRoles(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
