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

// ListEntries returns one tenant's entries of a type. It never asks for a
// status: the Domain API forces published-only for delivery credentials, and
// asking would imply the edge is trusted to choose (it is not).
func (c *Client) ListEntries(ctx context.Context, tenant, typeName, locale, cursor string, limit int) (json.RawMessage, error) {
	q := url.Values{}
	q.Set("type", typeName)
	// Locale IS the caller's to choose (unlike status): every locale of a
	// published entry is equally public.
	if locale != "" {
		q.Set("locale", locale)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	// Keyset paging. Forwarded verbatim: the token is the Domain API's to mint
	// and to validate, and a cursor the edge "helpfully" normalised would no
	// longer be the one that was issued.
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	return c.get(ctx, tenant, "/api/v1/content/entries?"+q.Encode())
}

// GetEntry returns one entry by id. Unpublished entries come back as 404 from
// the Domain API, so the edge does not have to special-case them.
func (c *Client) GetEntry(ctx context.Context, tenant, typeName, id string) (json.RawMessage, error) {
	q := url.Values{}
	q.Set("type", typeName)
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
func (c *Client) ResolveMedia(ctx context.Context, tenant, id string) (string, error) {
	raw, err := c.get(ctx, tenant, "/api/v1/content/media/"+url.PathEscape(id)+"/url")
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
