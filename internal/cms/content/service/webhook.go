package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Webhook registry verbs (ADR-011). Authorized as ActionContentSchemaWrite:
// registering a receiver decides where every future content event of the
// tenant is announced, which is operator-level configuration exactly as
// destructive schema verbs are. If webhooks ever need their own grantable
// verb, that split is an ADR-011 trigger, not a bigger list here.

// WebhookDTO is the read shape. Secret is ABSENT by construction, not by
// omission at one call site: the only struct that ever carries it outward is
// WebhookCreatedDTO, returned exactly once.
type WebhookDTO struct {
	ID          uuid.UUID `json:"id"`
	URL         string    `json:"url"`
	Active      bool      `json:"active"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

// WebhookCreatedDTO is the registration response — the one appearance of the
// secret. A caller that loses it deletes the webhook and registers again;
// that is the rotation story too, and it is deliberate (a readable secret is
// a secret every later reader of the list also has).
type WebhookCreatedDTO struct {
	WebhookDTO
	Secret string `json:"secret"`
}

type CreateWebhookInput struct {
	URL         string `json:"url"`
	Description string `json:"description"`
}

func webhookDTO(w *domain.Webhook) WebhookDTO {
	return WebhookDTO{
		ID: w.ID, URL: w.URL, Active: w.Active,
		Description: w.Description, CreatedAt: w.CreatedAt,
	}
}

func (s *contentService) CreateWebhook(ctx context.Context, in CreateWebhookInput) (WebhookCreatedDTO, error) {
	sub, err := s.authorize(ctx, ActionContentSchemaWrite, "webhooks", "")
	if err != nil {
		return WebhookCreatedDTO{}, err
	}
	rawURL := strings.TrimSpace(in.URL)
	if !domain.ValidWebhookURL(rawURL) {
		return WebhookCreatedDTO{}, apperrors.New("CONTENT_WEBHOOK_URL_INVALID", "webhook url must be absolute http(s) with a host", 422).
			WithDetails(map[string]any{"url": in.URL})
	}
	// 32 random bytes, hex — server-generated so its entropy is never the
	// caller's choice. The DB CHECK (16..128 chars) backstops rows written
	// around this path.
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return WebhookCreatedDTO{}, err
	}
	w := &domain.Webhook{
		ID:          uuid.New(),
		TenantID:    sub.TenantID,
		URL:         rawURL,
		Secret:      hex.EncodeToString(buf),
		Active:      true,
		Description: strings.TrimSpace(in.Description),
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.repo.CreateWebhook(ctx, w); err != nil {
		return WebhookCreatedDTO{}, err
	}
	return WebhookCreatedDTO{WebhookDTO: webhookDTO(w), Secret: w.Secret}, nil
}

func (s *contentService) ListWebhooks(ctx context.Context) ([]WebhookDTO, error) {
	sub, err := s.authorize(ctx, ActionContentSchemaWrite, "webhooks", "")
	if err != nil {
		return nil, err
	}
	ws, err := s.repo.ListWebhooks(ctx, sub.TenantID)
	if err != nil {
		return nil, err
	}
	out := make([]WebhookDTO, 0, len(ws))
	for _, w := range ws {
		out = append(out, webhookDTO(w))
	}
	return out, nil
}

func (s *contentService) DeleteWebhook(ctx context.Context, id uuid.UUID) error {
	sub, err := s.authorize(ctx, ActionContentSchemaWrite, "webhooks", "")
	if err != nil {
		return err
	}
	return s.repo.DeleteWebhook(ctx, sub.TenantID, id)
}
