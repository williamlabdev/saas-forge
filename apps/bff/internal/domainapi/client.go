package domainapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Envelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type Client struct {
	base string
	// gatewaySecret, when set, is sent as X-Gateway-Secret on every call so the
	// domain API's gateway guard (TKT-R7) accepts requests from this BFF — the
	// BFF is the trusted gateway in front of :8080.
	gatewaySecret string
	client        *http.Client
}

func NewClient(baseURL string) *Client {
	return NewClientWithGateway(baseURL, "")
}

func NewClientWithGateway(baseURL, gatewaySecret string) *Client {
	return &Client{
		base:          strings.TrimRight(baseURL, "/"),
		gatewaySecret: strings.TrimSpace(gatewaySecret),
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) Register(ctx context.Context, body any) (map[string]any, error) {
	return c.do(ctx, http.MethodPost, "/api/v1/users", "", body, http.StatusCreated, http.StatusOK)
}

func (c *Client) Login(ctx context.Context, body any) (map[string]any, error) {
	return c.do(ctx, http.MethodPost, "/api/v1/auth/login", "", body, http.StatusOK)
}

func (c *Client) GetUser(ctx context.Context, id, bearer string) (map[string]any, error) {
	return c.do(ctx, http.MethodGet, "/api/v1/users/"+id, bearer, nil, http.StatusOK)
}

func (c *Client) GetMe(ctx context.Context, bearer string) (map[string]any, error) {
	return c.do(ctx, http.MethodGet, "/api/v1/users/me", bearer, nil, http.StatusOK)
}

func (c *Client) ListNotifications(ctx context.Context, bearer string, limit int) ([]map[string]any, error) {
	path := fmt.Sprintf("/api/v1/notifications?limit=%d", limit)
	raw, err := c.do(ctx, http.MethodGet, path, bearer, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	items, ok := raw["items"].([]any)
	if !ok {
		return nil, fmt.Errorf("domainapi: unexpected notifications shape")
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("domainapi: invalid notification item")
		}
		out = append(out, m)
	}
	return out, nil
}

func (c *Client) CreateNotification(ctx context.Context, bearer string, body any) (map[string]any, error) {
	return c.do(ctx, http.MethodPost, "/api/v1/notifications", bearer, body, http.StatusCreated)
}

func (c *Client) ListPlatformApps(ctx context.Context, bearer, q, status string, limit, offset int) (map[string]any, error) {
	params := url.Values{}
	params.Set("limit", fmt.Sprintf("%d", limit))
	params.Set("offset", fmt.Sprintf("%d", offset))
	if q != "" {
		params.Set("q", q)
	}
	if status != "" {
		params.Set("status", status)
	}
	path := "/api/v1/platform/apps?" + params.Encode()
	return c.do(ctx, http.MethodGet, path, bearer, nil, http.StatusOK)
}

func (c *Client) CreatePlatformApp(ctx context.Context, bearer string, body any) (map[string]any, error) {
	return c.do(ctx, http.MethodPost, "/api/v1/platform/apps", bearer, body, http.StatusCreated)
}

func (c *Client) UpdatePlatformAppStatus(ctx context.Context, bearer, id, status string) (map[string]any, error) {
	path := fmt.Sprintf("/api/v1/platform/apps/%s/status", id)
	return c.do(ctx, http.MethodPatch, path, bearer, map[string]any{"status": status}, http.StatusOK)
}

func (c *Client) GetPlatformBillingSummary(ctx context.Context, bearer string) (map[string]any, error) {
	return c.do(ctx, http.MethodGet, "/api/v1/platform/billing/summary", bearer, nil, http.StatusOK)
}

func (c *Client) ListPlatformInvoices(ctx context.Context, bearer string, limit int) (map[string]any, error) {
	path := fmt.Sprintf("/api/v1/platform/billing/invoices?limit=%d", limit)
	return c.do(ctx, http.MethodGet, path, bearer, nil, http.StatusOK)
}

func (c *Client) ListPlatformStaff(ctx context.Context, bearer string) (map[string]any, error) {
	return c.do(ctx, http.MethodGet, "/api/v1/platform/staff", bearer, nil, http.StatusOK)
}

func (c *Client) ListPlatformAlerts(ctx context.Context, bearer string, limit int) (map[string]any, error) {
	path := fmt.Sprintf("/api/v1/platform/alerts?limit=%d", limit)
	return c.do(ctx, http.MethodGet, path, bearer, nil, http.StatusOK)
}

func (c *Client) GetPlatformReportsSummary(ctx context.Context, bearer string) (map[string]any, error) {
	return c.do(ctx, http.MethodGet, "/api/v1/platform/reports/summary", bearer, nil, http.StatusOK)
}

// ListAgentCredentials returns the calling tenant's registry (ADR-013 補裁 O).
//
// The domain answers a BARE ARRAY, not a {items:…} object, so this cannot go
// through do() — see doList for why that distinction is worth a second method
// rather than a flag.
func (c *Client) ListAgentCredentials(ctx context.Context, bearer string) ([]map[string]any, error) {
	return c.doList(ctx, http.MethodGet, "/api/v1/auth/agent-tokens", bearer, nil, http.StatusOK)
}

// IssueAgentCredential mints by downgrading the caller's own token.
//
// The bearer is not merely how this call authenticates — it is the INPUT: the
// domain parses it and derives the agent's claims from it (補裁 O-3). A caller
// on dev headers has nothing to downgrade and gets a 400 that says so, which
// is why the BFF forwards the token rather than deciding here.
func (c *Client) IssueAgentCredential(ctx context.Context, bearer string, body any) (map[string]any, error) {
	return c.do(ctx, http.MethodPost, "/api/v1/auth/agent-tokens", bearer, body, http.StatusCreated)
}

// RevokeAgentCredential deletes a credential. The domain answers 204.
func (c *Client) RevokeAgentCredential(ctx context.Context, bearer, id string) error {
	return c.doNoContent(ctx, http.MethodDelete, "/api/v1/auth/agent-tokens/"+url.PathEscape(id), bearer, http.StatusNoContent)
}

// doEnvelope is the half of a call that every response shape agrees on: send
// it, decode the {data,error} envelope, and turn a domain error into an
// APIError. What the callers disagree about is only the shape of Data — which
// is why there are three thin methods below rather than one with a flag.
func (c *Client) doEnvelope(ctx context.Context, method, path, bearer string, body any, okStatuses ...int) (json.RawMessage, error) {
	if stats := fanoutFromContext(ctx); stats != nil {
		start := time.Now()
		defer func() { stats.record(time.Since(start)) }()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if c.gatewaySecret != "" {
		req.Header.Set("X-Gateway-Secret", c.gatewaySecret)
	}
	res, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	payload, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	var env Envelope
	// 204 carries no body by definition, so the envelope contract does not
	// apply to it. Every other status must produce one: an empty body there is
	// a broken response, not a quiet success, and decoding is what says so.
	//
	// The decode happens BEFORE the status check on purpose — an error envelope
	// arriving with a 4xx must surface as its own code and message, not as
	// "unexpected status".
	if res.StatusCode != http.StatusNoContent {
		if err := json.Unmarshal(payload, &env); err != nil {
			return nil, fmt.Errorf("domainapi: decode envelope: %w", err)
		}
		if env.Error != nil {
			return nil, &APIError{Code: env.Error.Code, Message: env.Error.Message, HTTPStatus: res.StatusCode}
		}
	}
	ok := false
	for _, s := range okStatuses {
		if res.StatusCode == s {
			ok = true
			break
		}
	}
	if !ok {
		return nil, fmt.Errorf("domainapi: unexpected status %d", res.StatusCode)
	}
	return env.Data, nil
}

func (c *Client) do(ctx context.Context, method, path, bearer string, body any, okStatuses ...int) (map[string]any, error) {
	raw, err := c.doEnvelope(ctx, method, path, bearer, body, okStatuses...)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}, nil
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	return data, nil
}

// doList decodes a BARE ARRAY payload.
//
// It is separate from do() rather than a shape guess inside it because the two
// answers to "what is `data`" are not interchangeable: a handler that starts
// returning {items:…} where it used to return [...] has changed its contract,
// and a client that silently accepts both is the reason nobody notices.
func (c *Client) doList(ctx context.Context, method, path, bearer string, body any, okStatuses ...int) ([]map[string]any, error) {
	raw, err := c.doEnvelope(ctx, method, path, bearer, body, okStatuses...)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return []map[string]any{}, nil
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// doNoContent is for endpoints whose success carries nothing back. It still
// runs the whole envelope path, because a FAILED delete does answer with one.
func (c *Client) doNoContent(ctx context.Context, method, path, bearer string, okStatuses ...int) error {
	_, err := c.doEnvelope(ctx, method, path, bearer, nil, okStatuses...)
	return err
}

type APIError struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *APIError) Error() string {
	return e.Code + ": " + e.Message
}
