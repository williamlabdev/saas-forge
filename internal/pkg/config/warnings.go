package config

import (
	"log"
	"strings"
)

// LogProductionWarnings emits startup warnings for unsafe dev defaults.
func LogProductionWarnings(rt Runtime) {
	mode := strings.ToLower(strings.TrimSpace(rt.AuthzMode))
	if mode == "allow" || mode == "" {
		log.Printf("WARN: AUTHZ_MODE=%q — all authorization checks pass; use rbac or opa in production", rt.AuthzMode)
	}
	if rt.AuthDevHeaders {
		log.Printf("WARN: AUTH_DEV_HEADERS=true — X-User-* headers are trusted without JWT; set false in production")
	}
	if rt.TrustProxyHeaders {
		log.Printf("WARN: TRUST_PROXY_HEADERS=true — X-Forwarded-For is trusted for rate limiting/audit; ensure a proxy strips client-supplied values")
	}
	if strings.TrimSpace(rt.MCPBaseURL) == "" {
		log.Printf("WARN: MCP_BASE_URL is empty — outbox deliveries use NoopClient (no external MCP sync)")
	}
	if rt.AgentRateLimit <= 0 {
		log.Printf("WARN: AGENT_RATE_LIMIT is unset — one agent credential may call the domain API as fast as it likes; set a per-window cap suited to your replica count (the limiter is in-process, so the effective ceiling is replicas × limit)")
	}
	if strings.TrimSpace(rt.GatewaySecret) == "" {
		log.Printf("WARN: GATEWAY_SECRET is empty — the domain API accepts direct requests on :8080; put it behind a gateway that injects X-Gateway-Secret, or rely on a network boundary/mTLS")
	}
}
