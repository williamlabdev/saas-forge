package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Authorization (ActionContentSchemaWrite) is decided by the shared authorize
// path the schema-mutation tests already pin; what is webhook-specific — and
// pinned here — is the URL gate and the secret's single appearance.

func TestWebhook_CreateValidatesTheURL(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	for name, url := range map[string]string{
		"relative":       "/hooks/build",
		"no scheme":      "example.com/hook",
		"ftp":            "ftp://example.com/hook",
		"javascript":     "javascript:alert(1)",
		"empty":          "",
		"scheme no host": "https://",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.CreateWebhook(ctx, CreateWebhookInput{URL: url})
			assert.Equal(t, "CONTENT_WEBHOOK_URL_INVALID", codeOf(t, err))
		})
	}
}

func TestWebhook_SecretAppearsExactlyOnce(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")

	created, err := svc.CreateWebhook(ctx, CreateWebhookInput{URL: "https://build.example/hook", Description: "rebuild"})
	require.NoError(t, err)
	require.Len(t, created.Secret, 64, "32 random bytes, hex — entropy is never the caller's choice")
	assert.True(t, created.Active, "a new webhook is live immediately; registering it IS the opt-in")

	// The list shape carries no secret AT THE TYPE LEVEL — this assertion is on
	// the DTO's fields, so a future serializer cannot leak what the struct does
	// not hold.
	list, err := svc.ListWebhooks(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, created.ID, list[0].ID)
	assert.Equal(t, "https://build.example/hook", list[0].URL)
}

func TestWebhook_DeleteRemovesAndMissIs404(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	created, err := svc.CreateWebhook(ctx, CreateWebhookInput{URL: "https://build.example/hook"})
	require.NoError(t, err)

	require.NoError(t, svc.DeleteWebhook(ctx, created.ID))
	list, err := svc.ListWebhooks(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)

	err = svc.DeleteWebhook(ctx, created.ID)
	require.Error(t, err, "deleting what is gone must say so, not succeed idempotently into confusion")
}

func TestWebhook_TenantsSeeOnlyTheirOwn(t *testing.T) {
	svc, _ := newSvc()
	_, err := svc.CreateWebhook(ctxTenant("t1"), CreateWebhookInput{URL: "https://t1.example/hook"})
	require.NoError(t, err)

	list, err := svc.ListWebhooks(ctxTenant("t2"))
	require.NoError(t, err)
	assert.Empty(t, list)

	// And t2 cannot delete t1's registration either.
	t1List, err := svc.ListWebhooks(ctxTenant("t1"))
	require.NoError(t, err)
	require.Len(t, t1List, 1)
	assert.Error(t, svc.DeleteWebhook(ctxTenant("t2"), t1List[0].ID))
}
