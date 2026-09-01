package e2e_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestE2E_ContentEventsReachDelivery guards the wiring between a content write
// and the outbox worker's delivery path.
//
// It exists because that wiring silently rotted once: the e2e harness built its
// own worker by hand, production later gained WithContentWebhooks, and the hand
// -built copy kept nil webhooks. Every content event then failed delivery and
// retried into the dead-letter state — and the whole suite stayed green, because
// a passing test prints no log and nothing asserted on the outcome.
//
// Two assertions, because either one alone is blind:
//
//   - delivered must actually rise: a content write that enqueues nothing would
//     leave zero failed rows and pass the second assertion trivially.
//   - no row may end in 'failed' (the terminal dead-letter state): delivery that
//     errors and retries to exhaustion never touches the delivered counter, so
//     the first assertion cannot see it.
//
// The expected event count is written down here on purpose rather than read back
// from the system under test — one content type plus one entry is two events, and
// a change that alters that must change this line and say why.
func TestE2E_ContentEventsReachDelivery(t *testing.T) {
	requireE2E(t)
	ctx := context.Background()

	deliveredBefore := e2eOutboxReg.OutboxDelivered.Load()

	_, _, login := registerAndLogin(t, "obxdel")
	token := login["access_token"].(string)

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"obxwidget","label":"W","fields":[{"key":"title","type":"text","label":"Title","required":true}]}`,
		"Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=obxwidget",
		`{"title":"does this reach a handler"}`, "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// Wait for the worker to drain what this test enqueued. The type and the
	// entry are two content events; registerAndLogin adds user events on top,
	// so this is a floor, not an equality.
	const contentEvents = 2
	require.Eventually(t, func() bool {
		return e2eOutboxReg.OutboxDelivered.Load()-deliveredBefore >= contentEvents
	}, 15*time.Second, 100*time.Millisecond,
		"content events never reached a delivery handler: delivered rose by %d, want >= %d",
		e2eOutboxReg.OutboxDelivered.Load()-deliveredBefore, contentEvents)

	// 'failed' is terminal — the row exhausted its retries. Nothing in this suite
	// dead-letters on purpose, so any such row is a delivery path that is broken
	// and staying quiet about it. last_error names which one.
	var dead int
	var lastErrs string
	err := e2ePool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(STRING_AGG(DISTINCT last_error, ' | '), '')
		FROM integration_outbox
		WHERE status = 'failed'`).Scan(&dead, &lastErrs)
	require.NoError(t, err)
	require.Zero(t, dead, "outbox rows dead-lettered: %s", lastErrs)
}
