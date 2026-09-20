package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// ?preset= is carried to the API verbatim and nothing else about the route
// changes (ADR-019): the edge holds no copy of the preset table, so a name it
// has never heard of still goes upstream, and the API's 400 comes back as the
// API's 400.
func TestGetMedia_ForwardsPresetVerbatim(t *testing.T) {
	signed := "https://storage.example/get/acme/abc.thumb.jpg?X-Amz-Signature=deadbeef"
	edge, st := newEdge(t, okJSON(`{"url":"`+signed+`","expires_at":"2026-01-01T00:00:00Z"}`), nil)

	id := uuid.NewString()
	rec := httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/acme/media/"+id+"?preset=thumb", nil))

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != signed {
		t.Fatalf("code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	if st.path != "/api/v1/content/media/"+id+"/url" || st.rawQuery != "preset=thumb" {
		t.Fatalf("upstream path=%q query=%q", st.path, st.rawQuery)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Fatalf("Cache-Control=%q", cc)
	}

	// Without the parameter the upstream request carries no query at all —
	// the edge invents no default.
	rec = httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/acme/media/"+id, nil))
	if st.rawQuery != "" {
		t.Fatalf("upstream query=%q want none", st.rawQuery)
	}
}

// The API refuses an unknown preset with 400 too; the edge refuses first, and
// for a reason beyond latency: the redirect target is built from what goes
// upstream, and nothing that reaches a redirect may be derived from request
// input. The value forwarded is the platform table's own string.
func TestGetMedia_UnknownPresetIsRefusedBeforeTheRoundTrip(t *testing.T) {
	edge, st := newEdge(t, status(http.StatusBadRequest,
		`{"error":{"code":"CONTENT_MEDIA_PRESET_UNKNOWN","message":"unknown media preset"}}`), nil)

	id := uuid.NewString()
	for _, bad := range []string{"huge", "original", "Thumb", "thumb%20"} {
		rec := httptest.NewRecorder()
		edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/acme/media/"+id+"?preset="+bad, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("preset=%s: code=%d want 400", bad, rec.Code)
		}
		if st.path != "" {
			t.Fatalf("preset=%s: the edge called upstream (%s) for a preset it can see is unknown — the redirect target must never be built from request input", bad, st.path)
		}
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

// ADR-006 Amendment 4: filters and the projection reach upstream verbatim,
// repeated parameters included, and nothing else is invented on the way.
func TestListEntries_ForwardsFiltersAndFieldsVerbatim(t *testing.T) {
	edge, st := newEdge(t, okJSON(`{"items":[]}`), nil)
	edge.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet,
		"/v1/acme/post?filter=title:eq:hello&filter=price:gte:10&fields=title,price&fields=slug", nil))

	q, err := url.ParseQuery(st.rawQuery)
	if err != nil {
		t.Fatal(err)
	}
	if got := q["filter"]; len(got) != 2 || got[0] != "title:eq:hello" || got[1] != "price:gte:10" {
		t.Fatalf("filter forwarded as %q — want both clauses, in order, unchanged", got)
	}
	if got := q["fields"]; len(got) != 2 || got[0] != "title,price" || got[1] != "slug" {
		t.Fatalf("fields forwarded as %q — want both values unchanged (upstream splits the CSV)", got)
	}
	// status and offset are the two this audience may never send: the Domain
	// API forces published-only, and delivery pages by cursor. sort is NOT in
	// that list any more (ADR-006 Amendment 5) — it is forwarded when asked
	// for, and absent when not, which is what this asserts.
	if q.Get("status") != "" || q.Get("sort") != "" || q.Get("offset") != "" {
		t.Fatalf("upstream query=%q — status/offset must never be forwarded, and sort must not be invented", st.rawQuery)
	}
}

// A filter the Domain API cannot parse is the caller's mistake, and the 400 it
// wrote must reach them as a 400 — not be folded into the 502 that hides
// upstream auth failures.
func TestListEntries_MalformedFilterIs400NotUpstreamError(t *testing.T) {
	edge, _ := newEdge(t, status(http.StatusBadRequest,
		`{"error":{"code":"CONTENT_FILTER_MALFORMED","message":"filter must be key:op:value"}}`), nil)
	rec := httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/acme/post?filter=title", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400: %s", rec.Code, rec.Body.String())
	}
}

// ADR-006 Amendment 5: sort is forwarded verbatim, not answered here. It used
// to be a 400 at the edge, because upstream refused it for this audience and a
// bare 403 would have arrived as an opaque 502. Now that upstream accepts it,
// the edge must NOT keep a copy of the rule: which keys are sortable is a
// property of the tenant's content type, which only upstream knows, so an edge
// that validated `sort` would either duplicate a schema it cannot see or reject
// requests the Domain API would have honoured.
func TestListEntries_ForwardsSortVerbatim(t *testing.T) {
	edge, st := newEdge(t, okJSON(`{"items":[]}`), nil)
	edge.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet,
		"/v1/acme/post?sort=price:desc&cursor=abc", nil))

	q, err := url.ParseQuery(st.rawQuery)
	if err != nil {
		t.Fatal(err)
	}
	if got := q.Get("sort"); got != "price:desc" {
		t.Fatalf("sort forwarded as %q, want price:desc (query=%q)", got, st.rawQuery)
	}
	// The cursor rides along untouched: a token is minted under one sort and
	// refused under another, so the edge must not drop either half of the pair.
	if got := q.Get("cursor"); got != "abc" {
		t.Fatalf("cursor forwarded as %q, want abc", got)
	}
	if st.calls != 1 {
		t.Fatalf("upstream calls=%d, want 1", st.calls)
	}
}

// A sort the Domain API refuses — an unknown key, an unsortable field, a
// direction that is not asc/desc — is the caller's mistake, and the 400 it
// wrote must reach them as a 400 rather than being folded into the 502 that
// hides upstream auth failures. Same contract as a malformed filter — the
// STATUS crosses the boundary; the edge substitutes its own generic code, which
// is a deliberate choice made elsewhere and not this parameter's business.
func TestListEntries_UnsortableFieldIs400NotUpstreamError(t *testing.T) {
	edge, _ := newEdge(t, status(http.StatusBadRequest,
		`{"error":{"code":"CONTENT_SORT_FIELD_UNKNOWN","message":"sort field is not defined"}}`), nil)
	rec := httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/acme/post?sort=nope:asc", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400: %s", rec.Code, rec.Body.String())
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

// ADR-006 Amendment 6: ?populate= is forwarded verbatim, repeated values
// included, and the edge invents nothing. Whether a key names a relation, and
// whether this credential may read the collection at the other end, are
// answers only the Domain API holds — an edge that kept a copy of that rule
// would refuse valid requests the moment a tenant edited a content type.
func TestListEntries_ForwardsPopulateVerbatim(t *testing.T) {
	edge, st := newEdge(t, okJSON(`{"items":[]}`), nil)
	edge.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet,
		"/v1/acme/post?populate=author,tags&populate=cover&fields=title", nil))

	q, err := url.ParseQuery(st.rawQuery)
	if err != nil {
		t.Fatal(err)
	}
	if got := q["populate"]; len(got) != 2 || got[0] != "author,tags" || got[1] != "cover" {
		t.Fatalf("populate forwarded as %q — want both values unchanged (upstream splits the CSV)", got)
	}
	if st.calls != 1 {
		t.Fatalf("upstream calls=%d, want 1", st.calls)
	}
}

// ADR-006 Amendment 8: a dotted path (`author.avatar`) is not a special case
// for the edge — identPattern only ever validated tenant, type and locale, so
// there was never a content check on populate values to relax. This pins that
// down rather than leaving it as an inference: the depth cap, the segment
// grammar and every refusal for it live entirely in the Domain API.
func TestListEntries_ForwardsDottedPopulate(t *testing.T) {
	edge, st := newEdge(t, okJSON(`{"items":[]}`), nil)
	edge.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet,
		"/v1/acme/post?populate=author.avatar&populate=author.mentor.avatar", nil))

	q, err := url.ParseQuery(st.rawQuery)
	if err != nil {
		t.Fatal(err)
	}
	got := q["populate"]
	if len(got) != 2 || got[0] != "author.avatar" || got[1] != "author.mentor.avatar" {
		t.Fatalf("populate forwarded as %q — want both dotted values unchanged", got)
	}
	if st.calls != 1 {
		t.Fatalf("upstream calls=%d, want 1", st.calls)
	}
}

// Not asked for, not invented. A `populate` the edge added would change every
// response body and every cache entry for callers who never requested it.
func TestListEntries_PopulateIsAbsentWhenNotAsked(t *testing.T) {
	edge, st := newEdge(t, okJSON(`{"items":[]}`), nil)
	edge.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/acme/post", nil))

	if q, _ := url.ParseQuery(st.rawQuery); q.Get("populate") != "" {
		t.Fatalf("upstream query=%q — populate must not be invented", st.rawQuery)
	}
}

// The single-entry route takes the same parameter. It used to drop everything
// but the type, so a populate on a detail page silently returned an unexpanded
// entry — the failure mode a 4xx exists to prevent.
func TestGetEntry_ForwardsPopulate(t *testing.T) {
	edge, st := newEdge(t, okJSON(`{}`), nil)
	id := uuid.New().String()
	edge.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet,
		"/v1/acme/post/"+id+"?populate=author&populate=tags", nil))

	q, err := url.ParseQuery(st.rawQuery)
	if err != nil {
		t.Fatal(err)
	}
	if got := q["populate"]; len(got) != 2 || got[0] != "author" || got[1] != "tags" {
		t.Fatalf("populate forwarded as %q — want both values", got)
	}
	if got := q.Get("type"); got != "post" {
		t.Fatalf("type=%q — the existing parameter must survive", got)
	}
}

// A populate key the Domain API refuses is the caller's mistake, and its status
// must cross the boundary rather than be folded into the 502 that hides
// upstream auth failures. Same contract as a malformed filter or an unsortable
// field.
func TestListEntries_UnknownPopulateFieldIs400NotUpstreamError(t *testing.T) {
	edge, _ := newEdge(t, status(http.StatusBadRequest,
		`{"error":{"code":"CONTENT_POPULATE_FIELD_UNKNOWN","message":"populate field is not defined"}}`), nil)
	rec := httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/acme/post?populate=nope", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400: %s", rec.Code, rec.Body.String())
	}
}

// A relation into a collection this credential may not read is a 403 upstream,
// and 403 is the one status the edge deliberately does NOT pass through — it
// would advertise that an authenticated API sits behind this. The caller gets
// the generic upstream failure instead, which is the existing ruling and is
// asserted here so a future "improvement" to populate errors has to face it.
func TestListEntries_ForbiddenPopulateTargetIsNotLeaked(t *testing.T) {
	edge, _ := newEdge(t, status(http.StatusForbidden,
		`{"error":{"code":"CONTENT_TYPE_READ_FORBIDDEN","message":"your role may not read entries of this type"}}`), nil)
	rec := httptest.NewRecorder()
	edge.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/acme/post?populate=secret", nil))

	if rec.Code == http.StatusForbidden {
		t.Fatalf("a 403 reached the public unchanged: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "CONTENT_TYPE_READ_FORBIDDEN") {
		t.Fatalf("upstream error code leaked: %s", rec.Body.String())
	}
}
