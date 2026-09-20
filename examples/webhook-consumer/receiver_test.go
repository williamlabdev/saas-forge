package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func sign(body string) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

const validBody = `{"tenant_id":"acme","entry_id":"e1","content_type":"article","locale":"default"}`

// acted records what the handler dispatched. Dispatch is made synchronous in
// these tests (see Handler.spawn) so "it did not act" is a fact rather than a
// timeout.
type acted struct {
	calls []string
}

func (a *acted) fn(eventType string, ev Event) {
	a.calls = append(a.calls, eventType+" "+ev.EntryID)
}

func post(t *testing.T, h http.Handler, body, sig, delivery, event string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	if sig != "" {
		r.Header.Set("X-Webhook-Signature", sig)
	}
	if delivery != "" {
		r.Header.Set("X-Webhook-Delivery", delivery)
	}
	if event != "" {
		r.Header.Set("X-Webhook-Event", event)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func newTestHandler() (*Handler, *acted) {
	a := &acted{}
	h := NewHandler(testSecret, newDeliveryLog(16), a.fn)
	h.spawn = func(f func()) { f() } // synchronous: see acted
	return h, a
}

func TestAcceptsAProperlySignedEvent(t *testing.T) {
	h, a := newTestHandler()
	w := post(t, h, validBody, sign(validBody), "d1", "content.entry.published")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"content.entry.published e1"}, a.calls)
}

func TestAcceptsTheBareHexSignatureToo(t *testing.T) {
	// Receivers routinely strip the prefix; accepting both is one less way to
	// be mysteriously 401 in production.
	h, a := newTestHandler()
	bare := strings.TrimPrefix(sign(validBody), "sha256=")
	w := post(t, h, validBody, bare, "d1", "content.entry.published")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Len(t, a.calls, 1)
}

func TestRefusesATamperedBody(t *testing.T) {
	h, a := newTestHandler()
	tampered := strings.Replace(validBody, "acme", "evil", 1)
	// Signature of the ORIGINAL body, body swapped underneath it.
	w := post(t, h, tampered, sign(validBody), "d1", "content.entry.published")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, a.calls, "a tampered body must not reach the action")
}

func TestRefusesAForeignSecret(t *testing.T) {
	h, a := newTestHandler()
	mac := hmac.New(sha256.New, []byte("a different tenant's secret"))
	mac.Write([]byte(validBody))
	w := post(t, h, validBody, "sha256="+hex.EncodeToString(mac.Sum(nil)), "d1", "content.entry.published")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, a.calls)
}

func TestRefusesAMissingOrMalformedSignature(t *testing.T) {
	for _, sig := range []string{"", "sha256=not-hex", "sha256=", "garbage"} {
		h, a := newTestHandler()
		w := post(t, h, validBody, sig, "d1", "content.entry.published")
		assert.Equalf(t, http.StatusUnauthorized, w.Code, "signature %q", sig)
		assert.Empty(t, a.calls)
	}
}

func TestVerificationHappensBeforeParsing(t *testing.T) {
	// Garbage that is BOTH unsigned and unparseable must come back 401, not
	// 400: a 400 would prove the parser ran on unverified bytes. This is the
	// only way the ordering is observable from outside.
	h, a := newTestHandler()
	w := post(t, h, "}{ not json at all", "sha256=00", "d1", "content.entry.published")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, a.calls)
}

func TestRequiresTheDeliveryHeaders(t *testing.T) {
	// Without a delivery id there is no dedup key, so accepting the event
	// would silently make every retry a duplicate action.
	h, _ := newTestHandler()
	assert.Equal(t, http.StatusBadRequest,
		post(t, h, validBody, sign(validBody), "", "content.entry.published").Code)
	assert.Equal(t, http.StatusBadRequest,
		post(t, h, validBody, sign(validBody), "d1", "").Code)
}

func TestActsOnceWhenTheSameDeliveryArrivesTwice(t *testing.T) {
	// This is the at-least-once contract: a retry re-sends the SAME id.
	h, a := newTestHandler()
	require.Equal(t, http.StatusOK, post(t, h, validBody, sign(validBody), "d1", "content.entry.published").Code)
	require.Equal(t, http.StatusOK, post(t, h, validBody, sign(validBody), "d1", "content.entry.published").Code)
	assert.Len(t, a.calls, 1, "the retry must not act a second time")
}

func TestDistinctDeliveriesBothAct(t *testing.T) {
	h, a := newTestHandler()
	post(t, h, validBody, sign(validBody), "d1", "content.entry.published")
	post(t, h, validBody, sign(validBody), "d2", "content.entry.updated")
	assert.Len(t, a.calls, 2)
}

func TestSignedButUnparseablePayloadIsNotRetried(t *testing.T) {
	// It carries our signature, so retrying it would just fail identically
	// forever and burn the tenant's shared retry budget.
	h, a := newTestHandler()
	body := `{"not":"an event"}`
	w := post(t, h, body, sign(body), "d1", "content.entry.published")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, a.calls)
}

func TestRefusesNonPost(t *testing.T) {
	h, _ := newTestHandler()
	r := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestOversizedBodyFailsVerificationRatherThanBeingRead(t *testing.T) {
	// The cap truncates before the HMAC is computed, so an oversized body can
	// never verify — which is the safe direction: it is refused, not trusted.
	h, a := newTestHandler()
	big := `{"tenant_id":"acme","entry_id":"e1","pad":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	w := post(t, h, big, sign(big), "d1", "content.entry.published")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, a.calls)
}

func TestTheRealDispatchAnswersBeforeTheWorkFinishes(t *testing.T) {
	// The other tests replace spawn, so this one covers the default: a slow
	// action must not hold the response open. The sender times out at 10s and
	// the retry budget is shared with the tenant's other endpoints.
	release := make(chan struct{})
	done := make(chan struct{})
	h := NewHandler(testSecret, newDeliveryLog(4), func(string, Event) {
		<-release
		close(done)
	})
	w := post(t, h, validBody, sign(validBody), "d1", "content.entry.published")
	assert.Equal(t, http.StatusOK, w.Code, "answered while the action is still blocked")
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the action never ran")
	}
}

func TestDeliveryLogEvictsOldestAndStaysBounded(t *testing.T) {
	d := newDeliveryLog(2)
	assert.True(t, d.first("a"))
	assert.True(t, d.first("b"))
	assert.False(t, d.first("a"), "still remembered")
	assert.True(t, d.first("c")) // evicts "a"
	assert.Len(t, d.ids, 2)
	assert.True(t, d.first("a"), "evicted, so it looks new again — bounded memory costs this")
}
