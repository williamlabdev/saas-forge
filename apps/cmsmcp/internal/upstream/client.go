// Package upstream is the CMS MCP server's client for the Domain API's content
// endpoints.
//
// It reads and, since ADR-013 step 7, writes. The step-6 version of this comment
// said the read-only surface was "NOT what stops an agent from writing — that is
// the Domain API's authorize() and the agent scope refusals, both of which apply
// identically to a caller that ignores this server and speaks HTTP directly",
// and predicted that a write method appearing here would change nothing about
// what is refused. That is exactly what happened: the three methods below reach
// endpoints an agent credential was ALREADY allowed at (content:create and
// content:update are enumerated in agent_gate.go), and publish stays refused
// there rather than by this file declining to call it.
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
)

// Envelope mirrors the Domain API's response envelope (internal/pkg/response).
type Envelope struct {
	Data  json.RawMessage `json:"data"`
	Error *APIError       `json:"error"`
}

// APIError is the Domain API's error body, carried WHOLE.
//
// Details is the reason this type exists rather than a string (ADR-013 §8):
// errFieldEnumInvalid returns the allowed values, errFieldPropImmutable returns
// the "add a field, move the values, drop the old one" hint, and a bad filter
// returns the operators the field does accept. An agent handed those can fix
// its own call; an agent handed err.Error() can only guess again. Flattening
// happens by accident — it is what every ordinary Go error path does — so the
// map is kept on the wire type and asserted in the tests.
type APIError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// StatusError pairs the upstream status with its body. The status travels
// because "403 because this credential's whitelist does not name that type" and
// "404 because no such type exists" are different repairs for the agent, and
// the code alone does not always separate them.
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

// Body returns the structured error to hand back to the agent, synthesising one
// when the upstream reply was not a well-formed envelope (a gateway rejection,
// a proxy error page). A caller must always get the same SHAPE back, or the
// tool's error contract holds only when the CMS itself answered.
func (e *StatusError) Body() *APIError {
	if e.Err != nil {
		return e.Err
	}
	return &APIError{
		Code:    "UPSTREAM_" + strconv.Itoa(e.Status),
		Message: "the content API returned status " + strconv.Itoa(e.Status) + " with no error envelope",
	}
}

type Client struct {
	baseURL       string
	gatewaySecret string
	http          *http.Client
}

func NewClient(baseURL, gatewaySecret string, timeout time.Duration) *Client {
	return &Client{
		baseURL:       baseURL,
		gatewaySecret: gatewaySecret,
		http:          &http.Client{Timeout: timeout},
	}
}

// get performs one authenticated read. The bearer is a parameter rather than a
// field because the HTTP transport serves many callers from one process and
// each brings its own credential; a stored token would make whose-credential a
// property of the process instead of the request.
func (c *Client) get(ctx context.Context, bearer, path string, q url.Values) (json.RawMessage, error) {
	return c.do(ctx, bearer, http.MethodGet, path, q, nil, nil)
}

// do performs one authenticated call. get and every write method share it so
// that the envelope handling below — which decides what an agent is told when
// the reply is not a well-formed envelope — cannot come to differ between reads
// and writes. A write failing in a shape a read handles is the harder case to
// notice, because it is the one nobody exercises twice.
func (c *Client) do(ctx context.Context, bearer, method, path string, q url.Values, body []byte, headers map[string]string) (json.RawMessage, error) {
	u := c.baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if c.gatewaySecret != "" {
		req.Header.Set(authn.GatewayHeader, c.gatewaySecret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var env Envelope
	// A body that will not parse is reported by status, not swallowed: the
	// alternative is returning nil data with a nil error, which reads to every
	// caller as "the type has no fields" or "there are no entries".
	if err := json.Unmarshal(raw, &env); err != nil {
		if resp.StatusCode >= 400 {
			return nil, &StatusError{Status: resp.StatusCode}
		}
		return nil, fmt.Errorf("upstream returned a %d that is not a JSON envelope", resp.StatusCode)
	}
	if resp.StatusCode >= 400 || env.Error != nil {
		return nil, &StatusError{Status: resp.StatusCode, Err: env.Error}
	}
	return env.Data, nil
}

// GetType fetches one content type's declaration — the shape cms_describe
// publishes, including each field's `supported` filter operators (ADR-013 §6).
func (c *Client) GetType(ctx context.Context, bearer, name string) (json.RawMessage, error) {
	return c.get(ctx, bearer, "/api/v1/content/types/"+url.PathEscape(name), nil)
}

// ListEntriesParams mirrors the query the entries endpoint accepts. Every
// field is optional except Type, which every entry path requires (?type=).
type ListEntriesParams struct {
	Type    string
	Filters []string
	Sort    string
	Fields  []string
	Status  string
	Locale  string
	Limit   int
	Offset  int
}

func (c *Client) ListEntries(ctx context.Context, bearer string, p ListEntriesParams) (json.RawMessage, error) {
	q := url.Values{}
	q.Set("type", p.Type)
	for _, f := range p.Filters {
		// Repeated ?filter=, which is the spelling the handler reads.
		q.Add("filter", f)
	}
	for _, f := range p.Fields {
		q.Add("fields", f)
	}
	if p.Sort != "" {
		q.Set("sort", p.Sort)
	}
	if p.Status != "" {
		q.Set("status", p.Status)
	}
	if p.Locale != "" {
		q.Set("locale", p.Locale)
	}
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	if p.Offset > 0 {
		q.Set("offset", strconv.Itoa(p.Offset))
	}
	return c.get(ctx, bearer, "/api/v1/content/entries", q)
}

func (c *Client) GetEntry(ctx context.Context, bearer, typeName, id string) (json.RawMessage, error) {
	q := url.Values{}
	q.Set("type", typeName)
	return c.get(ctx, bearer, "/api/v1/content/entries/"+url.PathEscape(id), q)
}

func (c *Client) ListTranslations(ctx context.Context, bearer, typeName, id string) (json.RawMessage, error) {
	q := url.Values{}
	q.Set("type", typeName)
	return c.get(ctx, bearer, "/api/v1/content/entries/"+url.PathEscape(id)+"/translations", q)
}

// CreateEntryParams is one POST /entries. IdempotencyKey travels as a header,
// which is where the CMS reads it (ADR-013 §9).
type CreateEntryParams struct {
	Type           string
	Payload        json.RawMessage
	Locale         string
	TranslationOf  string
	IdempotencyKey string
}

func (c *Client) CreateEntry(ctx context.Context, bearer string, p CreateEntryParams) (json.RawMessage, error) {
	q := url.Values{}
	q.Set("type", p.Type)
	if p.Locale != "" {
		q.Set("locale", p.Locale)
	}
	if p.TranslationOf != "" {
		q.Set("translation_of", p.TranslationOf)
	}
	var headers map[string]string
	if p.IdempotencyKey != "" {
		headers = map[string]string{"Idempotency-Key": p.IdempotencyKey}
	}
	return c.do(ctx, bearer, http.MethodPost, "/api/v1/content/entries", q, p.Payload, headers)
}

// UpdateEntryParams is one PATCH /entries/{id}. Version is REQUIRED — see the
// tool description for why it is not defaulted.
type UpdateEntryParams struct {
	Type    string
	ID      string
	Payload json.RawMessage
	Version int
}

// UpdateEntry sends the expected version as `If-Match`, which is where the CMS
// reads it (handler.go's parseIfMatchVersion).
//
// THIS WAS ?version= FIRST, AND EVERY UNIT TEST PASSED. The CMS ignores an
// unknown query parameter, so expectedVersion arrived as 0, which means "no
// check" — the optimistic lock was not weakened, it was absent, and every agent
// update was a last-writer-wins overwrite of whoever wrote in between. Nothing
// short of the e2e could see it: a recorder asserts what went out, and what went
// out was exactly what the code intended to send.
func (c *Client) UpdateEntry(ctx context.Context, bearer string, p UpdateEntryParams) (json.RawMessage, error) {
	q := url.Values{}
	q.Set("type", p.Type)
	headers := map[string]string{"If-Match": strconv.Itoa(p.Version)}
	return c.do(ctx, bearer, http.MethodPatch, "/api/v1/content/entries/"+url.PathEscape(p.ID), q, p.Payload, headers)
}

// Unpublish takes an entry off the public edge. There is deliberately no
// Publish beside it: ADR-014 §1 makes a person the release gate, and
// agent_gate.go refuses content:publish to every agent credential — so a method
// here would produce a 403 with the tool's name on it instead of the gate's.
//
// The asymmetry is the design, not an omission. Retract is allowed precisely
// because it is how an agent STOPS bad content that is already out; refusing it
// too would mean the harm continues until a person appears.
func (c *Client) Unpublish(ctx context.Context, bearer, typeName, id string) (json.RawMessage, error) {
	q := url.Values{}
	q.Set("type", typeName)
	return c.do(ctx, bearer, http.MethodPost, "/api/v1/content/entries/"+url.PathEscape(id)+"/unpublish", q, nil, nil)
}
