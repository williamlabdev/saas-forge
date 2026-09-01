package authn

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/auth/jwt"
)

// The request carries a full set of forged identity headers — with dev
// headers disabled they must be ignored entirely (TKT-R2 DoD 3).
func TestJWTMiddleware_RejectsWithoutTokenWhenDevDisabled(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	signer := jwt.NewSigner(secret, time.Minute)
	id := uuid.New()
	token, _, err := signer.IssueAccessToken(id, []string{"member"}, "", "", "", false)
	require.NoError(t, err)

	var subject Subject
	var ok bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject, ok = SubjectFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	// No Bearer, forged identity headers — subject must be absent: with dev
	// headers disabled the X-* identity headers are dead inputs.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-User-Id", uuid.NewString())
	req.Header.Set("X-User-Roles", "admin")
	req.Header.Set("X-Tenant-Id", "tenant_victim")
	req.Header.Set("X-Tenant-Role", "owner")
	rec := httptest.NewRecorder()
	JWTMiddleware(signer, false, nil)(next).ServeHTTP(rec, req)
	assert.False(t, ok, "forged dev headers must be ignored when disabled")

	// Bearer works.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	rec2 := httptest.NewRecorder()
	JWTMiddleware(signer, false, nil)(next).ServeHTTP(rec2, req2)
	require.True(t, ok)
	assert.Equal(t, id, subject.UserID)
}

func TestJWTMiddleware_DevHeadersWhenAllowed(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	signer := jwt.NewSigner(secret, time.Minute)
	id := uuid.New()

	var subject Subject
	var ok bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject, ok = SubjectFromContext(r.Context())
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-User-Id", id.String())
	req.Header.Set("X-User-Roles", "admin")
	rec := httptest.NewRecorder()
	JWTMiddleware(signer, true, nil)(next).ServeHTTP(rec, req)

	require.True(t, ok)
	assert.Equal(t, id, subject.UserID)
	assert.Contains(t, subject.Roles, "admin")
}

// The delivery marker gates a public-facing surface, so it must only ever come
// from a signed claim. The dev-header path must not be able to forge it.
func TestJWTMiddleware_DeliveryOnlyFromSignedClaim(t *testing.T) {
	signer := jwt.NewSigner([]byte("test-secret-at-least-32-bytes-long!!"), time.Hour).
		WithDeliveryKey([]byte("delivery-secret-at-least-32-bytes!!!"))

	// Signed delivery token → marker present.
	tok, _, err := signer.IssueDeliveryToken(uuid.New(), "tenant-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	var got Subject
	h := JWTMiddleware(signer, true, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = SubjectFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !got.PublicDelivery {
		t.Fatal("signed delivery token must set PublicDelivery")
	}

	// Dev headers (no bearer) must never produce a delivery subject, however
	// they are crafted.
	got = Subject{}
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-User-Id", uuid.NewString())
	req.Header.Set("X-Tenant-Id", "tenant-1")
	req.Header.Set("X-Delivery", "true")
	req.Header.Set("X-Public-Delivery", "true")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got.PublicDelivery {
		t.Fatal("dev headers must not be able to forge a delivery credential")
	}
}

// With no dedicated delivery key the feature is off end-to-end: a token minted
// by an edge that holds some other key must not produce a delivery subject.
func TestJWTMiddleware_NoDeliveryKeyMeansNoDeliverySubject(t *testing.T) {
	main := []byte("test-secret-at-least-32-bytes-long!!")
	edgeKey := []byte("delivery-secret-at-least-32-bytes!!!")

	edge := jwt.NewSigner(main, time.Hour).WithDeliveryKey(edgeKey)
	tok, _, err := edge.IssueDeliveryToken(uuid.New(), "tenant-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// The API is configured WITHOUT a delivery key → must not accept it at all.
	api := jwt.NewSigner(main, time.Hour)
	var got Subject
	var called bool
	h := JWTMiddleware(api, false, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, called = SubjectFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if called && got.PublicDelivery {
		t.Fatal("delivery token must not be honoured when no delivery key is configured")
	}
}

// The bearer is tried FIRST and the handler returns on success, so the dev
// headers are a fallback that is never reached when a valid token is present.
//
// This is not a curiosity. ADR-015 landed `Authorization` on the console's
// content client on the argument that "one extra header cannot break
// anything" — true against a reverse proxy, FALSE against dev headers. From
// that landing on, any developer machine with a logged-in console stops
// answering to VITE_DEV_USER_ID / VITE_DEV_TENANT_ID / VITE_DEV_USER_ROLES for
// content requests, and starts answering to whoever the token says. When the
// two disagree about the tenant — and nothing keeps them in step — the console
// changes which tenant it operates on, silently and correctly.
func TestJWTMiddleware_BearerBeatsDevHeadersWhenBothPresent(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	signer := jwt.NewSigner(secret, time.Minute)
	tokenUser := uuid.New()
	token, _, err := signer.IssueAccessToken(tokenUser, []string{"member"}, "tenant-token", "editor", "", false)
	require.NoError(t, err)

	headerUser := uuid.New()
	var subject Subject
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		subject, _ = SubjectFromContext(r.Context())
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-User-Id", headerUser.String())
	req.Header.Set("X-User-Roles", "admin")
	req.Header.Set("X-Tenant-Id", "tenant-header")
	// Dev headers ALLOWED — the point is that permission to use them is not
	// enough, they also have to be reached.
	JWTMiddleware(signer, true, nil)(next).ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, tokenUser, subject.UserID, "the token decides who")
	assert.Equal(t, "tenant-token", subject.TenantID, "and which tenant")
	assert.NotContains(t, subject.Roles, "admin", "the header's role claim is not merged in")

	// The fallback is still live for a token that does not parse: an expired or
	// truncated one degrades to the dev headers rather than locking the console
	// out of a machine where they are trusted.
	subject = Subject{}
	stale := httptest.NewRequest(http.MethodGet, "/", nil)
	stale.Header.Set("Authorization", "Bearer not-a-jwt")
	stale.Header.Set("X-User-Id", headerUser.String())
	stale.Header.Set("X-User-Roles", "admin")
	JWTMiddleware(signer, true, nil)(next).ServeHTTP(httptest.NewRecorder(), stale)
	assert.Equal(t, headerUser, subject.UserID, "an unparseable bearer falls back, it does not refuse")
}
