// Package upstream is the delivery edge's read-only client for the Domain API's
// content endpoints. It mints a fresh, tenant-scoped delivery credential per
// request (ADR-004): the edge never holds a long-lived token, and never holds
// the main signing key.
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/auth/jwt"
)

// Envelope mirrors the Domain API's response envelope.
type Envelope struct {
	Data  json.RawMessage `json:"data"`
	Error *APIError       `json:"error"`
}

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// StatusError carries the upstream HTTP status so the edge can map it without
// inventing its own semantics.
type StatusError struct {
	Status int
	Err    *APIError
}

func (e *StatusError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return "upstream status " + strconv.Itoa(e.Status)
}

type Client struct {
	baseURL       string
	gatewaySecret string
	signer        *jwt.Signer
	// serviceID identifies this edge in the credential's subject claim. Stable
	// for the process lifetime so upstream logs can attribute delivery traffic.
	serviceID uuid.UUID
	http      *http.Client
}

func NewClient(baseURL, gatewaySecret string, signer *jwt.Signer) *Client {
	return &Client{
		baseURL:       baseURL,
		gatewaySecret: gatewaySecret,
		signer:        signer,
		serviceID:     uuid.New(),
		http:          &http.Client{Timeout: 10 * time.Second},
	}
}

// ListEntriesParams is what a public list may ask for. Every field is the
// caller's to choose; what is NOT here is as deliberate as what is — no status
// (the Domain API forces published-only for a delivery credential, and asking
// would imply the edge is trusted to choose) and no offset (delivery pages by
// cursor; the Domain API refuses one for this audience, and the handler answers
// it before it gets here rather than letting a 403 arrive as an opaque 502).
type ListEntriesParams struct {
	Type   string
	Locale string
	Cursor string
	Limit  int
	// Sort is `key:asc|desc`, forwarded verbatim like the filters and for the
	// same reason: which keys are sortable is a property of the tenant's
	// content type, which the edge does not know and must not guess.
	//
	// It is part of the cursor's contract, not merely alongside it — a token
	// minted under one sort is refused under another (ADR-006 Amendment 5) — so
	// a caller changing `sort` mid-page-through must drop its cursor. The edge
	// forwards both as given and lets upstream say so.
	Sort string
	// Filters are `key:op:value` clauses, one per value, forwarded verbatim.
	// The Domain API owns the grammar and evaluates them against the published
	// snapshot for this credential (ADR-006 Amendment 4); the edge neither
	// parses nor validates them, so a grammar change upstream needs no edge
	// deploy — and a malformed clause comes back as the 400 upstream wrote.
	Filters []string
	// Fields is the projection: which keys of `data` to return. Each value is
	// forwarded as its own `fields` parameter; the Domain API also splits CSV.
	Fields []string
	// Populate names the relation fields to expand into each entry's `related`
	// (ADR-006 Amendment 6). One `populate` parameter per value; the Domain API
	// also splits CSV, exactly as it does for Fields.
	//
	// The edge does not check the keys for the same reason it does not check
	// `sort`: whether a key is a relation, and whether this credential may read
	// the collection at the other end, are answers only the Domain API holds.
	Populate []string
	// Query is the free-text `q` parameter (ADR-021), forwarded verbatim like
	// Filters and Sort. The Domain API reads published_search_text for a
	// delivery credential (the same audience switch that already governs
	// Filters and Sort), so this can no more read a draft than a filter clause
	// can — the edge does not have to police it for that reason, only pass it
	// through and let a caller who sends something malformed see upstream's
	// 422 rather than an edge-invented one.
	Query string
}

// ListEntries returns one tenant's entries of a type. See ListEntriesParams
// for what it will and will not ask upstream for.
func (c *Client) ListEntries(ctx context.Context, tenant string, p ListEntriesParams) (json.RawMessage, error) {
	q := url.Values{}
	q.Set("type", p.Type)
	// Locale IS the caller's to choose (unlike status): every locale of a
	// published entry is equally public.
	if p.Locale != "" {
		q.Set("locale", p.Locale)
	}
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	// Keyset paging. Forwarded verbatim: the token is the Domain API's to mint
	// and to validate, and a cursor the edge "helpfully" normalised would no
	// longer be the one that was issued.
	if p.Cursor != "" {
		q.Set("cursor", p.Cursor)
	}
	if p.Sort != "" {
		q.Set("sort", p.Sort)
	}
	for _, f := range p.Filters {
		if f != "" {
			q.Add("filter", f)
		}
	}
	for _, f := range p.Fields {
		if f != "" {
			q.Add("fields", f)
		}
	}
	for _, f := range p.Populate {
		if f != "" {
			q.Add("populate", f)
		}
	}
	if p.Query != "" {
		q.Set("q", p.Query)
	}
	return c.get(ctx, tenant, "/api/v1/content/entries?"+q.Encode())
}

// GetEntry returns one entry by id. Unpublished entries come back as 404 from
// the Domain API, so the edge does not have to special-case them.
func (c *Client) GetEntry(ctx context.Context, tenant, typeName, id string, populate []string) (json.RawMessage, error) {
	q := url.Values{}
	q.Set("type", typeName)
	for _, f := range populate {
		if f != "" {
			q.Add("populate", f)
		}
	}
	return c.get(ctx, tenant, "/api/v1/content/entries/"+url.PathEscape(id)+"?"+q.Encode())
}

// GetEntryPreview forwards a CALLER-SUPPLIED delivery credential instead of
// minting the edge's own, so the Domain API sees the narrowed subject and
// answers with the working copy of the one entry that credential names.
//
// The token is forwarded verbatim and never decoded here, for the same reason
// the cursor is: its claims are the Domain API's to define and to validate, and
// an edge that read them would have to be kept in step with a format it does not
// own — and would be a second place that could get "is this a preview" wrong.
// The edge therefore cannot tell a valid preview token from a forged one, and
// does not need to: the only key that can produce one lives upstream.
//
// It takes no tenant, and the omission is the point: for every other read the
// edge mints a credential FOR a tenant, so the tenant is an input. Here the
// token already carries its own tenant claim and that claim is what scopes the
// read, so accepting a tenant argument would only invite a caller to believe the
// two are cross-checked. They are not — which is a rate-limit attribution
// problem, not a leak (the bearer holds that credential either way), and it is
// handled at the handler by keying preview traffic on the token.
func (c *Client) GetEntryPreview(ctx context.Context, typeName, id, previewToken string) (json.RawMessage, error) {
	q := url.Values{}
	q.Set("type", typeName)
	return c.getAs(ctx, "/api/v1/content/entries/"+url.PathEscape(id)+"?"+q.Encode(), previewToken)
}

// ResolveMedia asks the Domain API for a short-lived signed URL. The API is the
// one that decides whether the asset is publicly readable (it must be referenced
// by a published entry) — the edge only relays.
//
// preset is forwarded verbatim when non-empty (ADR-019): the API owns the
// preset table and answers 400 for a name it does not know, and the edge
// must not grow a copy of that table to pre-check it.
func (c *Client) ResolveMedia(ctx context.Context, tenant, id, preset string) (string, error) {
	path := "/api/v1/content/media/" + url.PathEscape(id) + "/url"
	if preset != "" {
		path += "?preset=" + url.QueryEscape(preset)
	}
	raw, err := c.get(ctx, tenant, path)
	if err != nil {
		return "", err
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", fmt.Errorf("decode media url: %w", err)
	}
	if body.URL == "" {
		return "", fmt.Errorf("upstream returned an empty media url")
	}
	return body.URL, nil
}

func (c *Client) get(ctx context.Context, tenant, path string) (json.RawMessage, error) {
	// Minted per request and scoped to this tenant only — a leaked token is
	// useless for any other tenant and expires in minutes.
	token, _, err := c.signer.IssueDeliveryToken(c.serviceID, tenant)
	if err != nil {
		return nil, fmt.Errorf("mint delivery credential: %w", err)
	}
	return c.getAs(ctx, path, token)
}

// getAs is get with the credential decided by the caller. Splitting it out —
// rather than giving get an "if token == "" then mint" branch — keeps the
// minting path unable to be skipped by accident: a future read path that forgets
// to pass a token gets a compile error from getAs, not an anonymous request.
func (c *Client) getAs(ctx context.Context, path, token string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if c.gatewaySecret != "" {
		req.Header.Set("X-Gateway-Secret", c.gatewaySecret)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var env Envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil && resp.StatusCode == http.StatusOK {
		return nil, fmt.Errorf("decode upstream response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{Status: resp.StatusCode, Err: env.Error}
	}
	return env.Data, nil
}
