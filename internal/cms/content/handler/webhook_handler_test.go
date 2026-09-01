package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"

	"github.com/williamlabdev/saas-forge/internal/cms/content/service"
)

// The webhook routes are a tenant's only way to say where its content events
// should go (ADR-011). Until now the service and repository layers were tested
// and the HTTP layer was not — so nothing checked the status codes, the id
// parsing, or the one rule that matters most here: the secret is returned by
// registration and by nothing else.

func TestCreateWebhook_OK(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{webhookCreated: service.WebhookCreatedDTO{
		WebhookDTO: service.WebhookDTO{ID: id, URL: "https://example.test/hook", Active: true},
		Secret:     "s3cr3t-value",
	}}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/webhooks",
		`{"url":"https://example.test/hook","description":"prod"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastWebhookCreate.URL != "https://example.test/hook" {
		t.Fatalf("url not plumbed through: %q", svc.lastWebhookCreate.URL)
	}
	if svc.lastWebhookCreate.Description != "prod" {
		t.Fatalf("description not plumbed through: %q", svc.lastWebhookCreate.Description)
	}
	var got struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	// Registration is the ONE response carrying the secret. A caller that cannot
	// read it here can never read it — there is no GET-one and no rotation verb.
	if got.Data["secret"] != "s3cr3t-value" {
		t.Fatalf("registration must return the signing secret, got %v", got.Data["secret"])
	}
}

func TestCreateWebhook_InvalidJSON(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodPost, "/api/v1/content/webhooks", `{bad`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestCreateWebhook_ServiceErrorKeepsItsStatus(t *testing.T) {
	svc := &fakeContentService{err: apperrors.New("WEBHOOK_URL_INVALID", "url must be https", http.StatusUnprocessableEntity)}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/webhooks", `{"url":"http://insecure.test"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("the service's status must survive the handler: code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestListWebhooks_OK(t *testing.T) {
	svc := &fakeContentService{webhookList: []service.WebhookDTO{
		{ID: uuid.New(), URL: "https://a.test/hook", Active: true, CreatedAt: time.Unix(0, 0).UTC()},
		{ID: uuid.New(), URL: "https://b.test/hook", Active: false, CreatedAt: time.Unix(0, 0).UTC()},
	}}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/webhooks", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var got struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if len(got.Data.Items) != 2 {
		t.Fatalf("items=%d, want 2: %s", len(got.Data.Items), rec.Body)
	}
	// The listing must never carry secrets. It reads as safe today only because
	// WebhookDTO has no Secret field — swap in WebhookCreatedDTO here (an easy
	// "just reuse the type" edit) and every later reader of the list gets every
	// tenant's signing key. Assert on the wire bytes, not on the Go type.
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("the listing leaked a secret field: %s", rec.Body)
	}
}

func TestListWebhooks_ServiceError(t *testing.T) {
	svc := &fakeContentService{err: apperrors.New("FORBIDDEN", "nope", http.StatusForbidden)}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/webhooks", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestDeleteWebhook_OK(t *testing.T) {
	id := uuid.New()
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodDelete, "/api/v1/content/webhooks/"+id.String(), "")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.lastID != id {
		t.Fatalf("deleted %s, want %s", svc.lastID, id)
	}
	// No assertion that the body is empty: response.JSON writes the envelope
	// unconditionally, so every 204 in this codebase carries one. That is the
	// existing, deliberate convention (see the type-delete case in handler_test.go)
	// — asserting otherwise here would put this route at odds with its siblings.
}

func TestDeleteWebhook_InvalidID(t *testing.T) {
	rec := do(t, &fakeContentService{}, http.MethodDelete, "/api/v1/content/webhooks/not-a-uuid", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestDeleteWebhook_NotFound(t *testing.T) {
	svc := &fakeContentService{err: apperrors.New("WEBHOOK_NOT_FOUND", "no such webhook", http.StatusNotFound)}
	rec := do(t, svc, http.MethodDelete, "/api/v1/content/webhooks/"+uuid.New().String(), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}
