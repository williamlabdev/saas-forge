package outbox

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/pkg/mcp"
	"github.com/williamlabdev/saas-forge/internal/pkg/metrics"
)

// --- fakes -----------------------------------------------------------------

type fakeDirectory struct {
	endpoints map[string][]WebhookEndpoint
	err       error
}

func (d *fakeDirectory) ActiveWebhookEndpoints(_ context.Context, tenantID string) ([]WebhookEndpoint, error) {
	if d.err != nil {
		return nil, d.err
	}
	return d.endpoints[tenantID], nil
}

type sentCall struct {
	Endpoint   WebhookEndpoint
	EventType  string
	DeliveryID string
}

type fakeSender struct {
	sent    []sentCall
	failURL string // Send fails for this URL
}

func (s *fakeSender) Send(_ context.Context, ep WebhookEndpoint, eventType, deliveryID string, _ json.RawMessage) error {
	if s.failURL != "" && ep.URL == s.failURL {
		return errors.New("receiver down")
	}
	s.sent = append(s.sent, sentCall{ep, eventType, deliveryID})
	return nil
}

func contentRow(t *testing.T, tenant string) Row {
	t.Helper()
	payload, err := json.Marshal(ContentEventPayload{
		TenantID: tenant, EntryID: uuid.NewString(), ContentType: "article", Locale: "default",
	})
	require.NoError(t, err)
	return Row{ID: uuid.New(), AggregateID: uuid.New(), EventType: EventContentEntryPublished, Payload: payload}
}

func webhookWorker(repo Repository, dir WebhookDirectory, sender WebhookSender) *Worker {
	return NewWorker(repo, mcp.NoopClient{}, 10, 3, time.Minute, metrics.NewRegistry()).
		WithContentWebhooks(dir, sender)
}

// --- worker routing ----------------------------------------------------------

func TestWorker_ContentEventFansOutToEveryEndpoint(t *testing.T) {
	row := contentRow(t, "t1")
	eps := []WebhookEndpoint{
		{ID: uuid.New(), URL: "https://a.example/hook", Secret: "s1"},
		{ID: uuid.New(), URL: "https://b.example/hook", Secret: "s2"},
	}
	repo := &fakeRepo{claim: []Row{row}}
	sender := &fakeSender{}
	w := webhookWorker(repo, &fakeDirectory{endpoints: map[string][]WebhookEndpoint{"t1": eps}}, sender)

	require.NoError(t, w.processBatch(context.Background()))
	require.Len(t, sender.sent, 2)
	assert.Equal(t, row.ID.String(), sender.sent[0].DeliveryID,
		"the delivery id must be the ROW id — a retry re-sends the same id, which is what receivers dedupe on")
	assert.Equal(t, []uuid.UUID{row.ID}, repo.doneIDs)
	assert.Empty(t, repo.failCalls)
}

func TestWorker_OneFailingEndpointRetriesTheWholeRow(t *testing.T) {
	row := contentRow(t, "t1")
	eps := []WebhookEndpoint{
		{ID: uuid.New(), URL: "https://ok.example/hook", Secret: "s1"},
		{ID: uuid.New(), URL: "https://down.example/hook", Secret: "s2"},
	}
	repo := &fakeRepo{claim: []Row{row}}
	sender := &fakeSender{failURL: "https://down.example/hook"}
	w := webhookWorker(repo, &fakeDirectory{endpoints: map[string][]WebhookEndpoint{"t1": eps}}, sender)

	require.NoError(t, w.processBatch(context.Background()))
	assert.Empty(t, repo.doneIDs, "a partial delivery must not be marked done")
	require.Len(t, repo.failCalls, 1, "the row must take the retry path")
	assert.Len(t, sender.sent, 1, "the healthy endpoint was reached before the failure — at-least-once, by design")
}

func TestWorker_ZeroEndpointsIsDeliveredNotDeadLettered(t *testing.T) {
	// Refusing would dead-letter every content write made before the first
	// webhook is registered.
	row := contentRow(t, "t-nobody")
	repo := &fakeRepo{claim: []Row{row}}
	sender := &fakeSender{}
	w := webhookWorker(repo, &fakeDirectory{endpoints: map[string][]WebhookEndpoint{}}, sender)

	require.NoError(t, w.processBatch(context.Background()))
	assert.Equal(t, []uuid.UUID{row.ID}, repo.doneIDs)
	assert.Empty(t, sender.sent)
	assert.Empty(t, repo.failCalls)
}

func TestWorker_ContentEventWithoutWiringFailsLoud(t *testing.T) {
	// Same rule as an unknown event type (TKT-OBX-2): a deployment emitting
	// content events with no webhook route must surface in retries and metrics,
	// not silently count as delivered.
	row := contentRow(t, "t1")
	repo := &fakeRepo{claim: []Row{row}}
	w := NewWorker(repo, mcp.NoopClient{}, 10, 3, time.Minute, metrics.NewRegistry())

	require.NoError(t, w.processBatch(context.Background()))
	assert.Empty(t, repo.doneIDs)
	require.Len(t, repo.failCalls, 1)
}

func TestWorker_DirectoryErrorRetries(t *testing.T) {
	row := contentRow(t, "t1")
	repo := &fakeRepo{claim: []Row{row}}
	w := webhookWorker(repo, &fakeDirectory{err: errors.New("db down")}, &fakeSender{})

	require.NoError(t, w.processBatch(context.Background()))
	assert.Empty(t, repo.doneIDs)
	require.Len(t, repo.failCalls, 1)
}

// --- HTTP sender ---------------------------------------------------------------

func TestHTTPWebhookSender_SignsAndDelivers(t *testing.T) {
	secret := "0123456789abcdef0123456789abcdef"
	body := json.RawMessage(`{"tenant_id":"t1","entry_id":"e1","content_type":"article"}`)
	deliveryID := uuid.NewString()

	var got struct {
		body      []byte
		event     string
		delivery  string
		signature string
		ctype     string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.body = b
		got.event = r.Header.Get("X-Webhook-Event")
		got.delivery = r.Header.Get("X-Webhook-Delivery")
		got.signature = r.Header.Get("X-Webhook-Signature")
		got.ctype = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	sender := NewHTTPWebhookSender()
	ep := WebhookEndpoint{ID: uuid.New(), URL: srv.URL, Secret: secret}
	require.NoError(t, sender.Send(context.Background(), ep, EventContentEntryPublished, deliveryID, body))

	assert.JSONEq(t, string(body), string(got.body))
	assert.Equal(t, EventContentEntryPublished, got.event)
	assert.Equal(t, deliveryID, got.delivery)
	assert.Equal(t, "application/json", got.ctype)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	assert.Equal(t, "sha256="+hex.EncodeToString(mac.Sum(nil)), got.signature,
		"the signature must verify against the RAW body bytes with the endpoint's secret")
}

func TestHTTPWebhookSender_NonSuccessStatusIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	sender := NewHTTPWebhookSender()
	err := sender.Send(context.Background(), WebhookEndpoint{ID: uuid.New(), URL: srv.URL, Secret: "0123456789abcdef"},
		EventContentEntryUpdated, uuid.NewString(), json.RawMessage(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestHTTPWebhookSender_RedirectIsNotFollowed(t *testing.T) {
	// A 3xx would re-send the signed body to a location nobody registered.
	var followed bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed = true
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()

	sender := NewHTTPWebhookSender()
	err := sender.Send(context.Background(), WebhookEndpoint{ID: uuid.New(), URL: srv.URL, Secret: "0123456789abcdef"},
		EventContentEntryUpdated, uuid.NewString(), json.RawMessage(`{}`))
	require.Error(t, err, "a redirect answer is a failed delivery")
	assert.False(t, followed)
}
