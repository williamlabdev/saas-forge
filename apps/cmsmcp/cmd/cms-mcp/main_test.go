package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/williamlabdev/saas-forge/apps/cmsmcp/internal/config"
	"github.com/williamlabdev/saas-forge/apps/cmsmcp/internal/upstream"
)

// The credential a tool acts as must be the one on THAT request, not the one
// that opened the session (ADR-013 未解項:MCP server 的租戶隔離).
//
// This is asserted through the upstream call rather than through anything the
// MCP layer reports, because the question is which token leaves this process.
// A test that only read the MCP response would pass while the wrong bearer went
// out the other side.
func TestEachRequestActsAsItsOwnBearer(t *testing.T) {
	var seen []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		// Shape does not matter here; only that the call was made and with what.
		_, _ = w.Write([]byte(`{"data":{"name":"post","fields":[]}}`))
	}))
	defer api.Close()

	cfg := config.Config{
		DomainAPIURL:   api.URL,
		RequestTimeout: 5 * time.Second,
		DefaultLimit:   10,
		MaxLimit:       50,
	}
	h := httpHandler(cfg, upstream.NewClient(cfg.DomainAPIURL, "", cfg.RequestTimeout))

	post := func(t *testing.T, bearer, sessionID, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+bearer)
		if sessionID != "" {
			req.Header.Set("Mcp-Session-Id", sessionID)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// THE HANDSHAKE IS NOT SCENERY. A stateful handler refuses a bare tools/call
	// before it reaches any tool, so a test that skipped initialize would go red
	// under the stateless option being removed — but red for "no handshake"
	// rather than for the credential, and it would be pinning the wrong rule.
	// Measured: without these two calls the mutation fails at "upstream saw 0
	// calls", which says nothing about whose token went out.
	const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` +
		`"protocolVersion":"2025-06-18","capabilities":{},` +
		`"clientInfo":{"name":"test","version":"0"}}}`
	initRec := post(t, "token-a", "", initBody)
	if initRec.Code != http.StatusOK {
		t.Fatalf("initialize: code=%d body=%s", initRec.Code, initRec.Body)
	}
	// A stateful handler answers with the session id it just minted. A stateless
	// one has none to give, and that absence IS the property: with no session,
	// there is nothing a credential can be pinned to.
	sessionID := initRec.Header().Get("Mcp-Session-Id")
	if sessionID != "" {
		t.Errorf("a session was opened (%s) — a credential is being pinned to it", sessionID)
	}
	post(t, "token-a", sessionID, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	const callBody = `{"jsonrpc":"2.0","id":2,"method":"tools/call",` +
		`"params":{"name":"cms_describe","arguments":{"type":"post"}}}`

	if rec := post(t, "token-a", sessionID, callBody); rec.Code != http.StatusOK {
		t.Fatalf("first tool call: code=%d body=%s", rec.Code, rec.Body)
	}
	// The second caller presents its OWN token on the same session. This is the
	// request that acts as token-a when the session holds the credential.
	if rec := post(t, "token-b", sessionID, callBody); rec.Code != http.StatusOK {
		t.Fatalf("second tool call: code=%d body=%s", rec.Code, rec.Body)
	}

	if len(seen) != 2 {
		t.Fatalf("upstream saw %d calls, want 2: %v", len(seen), seen)
	}
	if seen[0] != "Bearer token-a" {
		t.Errorf("first upstream call carried %q, want token-a", seen[0])
	}
	if seen[1] != "Bearer token-b" {
		t.Errorf("second upstream call carried %q — it acted as the credential that opened the session", seen[1])
	}
}

// requireBearer is the door, and it stays shut before any of the above matters:
// without it an unauthenticated caller would get a tool list and only meet a
// 401 forwarded from the CMS on the first call.
func TestUnauthenticatedRequestNeverReachesTheServer(t *testing.T) {
	var reached bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer api.Close()

	cfg := config.Config{DomainAPIURL: api.URL, RequestTimeout: 5 * time.Second, DefaultLimit: 10, MaxLimit: 50}
	h := httpHandler(cfg, upstream.NewClient(cfg.DomainAPIURL, "", cfg.RequestTimeout))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d, want 401: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate=%q — the refusal must say how to authenticate", got)
	}
	if reached {
		t.Error("an unauthenticated request reached the upstream API")
	}
}
