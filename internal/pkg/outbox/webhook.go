package outbox

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// WebhookEndpoint is one registered receiver of a tenant's content events.
type WebhookEndpoint struct {
	ID     uuid.UUID
	URL    string
	Secret string
}

// WebhookDirectory answers "who subscribes to this tenant's content events".
// Implemented by the CMS repository; an interface here so the outbox package
// depends on no CMS type.
type WebhookDirectory interface {
	ActiveWebhookEndpoints(ctx context.Context, tenantID string) ([]WebhookEndpoint, error)
}

// WebhookSender delivers one signed event to one endpoint. An interface so the
// worker tests can fail exactly one endpoint; HTTPWebhookSender is the real one.
type WebhookSender interface {
	Send(ctx context.Context, ep WebhookEndpoint, eventType, deliveryID string, body json.RawMessage) error
}

// HTTPWebhookSender POSTs the event body with an HMAC-SHA256 signature.
//
// The signature is over the RAW BODY BYTES with the endpoint's secret —
// receivers verify with constant-time comparison before parsing anything.
// Redirects are refused: a 3xx from the registered URL would re-send the
// signed body to a location nobody registered.
type HTTPWebhookSender struct {
	client *http.Client
}

// webhookTimeout bounds one delivery attempt. A receiver that needs longer
// than this should ack fast and process async — the retry budget (outbox
// maxRetries) is shared by every endpoint of the tenant, so one slow receiver
// must not be able to spend it.
const webhookTimeout = 10 * time.Second

func NewHTTPWebhookSender() *HTTPWebhookSender {
	return &HTTPWebhookSender{
		client: &http.Client{
			Timeout: webhookTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (s *HTTPWebhookSender) Send(ctx context.Context, ep WebhookEndpoint, eventType, deliveryID string, body json.RawMessage) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook %s: build request: %w", ep.ID, err)
	}
	mac := hmac.New(sha256.New, []byte(ep.Secret))
	mac.Write(body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Event", eventType)
	// The delivery id is the outbox row id: a retry re-sends the SAME id, which
	// is what lets an at-least-once receiver deduplicate.
	req.Header.Set("X-Webhook-Delivery", deliveryID)
	req.Header.Set("X-Webhook-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook %s: %w", ep.ID, err)
	}
	defer resp.Body.Close()
	// Drain a bounded slice so the connection can be reused; the body is not
	// part of the contract.
	_, _ = io.CopyN(io.Discard, resp.Body, 4096)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook %s: receiver answered %d", ep.ID, resp.StatusCode)
	}
	return nil
}
