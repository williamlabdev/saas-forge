// Package config loads the public delivery edge's settings from the
// environment. The edge is the only internet-facing piece of ADR-004 option A,
// so its configuration is deliberately narrow: it can reach the Domain API and
// mint delivery credentials, and nothing else.
package config

import (
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr     string
	DomainAPIURL string
	// GatewaySecret is forwarded as X-Gateway-Secret so the Domain API's
	// gateway guard (TKT-R7) accepts this edge.
	GatewaySecret string
	// DeliveryJWTSecret signs the per-request delivery credential. It is NOT the
	// main signing key — see ADR-004: whatever key this process holds is exposed
	// to the public side, so it must only be able to express delivery claims.
	DeliveryJWTSecret []byte
	// TokenTTL bounds how long a minted credential is useful if it leaks from a
	// log or a proxy. Tokens are minted per request, so this can be short.
	TokenTTL time.Duration
	// CacheMaxAge is the public Cache-Control lifetime for successful reads.
	//
	// ZERO IS THE DEFAULT AND MEANS `public, no-cache` — cacheable, but
	// revalidated on every use against the edge's strong ETag, so an unpublish
	// is visible on the next request. A positive value is the operator
	// accepting up to that much staleness in caches the platform cannot reach
	// (browsers, transparent proxies); pair it with a purge consumer on the
	// content.* webhooks (ADR-011). Before this default flipped, a retracted
	// entry kept being served for the full max-age with no recourse.
	CacheMaxAge time.Duration
	// RateLimit / RateWindow cap requests per tenant. A public endpoint keyed by
	// tenant is an obvious way to burn someone else's quota, so this is not
	// optional in production (see Validate).
	RateLimit  int
	RateWindow time.Duration
}

func Load() Config {
	cfg := Config{
		HTTPAddr:      getenv("DELIVERY_HTTP_ADDR", ":4100"),
		DomainAPIURL:  strings.TrimRight(getenv("DOMAIN_API_URL", "http://localhost:8080"), "/"),
		GatewaySecret: strings.TrimSpace(os.Getenv("GATEWAY_SECRET")),
		TokenTTL:      2 * time.Minute,
		CacheMaxAge:   0,
		RateLimit:     600,
		RateWindow:    time.Minute,
	}
	if h := strings.TrimSpace(os.Getenv("DELIVERY_JWT_SECRET_HEX")); h != "" {
		if b, err := hex.DecodeString(h); err == nil {
			cfg.DeliveryJWTSecret = b
		}
	}
	if n, err := strconv.Atoi(getenv("DELIVERY_TOKEN_TTL_SECONDS", "120")); err == nil && n > 0 {
		cfg.TokenTTL = time.Duration(n) * time.Second
	}
	if n, err := strconv.Atoi(getenv("DELIVERY_CACHE_MAX_AGE_SECONDS", "0")); err == nil && n >= 0 {
		cfg.CacheMaxAge = time.Duration(n) * time.Second
	}
	if n, err := strconv.Atoi(getenv("DELIVERY_RATE_LIMIT", "600")); err == nil {
		cfg.RateLimit = n
	}
	if n, err := strconv.Atoi(getenv("DELIVERY_RATE_WINDOW_SECONDS", "60")); err == nil && n > 0 {
		cfg.RateWindow = time.Duration(n) * time.Second
	}
	return cfg
}

// Validate refuses to start on a configuration that would be unsafe in public.
func Validate(cfg Config) error {
	if len(cfg.DeliveryJWTSecret) < 32 {
		return errors.New("config: DELIVERY_JWT_SECRET_HEX must be at least 64 hex chars (32 bytes) — the edge cannot mint credentials without it; generate: openssl rand -hex 32")
	}
	if cfg.DomainAPIURL == "" {
		return errors.New("config: DOMAIN_API_URL is required")
	}
	// A public endpoint keyed by a caller-supplied tenant with no cap is a
	// ready-made way to burn another tenant's quota (ADR-004 amendment).
	if cfg.RateLimit <= 0 {
		return errors.New("config: DELIVERY_RATE_LIMIT must be > 0 — an uncapped public endpoint lets anyone drive load against any tenant")
	}
	return nil
}

func getenv(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
