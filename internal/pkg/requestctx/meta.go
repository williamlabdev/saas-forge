package requestctx

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
)

type metaKey struct{}

// Meta carries request-scoped metadata for audit and rate limiting.
type Meta struct {
	ClientIP  string
	UserAgent string
}

func WithMeta(ctx context.Context, m Meta) context.Context {
	return context.WithValue(ctx, metaKey{}, m)
}

func MetaFrom(ctx context.Context) (Meta, bool) {
	m, ok := ctx.Value(metaKey{}).(Meta)
	return m, ok
}

// trustProxyHeaders gates whether ClientIP believes X-Forwarded-For. Off by
// default: XFF is client-supplied, so trusting it on a directly exposed
// listener lets an attacker rotate the header to bypass IP rate limiting and
// forge audit entries (the same class as chi's deprecated RealIP,
// GHSA-3fxj-6jh8-hvhx). Enable only behind a proxy that strips and rewrites
// the header (TRUST_PROXY_HEADERS=true).
var trustProxyHeaders atomic.Bool

// SetTrustProxyHeaders is called once at startup from runtime config.
func SetTrustProxyHeaders(v bool) { trustProxyHeaders.Store(v) }

// ClientIP returns the client IP for rate limiting and audit. It uses the
// socket peer address unless proxy headers are explicitly trusted.
func ClientIP(r *http.Request) string {
	if trustProxyHeaders.Load() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
