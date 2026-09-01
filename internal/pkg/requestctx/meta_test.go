package requestctx

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestClientIP_DefaultIgnoresForwardedFor(t *testing.T) {
	SetTrustProxyHeaders(false)
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.9:4444"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")
	if got := ClientIP(r); got != "10.0.0.9" {
		t.Fatalf("got %q, want socket peer 10.0.0.9 (XFF must be ignored by default)", got)
	}
}

func TestClientIP_TrustedProxyUsesFirstHop(t *testing.T) {
	SetTrustProxyHeaders(true)
	t.Cleanup(func() { SetTrustProxyHeaders(false) })
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.9:4444"
	r.Header.Set("X-Forwarded-For", " 1.2.3.4 , 5.6.7.8")
	if got := ClientIP(r); got != "1.2.3.4" {
		t.Fatalf("got %q, want first XFF hop", got)
	}
}

func TestClientIP_TrustedProxyWithoutHeaderFallsBack(t *testing.T) {
	SetTrustProxyHeaders(true)
	t.Cleanup(func() { SetTrustProxyHeaders(false) })
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.9:4444"
	if got := ClientIP(r); got != "10.0.0.9" {
		t.Fatalf("got %q, want 10.0.0.9", got)
	}
}

func TestClientIP_BadRemoteAddrReturnedVerbatim(t *testing.T) {
	SetTrustProxyHeaders(false)
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "no-port"
	if got := ClientIP(r); got != "no-port" {
		t.Fatalf("got %q, want raw RemoteAddr", got)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	ctx := WithMeta(context.Background(), Meta{ClientIP: "1.1.1.1", UserAgent: "ua"})
	m, ok := MetaFrom(ctx)
	if !ok || m.ClientIP != "1.1.1.1" || m.UserAgent != "ua" {
		t.Fatalf("meta round-trip failed: %+v ok=%v", m, ok)
	}
	if _, ok := MetaFrom(context.Background()); ok {
		t.Fatal("empty context must not carry meta")
	}
}
