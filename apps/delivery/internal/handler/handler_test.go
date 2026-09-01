package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/apps/delivery/internal/upstream"
	"github.com/williamlabdev/saas-forge/internal/auth/jwt"
)

var deliveryKey = []byte("delivery-secret-at-least-32-bytes!!!")

// newEdge wires a Handler against a stub Domain API. The stub records what the
// edge actually sent upstream, which is where the interesting assertions are.
func newEdge(t *testing.T, upstreamHandler http.HandlerFunc, limiter Limiter) (http.Handler, *stub) {
	t.Helper()
	st := &stub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.record(r)
		upstreamHandler(w, r)
	}))
	t.Cleanup(srv.Close)

	signer := jwt.NewSigner(nil, time.Minute).WithDeliveryKey(deliveryKey)
	up := upstream.NewClient(srv.URL, "gw-secret", signer)
	h := New(up, limiter, 60*time.Second)

	r := chi.NewRouter()
	h.Routes(r)
	return r, st
}

type stub struct {
	path       string
	rawQuery   string
	authHeader string
	gateway    string
	calls      int
}

func (s *stub) record(r *http.Request) {
	s.path = r.URL.Path
	s.rawQuery = r.URL.RawQuery
	s.authHeader = r.Header.Get("Authorization")
	s.gateway = r.Header.Get("X-Gateway-Secret")
	s.calls++
}

func okJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":` + body + `}`))
	}
}

func status(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
}

func TestListEntries_ProxiesAndCaches(t *testing.T) {
	// The stub's body is a stand-in the edge passes through untouched, but it
	// should still be a shape the Domain API can actually produce: delivery pages
	// by cursor and reports no `total` (ADR-004 Amendment 4).
	edge, st := newEdge(t, okJSON(`{"items":[],"limit":20,"has_more":false}`), nil)

	rec := httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/acme/post?limit=5", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Fatalf("Cache-Control=%q — published content must be shared-cacheable", got)
	}
	if st.path != "/api/v1/content/entries" {
		t.Fatalf("upstream path=%q", st.path)
	}
	if st.gateway != "gw-secret" {
		t.Fatalf("gateway secret not forwarded: %q", st.gateway)
	}
}

// The credential the edge mints must be scoped to the tenant in the PATH and
// carry the delivery marker — that is what makes the Domain API force
// published-only.
func TestListEntries_MintsTenantScopedDeliveryCredential(t *testing.T) {
	edge, st := newEdge(t, okJSON(`{"items":[]}`), nil)
	edge.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/acme/post", nil))

	const prefix = "Bearer "
	if len(st.authHeader) <= len(prefix) {
		t.Fatalf("no bearer sent upstream: %q", st.authHeader)
	}
	verifier := jwt.NewSigner([]byte("main-secret-at-least-32-bytes-long!!"), time.Minute).
		WithDeliveryKey(deliveryKey)
	claims, err := verifier.ParseAccessToken(st.authHeader[len(prefix):])
	if err != nil {
		t.Fatalf("minted token does not verify: %v", err)
	}
	if !claims.Delivery {
		t.Fatal("minted token must carry the delivery marker")
	}
	if claims.TenantID != "acme" {
		t.Fatalf("tenant=%q want acme — the credential must be scoped to the path tenant", claims.TenantID)
	}
	if claims.TenantRole != "viewer" {
		t.Fatalf("role=%q want viewer", claims.TenantRole)
	}
}

// The edge must never ask upstream for a status. Asking would imply it is
// trusted to choose; the Domain API forces published-only regardless.
func TestListEntries_NeverRequestsAStatus(t *testing.T) {
	edge, st := newEdge(t, okJSON(`{"items":[]}`), nil)
	edge.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/v1/acme/post?status=draft", nil))

	if q := st.rawQuery; q != "type=post" {
		t.Fatalf("upstream query=%q — a caller-supplied status must not be forwarded", q)
	}
}

func TestGetEntry_NotFoundPassesThrough(t *testing.T) {
	edge, _ := newEdge(t, status(http.StatusNotFound, `{"error":{"code":"NOT_FOUND","message":"x"}}`), nil)

	rec := httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/acme/post/abc", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control=%q — errors must not be cached", cc)
	}
}

// A refused delivery credential is an edge misconfiguration. Reporting 401/403
// verbatim would advertise that an authenticated API sits behind this.
func TestUpstreamAuthFailure_IsNotLeakedToThePublic(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		edge, _ := newEdge(t, status(code, `{"error":{"code":"FORBIDDEN","message":"nope"}}`), nil)
		rec := httptest.NewRecorder()
		edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/acme/post", nil))

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("upstream %d surfaced as %d — must be masked", code, rec.Code)
		}
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if e, ok := body["error"].(map[string]any); ok && e["code"] == "FORBIDDEN" {
			t.Fatal("upstream error code leaked to the public surface")
		}
	}
}

func TestRateLimit_PerTenant(t *testing.T) {
	edge, _ := newEdge(t, okJSON(`{"items":[]}`), NewLimiter(2, time.Hour))

	for i := range 2 {
		rec := httptest.NewRecorder()
		edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/acme/post", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: code=%d", i, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/acme/post", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}

	// A different tenant has its own budget — one tenant's traffic must not
	// deny service to another.
	rec = httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/other/post", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("second tenant code=%d — limits must be per tenant", rec.Code)
	}
}

// Malformed identifiers are rejected before the limiter, so a flood of junk
// slugs cannot inflate its key space, and never reach upstream.
func TestInvalidPath_RejectedBeforeUpstream(t *testing.T) {
	for _, target := range []string{
		"/v1/acme/po%20st",       // whitespace in the type name
		"/v1/9acme/post",         // slug must not start with a digit
		"/v1/acme/post%21",       // punctuation outside the identifier grammar
		"/v1/ac.me/post",         // dots are not part of the grammar
		"/v1/acme%2F..%2Fx/post", // encoded traversal in the tenant segment
	} {
		edge, st := newEdge(t, okJSON(`{}`), nil)
		rec := httptest.NewRecorder()
		edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))

		if rec.Code == http.StatusOK {
			t.Fatalf("%s: accepted a malformed path", target)
		}
		if st.calls != 0 {
			t.Fatalf("%s: reached upstream %d times", target, st.calls)
		}
	}
}

func TestHealth(t *testing.T) {
	edge, _ := newEdge(t, okJSON(`{}`), nil)
	rec := httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}

// Locale IS the caller's choice — unlike status, every locale of a published
// entry is equally public — so it must reach upstream.
func TestListEntries_ForwardsLocale(t *testing.T) {
	edge, st := newEdge(t, okJSON(`{"items":[]}`), nil)
	edge.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/v1/acme/post?locale=zh-TW&status=draft", nil))

	if st.rawQuery != "locale=zh-TW&type=post" {
		t.Fatalf("upstream query=%q — locale must be forwarded and status must not", st.rawQuery)
	}
}

// A malformed locale is rejected at the edge: it would otherwise become part of
// the shared cache key.
func TestListEntries_RejectsMalformedLocale(t *testing.T) {
	edge, st := newEdge(t, okJSON(`{}`), nil)
	rec := httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/acme/post?locale=zh%20TW", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
	if st.calls != 0 {
		t.Fatalf("reached upstream %d times", st.calls)
	}
}

// Media is a redirect to a signed URL, never a proxy of the bytes.
func TestGetMedia_RedirectsToSignedURL(t *testing.T) {
	signed := "https://storage.example/get/acme/abc?X-Amz-Signature=deadbeef"
	edge, st := newEdge(t, okJSON(`{"url":"`+signed+`","expires_at":"2026-01-01T00:00:00Z"}`), nil)

	id := uuid.NewString()
	rec := httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/acme/media/"+id, nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("code=%d want 302 (redirect, not proxy)", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != signed {
		t.Fatalf("Location=%q want the signed URL", got)
	}
	// A signed, expiring URL must never be pinned into a shared cache.
	if cc := rec.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Fatalf("Cache-Control=%q — a credentialed URL must not be shared-cacheable", cc)
	}
	if st.path != "/api/v1/content/media/"+id+"/url" {
		t.Fatalf("upstream path=%q", st.path)
	}
}

// The API decides publishability; a refusal must surface as a plain 404 without
// revealing that an authenticated API sits behind the edge.
func TestGetMedia_UnpublishedIsNotFound(t *testing.T) {
	edge, _ := newEdge(t, status(http.StatusNotFound, `{"error":{"code":"NOT_FOUND","message":"x"}}`), nil)
	rec := httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/acme/media/"+uuid.NewString(), nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("errors must not be cached")
	}
}

func TestGetMedia_RejectsNonUUID(t *testing.T) {
	edge, st := newEdge(t, okJSON(`{}`), nil)
	rec := httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/acme/media/not-a-uuid", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
	if st.calls != 0 {
		t.Fatalf("reached upstream %d times", st.calls)
	}
}

// The cursor is forwarded verbatim. Normalising or re-encoding it at the edge
// would hand upstream a token it never issued.
func TestListEntries_ForwardsCursorVerbatim(t *testing.T) {
	edge, st := newEdge(t, okJSON(`{"items":[]}`), nil)
	const tok = "eyJ0IjoiMjAyNi0wNy0zMVQxMjowMDowMFoiLCJpIjoiYWJjIn0"
	edge.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/v1/acme/post?cursor="+tok, nil))

	if !strings.Contains(st.rawQuery, "cursor="+tok) {
		t.Fatalf("upstream query=%q — cursor must be forwarded unchanged", st.rawQuery)
	}
}

// offset is answered here rather than forwarded: upstream refuses it for a
// delivery credential, and a bare 403 would not tell the caller what to do.
func TestListEntries_RejectsOffsetWithAnActionableError(t *testing.T) {
	edge, st := newEdge(t, okJSON(`{}`), nil)
	rec := httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/acme/post?offset=20", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "next_cursor") {
		t.Fatalf("error must point at the replacement: %s", rec.Body.String())
	}
	if st.calls != 0 {
		t.Fatalf("reached upstream %d times — the edge should answer this itself", st.calls)
	}
}

// --- freshness / revalidation (ADR-011 §快取失效) -----------------------------

// newEdgeMaxAge is newEdge with a caller-chosen Cache-Control lifetime.
func newEdgeMaxAge(t *testing.T, upstreamHandler http.HandlerFunc, maxAge time.Duration) http.Handler {
	t.Helper()
	srv := httptest.NewServer(upstreamHandler)
	t.Cleanup(srv.Close)
	signer := jwt.NewSigner(nil, time.Minute).WithDeliveryKey(deliveryKey)
	up := upstream.NewClient(srv.URL, "gw-secret", signer)
	h := New(up, nil, maxAge)
	r := chi.NewRouter()
	h.Routes(r)
	return r
}

func TestFreshness_DefaultIsRevalidateEveryUse(t *testing.T) {
	// max-age=0 must not mean "uncacheable" and must not mean "fresh for 0
	// seconds with no validator" — it means cache, but ask first. This is what
	// makes an unpublish visible on the NEXT request instead of TTL expiry.
	edge := newEdgeMaxAge(t, okJSON(`{"items":[]}`), 0)
	rec := httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/acme/post", nil))

	if got := rec.Header().Get("Cache-Control"); got != "public, no-cache" {
		t.Fatalf("Cache-Control=%q, want public, no-cache", got)
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("no ETag — no-cache without a validator makes every revalidation a full 200")
	}
}

func TestFreshness_ETagRoundTripsAs304(t *testing.T) {
	edge := newEdgeMaxAge(t, okJSON(`{"items":[{"id":"e1"}]}`), 0)

	first := httptest.NewRecorder()
	edge.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/v1/acme/post", nil))
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the 200")
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/acme/post", nil)
	req.Header.Set("If-None-Match", etag)
	second := httptest.NewRecorder()
	edge.ServeHTTP(second, req)

	if second.Code != http.StatusNotModified {
		t.Fatalf("code=%d, want 304", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Fatalf("304 carried a body: %q", second.Body)
	}
	if got := second.Header().Get("ETag"); got != etag {
		t.Fatalf("the validator must ride the 304 (got %q) — some caches drop the entry without it", got)
	}
}

func TestFreshness_ChangedContentDefeatsTheStaleValidator(t *testing.T) {
	// The exchange that retires stale copies: after an unpublish changes the
	// body, yesterday's ETag must earn a fresh 200, not a 304.
	bodies := []string{`{"items":[{"id":"e1"}]}`, `{"items":[]}`}
	var call int
	edge := newEdgeMaxAge(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":` + bodies[min(call, 1)] + `}`))
		call++
	}, 0)

	first := httptest.NewRecorder()
	edge.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/v1/acme/post", nil))

	req := httptest.NewRequest(http.MethodGet, "/v1/acme/post", nil)
	req.Header.Set("If-None-Match", first.Header().Get("ETag"))
	second := httptest.NewRecorder()
	edge.ServeHTTP(second, req)

	if second.Code != http.StatusOK {
		t.Fatalf("code=%d — a stale validator must be answered with the new content", second.Code)
	}
	if second.Body.Len() == 0 {
		t.Fatal("the fresh 200 carried no body")
	}
}

func TestFreshness_WeakComparisonMatches(t *testing.T) {
	// RFC 9110 §13.1.2: If-None-Match uses weak comparison, so W/"x" matches
	// "x". Refusing it would only cost 200s where 304s were owed.
	edge := newEdgeMaxAge(t, okJSON(`{"items":[]}`), 0)
	first := httptest.NewRecorder()
	edge.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/v1/acme/post", nil))

	req := httptest.NewRequest(http.MethodGet, "/v1/acme/post", nil)
	req.Header.Set("If-None-Match", `W/`+first.Header().Get("ETag")+`, "something-else"`)
	second := httptest.NewRecorder()
	edge.ServeHTTP(second, req)
	if second.Code != http.StatusNotModified {
		t.Fatalf("code=%d, want 304 for a weak-prefixed match in a list", second.Code)
	}
}

func TestFreshness_TTLModeStillCarriesTheValidator(t *testing.T) {
	// Positive max-age is the CDN posture — but expiry-time revalidation still
	// deserves a 304, so the ETag must be present in this mode too.
	edge := newEdgeMaxAge(t, okJSON(`{"items":[]}`), 60*time.Second)
	rec := httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/acme/post", nil))

	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Fatalf("Cache-Control=%q", got)
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("TTL mode dropped the validator")
	}
}

func TestFreshness_ErrorsCarryNoValidator(t *testing.T) {
	edge := newEdgeMaxAge(t, status(http.StatusNotFound, `{}`), 0)
	rec := httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/acme/post/00000000-0000-0000-0000-000000000000", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d", rec.Code)
	}
	if rec.Header().Get("ETag") != "" {
		t.Fatal("an error response must not hand out a validator")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q — errors are never cached", got)
	}
}

// --- preview links -----------------------------------------------------------

// previewToken is a real delivery-key token with a preview claim. Minted rather
// than faked because the assertion below is that the edge forwards it BYTE FOR
// BYTE: a fixture string would still prove that, but a real one also proves the
// edge does not need to be able to read what it forwards.
func previewToken(t *testing.T, tenant string, entry uuid.UUID) string {
	t.Helper()
	signer := jwt.NewSigner(nil, time.Minute).WithDeliveryKey(deliveryKey)
	tok, _, err := signer.IssuePreviewToken(uuid.New(), tenant, entry)
	if err != nil {
		t.Fatalf("mint preview token: %v", err)
	}
	return tok
}

// The wiring, end to end at the edge: the caller's token goes upstream in place
// of the edge's own minted credential. If the edge minted its own here, the
// Domain API would see an ordinary delivery subject and answer with the
// published snapshot — a preview that silently shows live content is worse than
// one that errors, because nothing about the response says so.
func TestPreview_ForwardsCallerTokenInsteadOfMinting(t *testing.T) {
	entry := uuid.New()
	tok := previewToken(t, "acme", entry)
	edge, st := newEdge(t, okJSON(`{"id":"x","data":{"title":"draft"}}`), nil)

	rec := httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/acme/post/"+entry.String()+"?preview_token="+tok, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if st.authHeader != "Bearer "+tok {
		t.Fatalf("upstream Authorization = %q, want the caller's token forwarded verbatim", st.authHeader)
	}
	// The preview token must not leak into the upstream URL as well — it belongs
	// in the header, and a query-string copy would land in upstream access logs.
	if strings.Contains(st.rawQuery, "preview_token") {
		t.Fatalf("preview token forwarded in the query string: %q", st.rawQuery)
	}
}

// A draft served against a bearer credential that lives in the URL must not be
// storable by anything. `no-cache` would not do: it permits storage and only
// forces revalidation, and after the 30-minute token expires the revalidation
// fails while the stored copy remains.
func TestPreview_IsNeverStored(t *testing.T) {
	entry := uuid.New()
	edge, _ := newEdge(t, okJSON(`{"id":"x"}`), nil)

	rec := httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v1/acme/post/"+entry.String()+"?preview_token="+previewToken(t, "acme", entry), nil))

	if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q, want \"private, no-store\"", got)
	}
	// An ETag is an invitation to store what was just forbidden to be stored.
	if got := rec.Header().Get("ETag"); got != "" {
		t.Fatalf("ETag = %q on a preview response, want none", got)
	}

	// The published route is untouched by preview's arrival — otherwise a
	// regression that made every response no-store would pass the check above.
	rec = httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/acme/post/"+entry.String(), nil))
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Fatalf("published Cache-Control = %q, want it unchanged", got)
	}
}

// Every JSON body this edge emits is editor-authored, so `Content-Type:
// application/json` is load-bearing rather than cosmetic: it is the only thing
// standing between "an entry contains markup" and "a browser runs it". A
// declared type a client may second-guess is not a guarantee, and nosniff is
// what turns it into one.
//
// The three responses are checked separately because they leave the handler by
// three different paths — writeJSON's 200, writeJSON's early 304 return, and
// writePreview, which deliberately shares no code with writeJSON. A header set
// in one says nothing about the other two.
func TestNoSniff_OnEveryJSONResponse(t *testing.T) {
	const want = "nosniff" // spelled out, not read back from the handler

	entry := uuid.New()
	edge, _ := newEdge(t, okJSON(`{"id":"x"}`), nil)

	published := httptest.NewRecorder()
	edge.ServeHTTP(published, httptest.NewRequest(http.MethodGet, "/v1/acme/post/"+entry.String(), nil))
	if got := published.Header().Get("X-Content-Type-Options"); got != want {
		t.Fatalf("published 200: X-Content-Type-Options = %q, want %q", got, want)
	}

	// The 304 is the one that is easy to lose: it returns before the body is
	// written, and a cache replays its STORED body under these headers. A
	// nosniff that only rode along with 200s would be missing exactly when the
	// content is served from a cache instead of from here.
	etag := published.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the 200 — the 304 leg below cannot be exercised")
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/acme/post/"+entry.String(), nil)
	req.Header.Set("If-None-Match", etag)
	revalidated := httptest.NewRecorder()
	edge.ServeHTTP(revalidated, req)
	if revalidated.Code != http.StatusNotModified {
		t.Fatalf("code = %d, want 304 — the revalidation leg did not happen", revalidated.Code)
	}
	if got := revalidated.Header().Get("X-Content-Type-Options"); got != want {
		t.Fatalf("304: X-Content-Type-Options = %q, want %q", got, want)
	}

	// Preview carries the working copy — the least reviewed content the edge
	// ever emits, and the one most likely to hold whatever an editor just
	// pasted in.
	preview := httptest.NewRecorder()
	edge.ServeHTTP(preview, httptest.NewRequest(http.MethodGet,
		"/v1/acme/post/"+entry.String()+"?preview_token="+previewToken(t, "acme", entry), nil))
	if got := preview.Header().Get("X-Content-Type-Options"); got != want {
		t.Fatalf("preview: X-Content-Type-Options = %q, want %q", got, want)
	}
}

// Routes that cannot honour a preview token say so, rather than letting the
// Domain API's 403 arrive as a generic 502 — and rather than ignoring the
// parameter and serving the published snapshot as if nothing was asked for.
func TestPreview_RefusedOnRoutesThatCannotHonourIt(t *testing.T) {
	entry := uuid.New()
	tok := previewToken(t, "acme", entry)
	edge, st := newEdge(t, okJSON(`{"items":[]}`), nil)

	for name, path := range map[string]string{
		"list":  "/v1/acme/post?preview_token=" + tok,
		"media": "/v1/acme/media/" + uuid.New().String() + "?preview_token=" + tok,
	} {
		t.Run(name, func(t *testing.T) {
			before := st.calls
			rec := httptest.NewRecorder()
			edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code=%d body=%s, want 400", rec.Code, rec.Body)
			}
			var body struct {
				Error struct{ Code string } `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Error.Code != "PREVIEW_NOT_SUPPORTED" {
				t.Fatalf("code = %q, want PREVIEW_NOT_SUPPORTED", body.Error.Code)
			}
			// Refused at the edge means upstream was never asked — a token that
			// reached the Domain API on these routes would be metered against the
			// tenant before being refused.
			if st.calls != before {
				t.Fatalf("upstream was called %d time(s) for a refused preview", st.calls-before)
			}
		})
	}
}

// Preview traffic is capped per TOKEN. The tenant cap cannot do this job: the
// edge does not decode the token, so the tenant in the path is unverified, and a
// bearer that walks it across arbitrary tenants gets a fresh bucket each time
// while upstream meters every read against the token's real tenant.
func TestPreview_CappedPerTokenNotPerPathTenant(t *testing.T) {
	entry := uuid.New()
	tok := previewToken(t, "acme", entry)
	// Two allowed per key. The tenant in the path differs on every request, so a
	// tenant-keyed cap alone would never fire.
	edge, _ := newEdge(t, okJSON(`{"id":"x"}`), NewLimiter(2, time.Hour))

	codes := make([]int, 0, 3)
	for _, tenant := range []string{"acme", "beta", "gamma"} {
		rec := httptest.NewRecorder()
		edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
			"/v1/"+tenant+"/post/"+entry.String()+"?preview_token="+tok, nil))
		codes = append(codes, rec.Code)
	}
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Fatalf("first two requests: %v, want both 200", codes)
	}
	if codes[2] != http.StatusTooManyRequests {
		t.Fatalf("third request on a third tenant path: %d, want 429 — the cap follows the token", codes[2])
	}
}
