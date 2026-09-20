// Package config loads the CMS MCP server's settings from the environment.
//
// The process is deliberately thin (ADR-013, dominating rule): it can reach the
// Domain API over HTTP and nothing else. No database handle, no signing key,
// no ability to mint a credential. That is what makes "the tool list is UX, not
// an authorization boundary" true by construction rather than by discipline —
// a tool this process forgets to expose is a button an agent does not see, not
// a permission the agent loses, and there is no path here that could bypass a
// check even if someone wanted one.
//
// The package lives under apps/cmsmcp rather than internal/pkg/mcp because that
// path is taken by the userstate mock, which collides with Model Context
// Protocol in name only (ADR-013 §範圍外). The compose service name `mcp` is
// taken by the same mock, hence `cms-mcp` throughout.
package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// HTTPAddr selects the transport. Empty (the default) runs the stdio
	// transport, which is how an MCP client launches a local server. Non-empty
	// serves the streamable HTTP transport on that address.
	HTTPAddr string
	// DomainAPIURL is the CMS this server is a transport adapter for.
	DomainAPIURL string
	// GatewaySecret is forwarded as X-Gateway-Secret. The Domain API's gateway
	// guard (TKT-R7) is global middleware, not a per-route concern, so a server
	// that omits this header is refused before authentication is even looked
	// at — an easy way to spend an afternoon debugging a 403 that has nothing
	// to do with the agent credential.
	GatewaySecret string
	// AgentToken is the credential every request carries in stdio mode.
	//
	// It must be an AGENT credential (ADR-013 §1). Handing this process an
	// ordinary human access token is the one way to make this whole ADR
	// inert: sub.IsAgent() would be false, so the scope refusals, the
	// AllowedTypes whitelist and the agent provenance columns all stop
	// applying, and the server would look like it works. Nothing here can
	// detect that — the Domain API would happily serve it — so it is called
	// out in the operator docs instead.
	//
	// Mint one at POST /api/v1/auth/agent-tokens, as an owner or admin of the
	// tenant (ADR-013, ruled 2026-08-06). The credential is revocable — DELETE
	// on /api/v1/auth/agent-tokens/{id} stops it on its next request — which is
	// what makes its long TTL issuable at all. A revoked token fails here as a
	// plain 401 from the Domain API, indistinguishable from an expired one.
	AgentToken     string
	RequestTimeout time.Duration
	// DefaultLimit / MaxLimit implement ADR-013 §7's token budget. They are NOT
	// a security control — the Domain API applies its own cap independently,
	// and an agent that goes around this server to plain HTTP gets that cap,
	// not this one. They exist because the model's context is the scarce
	// resource, so the default page here is far smaller than a human UI's.
	DefaultLimit int
	MaxLimit     int
}

func Load() Config {
	cfg := Config{
		HTTPAddr:       strings.TrimSpace(os.Getenv("CMS_MCP_HTTP_ADDR")),
		DomainAPIURL:   strings.TrimRight(getenv("DOMAIN_API_URL", "http://localhost:8080"), "/"),
		GatewaySecret:  strings.TrimSpace(os.Getenv("GATEWAY_SECRET")),
		AgentToken:     strings.TrimSpace(os.Getenv("CMS_AGENT_TOKEN")),
		RequestTimeout: 10 * time.Second,
		DefaultLimit:   10,
		MaxLimit:       50,
	}
	if n, err := strconv.Atoi(getenv("CMS_MCP_TIMEOUT_SECONDS", "10")); err == nil && n > 0 {
		cfg.RequestTimeout = time.Duration(n) * time.Second
	}
	if n, err := strconv.Atoi(getenv("CMS_MCP_DEFAULT_LIMIT", "10")); err == nil && n > 0 {
		cfg.DefaultLimit = n
	}
	if n, err := strconv.Atoi(getenv("CMS_MCP_MAX_LIMIT", "50")); err == nil && n > 0 {
		cfg.MaxLimit = n
	}
	return cfg
}

// Validate refuses configurations whose failure mode is silent.
func Validate(cfg Config) error {
	if cfg.DomainAPIURL == "" {
		return errors.New("config: DOMAIN_API_URL is required")
	}
	if cfg.HTTPAddr == "" && cfg.AgentToken == "" {
		return errors.New("config: CMS_AGENT_TOKEN is required in stdio mode — this server holds no key and cannot mint a credential of its own")
	}
	// The HTTP transport takes each caller's own bearer, so a process-wide
	// token there is not a fallback — it is a second, invisible answer to
	// "whose credential is this?". A caller that forgot its Authorization
	// header would silently act as whoever CMS_AGENT_TOKEN belongs to, and
	// every write would be attributed to that principal. Refuse the
	// combination rather than pick a winner.
	if cfg.HTTPAddr != "" && cfg.AgentToken != "" {
		return errors.New("config: CMS_AGENT_TOKEN must not be set when CMS_MCP_HTTP_ADDR is — in HTTP mode each request carries its own credential, and a process-wide token would silently answer for callers that sent none")
	}
	if cfg.MaxLimit < cfg.DefaultLimit {
		return errors.New("config: CMS_MCP_MAX_LIMIT must not be below CMS_MCP_DEFAULT_LIMIT")
	}
	return nil
}

func getenv(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
