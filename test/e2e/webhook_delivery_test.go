package e2e_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// received is one webhook delivery as the receiver saw it.
type received struct {
	event     string
	delivery  string
	signature string
	body      []byte
}

// TestE2E_WebhookRegisteredThenContentEventArrives walks the whole announcement
// path in one test: a tenant registers an endpoint over HTTP, writes content,
// and a real HTTP receiver gets a signed delivery.
//
// Every segment of this path already had unit tests — the sender signs, a
// non-2xx is an error, a redirect is not followed, fan-out reaches every
// endpoint, and the reference consumer verifies. What nothing covered was the
// seam BETWEEN them: no test had ever registered a webhook against the running
// app, so sender.Send had never once executed in an assembled system. The
// wiring bug this suite carried for two sessions (a hand-built worker with nil
// webhooks) lived precisely in that gap.
func TestE2E_WebhookRegisteredThenContentEventArrives(t *testing.T) {
	requireE2E(t)

	var mu sync.Mutex
	var got []received
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, received{
			event:     r.Header.Get("X-Webhook-Event"),
			delivery:  r.Header.Get("X-Webhook-Delivery"),
			signature: r.Header.Get("X-Webhook-Signature"),
			body:      body,
		})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	_, _, login := registerAndLogin(t, "hookdel")
	token := login["access_token"].(string)

	// Register over HTTP, exactly as a tenant would. The secret appears here and
	// nowhere else, so this response is also the only way the test can verify a
	// signature — the same constraint every real consumer lives under.
	rec := doJSON(t, http.MethodPost, "/api/v1/content/webhooks",
		`{"url":"`+receiver.URL+`","description":"e2e receiver"}`,
		"Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	secret, _ := decodeDataMap(t, rec)["secret"].(string)
	require.NotEmpty(t, secret, "registration must hand back the signing secret")

	rec = doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"hookpost","label":"P","fields":[{"key":"title","type":"text","label":"Title","required":true}]}`,
		"Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=hookpost",
		`{"title":"announce me"}`, "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	snapshot := func() []received {
		mu.Lock()
		defer mu.Unlock()
		return append([]received(nil), got...)
	}
	require.Eventually(t, func() bool { return len(snapshot()) > 0 }, 15*time.Second, 100*time.Millisecond,
		"no webhook ever arrived at the registered endpoint")

	for _, d := range snapshot() {
		// Recomputed from the secret this test was handed, over the exact bytes
		// the receiver read. A signature checked any other way (say, against a
		// value the sender also produced) would prove nothing.
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(d.body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		require.Equal(t, want, d.signature, "signature does not verify against the registration secret")

		require.NotEmpty(t, d.event, "delivery carries no X-Webhook-Event")
		require.NotEmpty(t, d.delivery, "delivery carries no X-Webhook-Delivery (receivers dedupe on it)")

		var payload map[string]any
		require.NoError(t, json.Unmarshal(d.body, &payload), "payload is not JSON: %s", d.body)
		require.NotEmpty(t, payload["tenant_id"], "payload names no tenant: %s", d.body)
	}
}
