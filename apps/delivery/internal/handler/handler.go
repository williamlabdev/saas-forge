// Package handler is the public read surface of the content delivery edge
// (ADR-004 option A, phase 1): a platform-owned host with the tenant in the
// path. Everything it serves is content a tenant deliberately published, so a
// guessable tenant identifier is not an isolation failure — but load IS
// attributable to that tenant, hence the per-tenant cap.
package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/apps/delivery/internal/upstream"
	"github.com/williamlabdev/saas-forge/apps/delivery/openapi"
	"github.com/williamlabdev/saas-forge/internal/pkg/mediapreset"
	"github.com/williamlabdev/saas-forge/internal/pkg/ratelimit"
)

// Limiter is the per-tenant request cap (satisfied by ratelimit.IPLimiter).
type Limiter interface{ Allow(key string) bool }

type Handler struct {
	up          *upstream.Client
	limiter     Limiter
	cacheMaxAge time.Duration
}

func New(up *upstream.Client, limiter Limiter, cacheMaxAge time.Duration) *Handler {
	return &Handler{up: up, limiter: limiter, cacheMaxAge: cacheMaxAge}
}

// identPattern matches the tenant slug and content-type name grammar the Domain
// API already enforces (domain.ValidName). Rejecting non-matching input here
// keeps junk out of the rate-limiter key space and out of upstream URLs.
var identPattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)

func (h *Handler) Routes(r chi.Router) {
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// The spec of everything above and below: public, unauthenticated, and
	// cacheable by an intermediary for an hour — it changes only on deploy.
	// apps/delivery/internal/handler/openapi_test.go fails the build the day
	// this drifts from the routes and handlers actually below it.
	r.Get("/v1/openapi.json", h.getOpenAPIJSON)
	r.Get("/v1/openapi.yaml", h.getOpenAPIYAML)
	// Media is scoped by tenant but not by content type — an asset belongs to
	// the tenant, and may be referenced from several types.
	r.Get("/v1/{tenant}/media/{id}", h.getMedia)
	r.Route("/v1/{tenant}/{type}", func(r chi.Router) {
		r.Get("/", h.listEntries)
		r.Get("/{id}", h.getEntry)
	})
}

func (h *Handler) listEntries(w http.ResponseWriter, r *http.Request) {
	tenant, typeName, ok := h.scope(w, r)
	if !ok {
		return
	}
	if h.refusePreview(w, r) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	// A malformed locale is rejected here rather than forwarded — it would also
	// become part of the cache key.
	locale := r.URL.Query().Get("locale")
	if locale != "" && !identPattern.MatchString(locale) {
		writeError(w, http.StatusBadRequest, "INVALID_LOCALE", "locale must be a language tag")
		return
	}
	// The public path pages by cursor: the Domain API refuses offset for a
	// delivery credential, so forwarding one would turn a caller's mistake into
	// an opaque 403 from upstream. Answering here names the actual problem.
	if r.URL.Query().Get("offset") != "" {
		writeError(w, http.StatusBadRequest, "OFFSET_UNSUPPORTED", "this API pages by cursor — follow next_cursor instead of offset")
		return
	}
	// Opaque round-trip token. The edge does not decode it; validating its shape
	// here would couple the edge to an encoding the Domain API owns — and since
	// ADR-006 Amendment 5 the token's contents depend on `sort`, which is
	// exactly the kind of change an edge that parsed it would have to be
	// redeployed for.
	cursor := r.URL.Query().Get("cursor")

	// Filters, the sort and the field projection are forwarded verbatim,
	// repeated parameters included. The Domain API owns all three grammars and
	// evaluates the filters AND the ORDER BY against the PUBLISHED snapshot for
	// a delivery credential (ADR-006 Amendments 4 and 5), so nothing a caller
	// writes here can probe a draft — not through which rows come back, and not
	// through the order they come back in. A clause it cannot parse comes back
	// as its 400, which writeUpstreamError passes through; sort in particular is
	// NOT validated here, because the sortable keys are a property of the
	// tenant's content type, which only upstream knows.
	//
	// All of them become part of the cache key by way of the URL, which is
	// correct: a different sort is a different page, and a cursor minted under
	// one sort is refused under another.
	data, err := h.up.ListEntries(r.Context(), tenant, upstream.ListEntriesParams{
		Type:    typeName,
		Locale:  locale,
		Cursor:  cursor,
		Limit:   limit,
		Sort:    r.URL.Query().Get("sort"),
		Filters: r.URL.Query()["filter"],
		Fields:  r.URL.Query()["fields"],
		// Relation expansion (ADR-006 Amendment 6), forwarded verbatim like the
		// three above. Which keys are populatable is a property of the tenant's
		// content type — and of what the credential may read at the other end —
		// so the edge cannot validate it without holding a copy of the schema
		// and of the permission rules. Part of the cache key by way of the URL,
		// which is correct: a populated page is a different response.
		Populate: r.URL.Query()["populate"],
		// Free-text search (ADR-021), forwarded verbatim like Filters and Sort
		// and for the identical reason — the Domain API reads
		// published_search_text for this audience, so nothing here can reach a
		// draft any more than a filter clause could.
		Query: r.URL.Query().Get("q"),
	})
	if err != nil {
		h.writeUpstreamError(w, err)
		return
	}
	h.writeJSON(w, r, http.StatusOK, data)
}

func (h *Handler) getEntry(w http.ResponseWriter, r *http.Request) {
	tenant, typeName, ok := h.scope(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")

	// The one route a preview link may address: a preview credential names a
	// single entry, and this is the endpoint that serves a single entry.
	if token := r.URL.Query().Get(previewTokenParam); token != "" {
		if !h.allowPreview(w, token) {
			return
		}
		data, err := h.up.GetEntryPreview(r.Context(), typeName, id, token)
		if err != nil {
			h.writeUpstreamError(w, err)
			return
		}
		h.writePreview(w, data)
		return
	}

	// The single-entry route takes ?populate= too — the same parameter and the
	// same forwarding rule as the list. It is read AFTER the preview branch
	// above returns, because a preview credential is refused populate upstream
	// (it addresses one entry) and a preview link that also carried a populate
	// should get that refusal, not have the parameter quietly dropped here.
	data, err := h.up.GetEntry(r.Context(), tenant, typeName, id, r.URL.Query()["populate"])
	if err != nil {
		h.writeUpstreamError(w, err)
		return
	}
	h.writeJSON(w, r, http.StatusOK, data)
}

// previewTokenParam is the query parameter that carries a preview credential.
// A query parameter and not a header, because the whole point of a preview link
// is that it can be pasted into a message and opened in a browser — which is
// also why it is refused everywhere except the single-entry route below, and why
// its response is never stored.
const previewTokenParam = "preview_token"

// refusePreview rejects a preview token on a route that cannot honour one. The
// Domain API already refuses these (403 CONTENT_PREVIEW_SCOPE_EXCEEDED) and that
// is the gate that matters — but the edge maps an upstream 403 to a generic 502,
// so without this the caller's diagnosable mistake would arrive as "content is
// temporarily unavailable". Answering here names the actual problem, exactly as
// the offset rejection above does.
//
// Ignoring the parameter instead is the option NOT taken: a caller who pasted a
// preview link into a list URL would get the published snapshot and no
// indication that the draft they were sent is missing from it.
func (h *Handler) refusePreview(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Query().Get(previewTokenParam) == "" {
		return false
	}
	writeError(w, http.StatusBadRequest, "PREVIEW_NOT_SUPPORTED",
		"a preview token addresses exactly one entry — open it at /v1/{tenant}/{type}/{id}")
	return true
}

// allowPreview caps preview traffic PER TOKEN rather than per tenant.
//
// The tenant cap in scope() is keyed on the tenant in the URL, and for a
// forwarded credential that identifier is not evidence of anything: the edge
// does not decode the token, so a bearer can spread requests across arbitrary
// tenant paths and land in a fresh limiter bucket every time, while upstream
// meters every read against the token's real tenant. Keying on the token closes
// that: whatever path it is presented at, one link gets one budget.
//
// The key is a hash, not the token, so a limiter's key space — which may be
// logged or exported as metric labels — never holds a live credential.
func (h *Handler) allowPreview(w http.ResponseWriter, token string) bool {
	if h.limiter == nil {
		return true
	}
	sum := sha256.Sum256([]byte(token))
	if h.limiter.Allow("preview:" + hex.EncodeToString(sum[:])) {
		return true
	}
	w.Header().Set("Retry-After", "60")
	writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests for this preview link")
	return false
}

// writePreview serves a preview response. It deliberately does NOT go through
// writeJSON: everything writeJSON does is about being cacheable, and none of it
// is safe here.
//
// no-store, not merely no-cache: this body is one tenant's UNPUBLISHED working
// copy, delivered against a bearer credential that sits in the URL. A shared
// cache that stored it keyed by URL would serve the draft to anyone who guessed
// or was forwarded that URL — after the token expired, and after the entry was
// deleted. `private` alone would still permit a browser cache to keep it in a
// shared machine's profile.
//
// No ETag either. The validator exists so a cache can revalidate what it stored,
// and nothing here is allowed to store anything; emitting one would invite
// precisely the storage the header above forbids. A draft under active editing
// is also the worst possible candidate for a 304.
func (h *Handler) writePreview(w http.ResponseWriter, data json.RawMessage) {
	if len(data) == 0 {
		data = json.RawMessage("null")
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	// The body is editor-authored content, so gosec's taint analysis calls this
	// an XSS sink. It is not one: this endpoint answers `application/json`, and
	// the nosniff above is what makes that a rule rather than a convention — a
	// browser may no longer disregard the declared type and re-interpret a
	// crafted body as HTML. Sniffing was the only path from "content contains
	// markup" to "markup executes", and it is closed.
	_, _ = w.Write(data) //#nosec G705 -- served as application/json with X-Content-Type-Options: nosniff, so it is never interpreted as HTML
}

// getMedia redirects to a short-lived signed URL instead of proxying bytes:
// media traffic must not run through this process, and the signed URL is what
// keeps the bucket private.
func (h *Handler) getMedia(w http.ResponseWriter, r *http.Request) {
	tenant := chi.URLParam(r, "tenant")
	if !identPattern.MatchString(tenant) {
		writeError(w, http.StatusBadRequest, "INVALID_PATH", "tenant must be an identifier")
		return
	}
	if h.limiter != nil && !h.limiter.Allow(tenant) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests for this tenant")
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_PATH", "media id must be a uuid")
		return
	}
	// Media is refused a preview token for a reason the list route does not have:
	// a preview credential IS a delivery credential, so forwarding one here would
	// have worked — and would have quietly published "which assets exist" to
	// whoever holds a preview link, on a route the token was never scoped to.
	// Preview images therefore resolve under delivery's own rules or not at all
	// (william 0802: 媒體沿用 delivery 規則).
	if h.refusePreview(w, r) {
		return
	}

	// ?preset= selects a rendition. The API falls back to the original for
	// one that is not ready, so this route is always a 302 to SOMETHING
	// readable — never a 404 for a thumbnail that is still being made
	// (ADR-019 §8). The value forwarded upstream is taken from the platform's
	// own preset table, not from the query string: what reaches the redirect
	// below must never be derived from request input, and an unknown preset
	// is a template bug worth failing loudly here rather than after a round
	// trip (the API would refuse it with the same status).
	preset, ok := mediaPreset(r.URL.Query().Get("preset"))
	if !ok {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "unknown media preset")
		return
	}
	signed, err := h.up.ResolveMedia(r.Context(), tenant, id, preset)
	if err != nil {
		h.writeUpstreamError(w, err)
		return
	}
	// NOT shared-cacheable: the target carries a credential and expires, so a
	// cache would hand a stale or unusable URL to the next visitor — and would
	// keep serving content that has since been unpublished.
	w.Header().Set("Cache-Control", "private, no-store")
	http.Redirect(w, r, signed, http.StatusFound)
}

// mediaPreset canonicalises a ?preset= value against the platform table in
// internal/pkg/mediapreset. The string returned is the table's own, so the
// redirect target built from it downstream carries nothing the request
// supplied. Empty means "the original" and is passed through as such. The
// edge deliberately imports that stdlib-only package and not the cms domain
// (ADR-004: thin edge).
//
// The loop stays inline on purpose: gosec G710 follows taint across a call,
// so a helper that took raw and returned a string would still look like the
// request reaching http.Redirect. Ranging over Names() and returning the
// element is what breaks the chain.
func mediaPreset(raw string) (string, bool) {
	if raw == "" {
		return "", true
	}
	for _, name := range mediapreset.Names() {
		if name == raw {
			return name, true
		}
	}
	return "", false
}

// getOpenAPIJSON and getOpenAPIYAML serve the embedded spec (apps/delivery/openapi).
// No auth, no tenant scope, no rate limit — the document is the same fixed
// bytes for every caller, and this route is not proxying anything a tenant's
// load should be attributed against.
//
// max-age=3600, not the request-scoped ETag machinery writeJSON uses for
// entries: the spec changes on deploy, not per request, so a plain hour-long
// TTL is the whole freshness story — there is no upstream state to revalidate
// against between deploys.
func (h *Handler) getOpenAPIJSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openapi.JSON)
}

func (h *Handler) getOpenAPIYAML(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openapi.YAML)
}

// scope validates the path identifiers and applies the per-tenant cap. It
// validates BEFORE limiting so a flood of malformed slugs cannot inflate the
// limiter's key space.
func (h *Handler) scope(w http.ResponseWriter, r *http.Request) (tenant, typeName string, ok bool) {
	tenant = chi.URLParam(r, "tenant")
	typeName = chi.URLParam(r, "type")
	if !identPattern.MatchString(tenant) || !identPattern.MatchString(typeName) {
		writeError(w, http.StatusBadRequest, "INVALID_PATH", "tenant and type must be identifiers")
		return "", "", false
	}
	if h.limiter != nil && !h.limiter.Allow(tenant) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests for this tenant")
		return "", "", false
	}
	return tenant, typeName, true
}

func (h *Handler) writeJSON(w http.ResponseWriter, r *http.Request, status int, data json.RawMessage) {
	if len(data) == 0 {
		data = json.RawMessage("null")
	}
	// Published content is public and identical for every caller, so it is
	// cacheable by shared caches. Vary is deliberately absent: the response
	// varies by URL only (locale is a query param, so it is already part of the
	// cache key) and depends on no request header — no auth, no cookies.
	//
	// Two freshness modes, and the difference is WHO decides when an unpublish
	// becomes visible (ADR-011 §快取失效):
	//
	//   max-age=0 (the default): `public, no-cache` — cacheable but revalidated
	//   on every use, so a retract is visible on the next request. The strong
	//   ETag below makes that revalidation a 304 with no body; with no CDN in
	//   front, the edge answered every request anyway, so exact freshness costs
	//   nothing extra here.
	//
	//   max-age>0: the operator ACCEPTS staleness up to N seconds in caches the
	//   platform cannot reach (browsers, transparent proxies). This is the CDN
	//   posture — pair it with a purge consumer on the content.* webhooks,
	//   because TTL expiry is then the only correction this header offers.
	if h.cacheMaxAge > 0 {
		w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(h.cacheMaxAge.Seconds())))
	} else {
		w.Header().Set("Cache-Control", "public, no-cache")
	}
	w.Header().Set("Content-Type", "application/json")
	// Set before the 304 branch below, not after: a revalidating cache reuses
	// the stored body under the headers the 304 carries, so a nosniff that only
	// rode along with 200s would be absent exactly when the body is replayed.
	w.Header().Set("X-Content-Type-Options", "nosniff")

	if status == http.StatusOK {
		// Strong validator over the exact bytes served. Content-addressed rather
		// than versioned: the edge is stateless and the body is the one truth it
		// holds. Set on the 304 too — RFC 9110 requires the validator to ride
		// along, and some caches drop the stored entry if it does not.
		sum := sha256.Sum256(data)
		etag := `"` + hex.EncodeToString(sum[:]) + `"`
		w.Header().Set("ETag", etag)
		if inmMatches(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.WriteHeader(status)
	// Same reading as writePreview: editor-authored bytes, but declared
	// application/json and pinned there by the nosniff set above.
	_, _ = w.Write(data) //#nosec G705 -- served as application/json with X-Content-Type-Options: nosniff, so it is never interpreted as HTML
}

// inmMatches reports whether an If-None-Match header names this entity. The
// weak-comparison rule applies (a W/ prefix on the candidate still matches):
// for cache revalidation weak equality is the correct one per RFC 9110 §13.1.2,
// and refusing it would only cost 200s where 304s were owed.
func inmMatches(header, etag string) bool {
	if header == "" {
		return false
	}
	if strings.TrimSpace(header) == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		c := strings.TrimSpace(candidate)
		c = strings.TrimPrefix(c, "W/")
		if c == etag {
			return true
		}
	}
	return false
}

// writeUpstreamError maps the Domain API's status onto the public surface
// without leaking its error codes. Anything that is not a clean 404 becomes a
// generic failure: upstream codes describe an internal API the public must not
// learn the shape of.
func (h *Handler) writeUpstreamError(w http.ResponseWriter, err error) {
	var se *upstream.StatusError
	if errors.As(err, &se) {
		switch se.Status {
		case http.StatusNotFound:
			writeError(w, http.StatusNotFound, "NOT_FOUND", "not found")
			return
		case http.StatusTooManyRequests:
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests")
			return
		case http.StatusBadRequest:
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid request")
			return
		}
	}
	// Includes 401/403 from upstream: a delivery credential being refused is an
	// edge misconfiguration, not something the caller can act on — and saying so
	// would advertise that an authenticated API sits behind this.
	writeError(w, http.StatusBadGateway, "UPSTREAM_ERROR", "content is temporarily unavailable")
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	// Errors are never cached: a transient upstream failure must not be pinned
	// into a shared cache for the whole max-age.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": msg},
	})
}

// NewLimiter builds the per-tenant cap.
func NewLimiter(max int, window time.Duration) Limiter {
	return ratelimit.NewIPLimiter(max, window)
}
