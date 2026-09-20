package authn

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// GatewayHeader carries the shared secret a trusted gateway injects so the
// domain API can reject requests that reached :8080 directly (TKT-R7). The
// gateway is expected to strip any client-supplied copy and set its own.
const GatewayHeader = "X-Gateway-Secret"

// GatewayGuard rejects requests whose GatewayHeader does not match secret.
// An empty secret disables the guard (dev / when a gateway enforces mTLS or a
// network boundary instead). The exempt predicate lets liveness probes through
// (they cannot carry the secret). The comparison is constant-time.
func GatewayGuard(secret string, exempt func(*http.Request) bool) func(http.Handler) http.Handler {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return func(next http.Handler) http.Handler { return next }
	}
	want := []byte(secret)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if exempt == nil || !exempt(r) {
				got := []byte(r.Header.Get(GatewayHeader))
				if subtle.ConstantTimeCompare(got, want) != 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"error":{"code":"GATEWAY_REQUIRED","message":"requests must arrive through the trusted gateway"}}`))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
