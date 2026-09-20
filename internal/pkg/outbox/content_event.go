package outbox

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Content-plane integration events (ADR-011). Emitted by the CMS repository in
// the SAME transaction as the write they describe — the transactional-outbox
// pattern this handle was retained for — and delivered to tenant-registered
// webhooks by the worker.
const (
	EventContentEntryCreated     = "content.entry.created"
	EventContentEntryUpdated     = "content.entry.updated"
	EventContentEntryDeleted     = "content.entry.deleted"
	EventContentEntryPublished   = "content.entry.published"
	EventContentEntryUnpublished = "content.entry.unpublished"
)

// IsContentEvent routes a row to the webhook deliverer. Prefix-matched so a
// future content event type added by the CMS is delivered rather than
// dead-lettered by the fail-loud default arm — the registry of legal types
// lives with the emitter, not here.
func IsContentEvent(eventType string) bool {
	return strings.HasPrefix(eventType, "content.")
}

// ContentEventPayload is deliberately THIN: it names WHAT changed, never what
// the content says. A body carrying the payload would hand every webhook
// receiver a copy of the document with no field-level read check and no
// draft/published distinction — a second read path around the entire
// authorization stack (ADR-009). A receiver that wants the content calls the
// API back with its own credential and gets exactly what that credential may
// see.
type ContentEventPayload struct {
	TenantID string `json:"tenant_id"`
	EntryID  string `json:"entry_id"`
	// ContentType is the type NAME (what consumers think in), resolved by the
	// emitter inside the same transaction.
	ContentType string `json:"content_type"`
	// Locale is empty on deletion events — the row is gone by the time the
	// event is assembled, and resurrecting it for one field is not worth a
	// second query shape.
	Locale string `json:"locale,omitempty"`
}

// ParseContentPayload decodes a content.* event payload.
func ParseContentPayload(raw json.RawMessage) (ContentEventPayload, error) {
	var p ContentEventPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return ContentEventPayload{}, fmt.Errorf("outbox: parse content payload: %w", err)
	}
	if p.TenantID == "" {
		return ContentEventPayload{}, fmt.Errorf("outbox: content payload names no tenant")
	}
	return p, nil
}
