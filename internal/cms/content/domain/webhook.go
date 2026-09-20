package domain

import (
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Webhook is one tenant-registered receiver of content events (ADR-011).
type Webhook struct {
	ID       uuid.UUID
	TenantID string
	URL      string
	// Secret signs every delivery (HMAC-SHA256 over the body). Server-generated
	// at registration, shown to the caller exactly once, and never serialised
	// into any later read — see WebhookDTO.
	Secret      string
	Active      bool
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Webhook limits, pinned by the CHECK constraints in migration 000029; the
// parity is one-directional here (Go refuses first, the DB backstops rows
// written around the service).
const (
	MinWebhookURLLen = 12 // "http://a.io/" — anything shorter names no host
	MaxWebhookURLLen = 2048
)

// ValidWebhookURL accepts an absolute http(s) URL with a host.
//
// A WHITELIST of schemes, not a blacklist: everything the sender must never
// follow (file:, javascript:, gopher:) is refused by not being named. Plain
// http stays legal because the platform is self-hosted and "the rebuild
// service on this box" is a legitimate receiver; that same choice means a
// registered URL can point INSIDE the network (SSRF is admin-gated, not
// impossible) — accepted for v1 and listed as an ADR-011 trigger.
func ValidWebhookURL(s string) bool {
	if len(s) < MinWebhookURLLen || len(s) > MaxWebhookURLLen {
		return false
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	return (scheme == "http" || scheme == "https") && u.Host != ""
}
