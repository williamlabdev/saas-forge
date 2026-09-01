// Command webhook-consumer is a reference receiver for the CMS content events
// (ADR-011). It is not a platform service — it is the thing a TENANT writes,
// kept in-tree so the contract has an executable definition instead of a
// paragraph in a document.
//
// It demonstrates the four things every receiver has to get right:
//
//  1. Verify the HMAC over the RAW BODY BYTES before parsing anything.
//  2. Deduplicate on X-Webhook-Delivery — delivery is at-least-once, and a
//     retry re-sends the same id.
//  3. Answer fast. The sender's timeout is 10s and the retry budget is shared
//     by every endpoint of the tenant, so a slow receiver spends other
//     receivers' retries. Ack, then work.
//  4. Fetch the content separately. The payload names WHAT changed and never
//     what it says (ADR-009): the receiver reads it back through the delivery
//     edge, with its own credentials and its own ETag, and therefore sees
//     exactly what it is allowed to see.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
)

// maxBodyBytes caps what we will read and sign-check. The events are tiny; a
// receiver that reads an unbounded body from the internet is a receiver that
// can be made to allocate arbitrarily.
const maxBodyBytes = 64 << 10

// Event is the thin payload (see internal/pkg/outbox.ContentEventPayload). It
// is duplicated rather than imported because a real tenant's consumer is a
// different program in a different language — if this file needed the platform
// module to compile, it would not be demonstrating anything.
type Event struct {
	TenantID    string `json:"tenant_id"`
	EntryID     string `json:"entry_id"`
	ContentType string `json:"content_type"`
	Locale      string `json:"locale,omitempty"`
}

// Handler is the receiving half. Work is handed to `act`, which runs after the
// response is sent.
type Handler struct {
	secret []byte
	seen   *deliveryLog
	act    func(eventType string, ev Event)
	// spawn runs the action. It exists so the tests can make dispatch
	// synchronous: asserting "the action did NOT run" against a goroutine can
	// only be done by waiting, and a test that passes because it waited long
	// enough is a test that fails on a loaded machine.
	spawn func(func())
}

func NewHandler(secret string, seen *deliveryLog, act func(string, Event)) *Handler {
	return &Handler{
		secret: []byte(secret),
		seen:   seen,
		act:    act,
		spawn:  func(f func()) { go f() },
	}
}

var errBadSignature = errors.New("signature does not match")

// verify recomputes the HMAC over the exact bytes received. Constant-time
// compare: a byte-by-byte one leaks, through timing, how much of a forged
// signature was right, which is enough to build one.
func verify(secret, body []byte, header string) error {
	// The header is `sha256=<hex>`; accept the bare hex too, since that is the
	// single most common thing a receiver gets wrong when hand-rolling this.
	got := header
	if _, hexPart, found := strings.Cut(header, "="); found {
		got = hexPart
	}
	sum, err := hex.DecodeString(got)
	if err != nil {
		return errBadSignature
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	if !hmac.Equal(sum, mac.Sum(nil)) {
		return errBadSignature
	}
	return nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		// Nothing was verified, so nothing can be trusted — but the read
		// failing is our problem, not the sender's. 500 asks for the retry.
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}

	// Verification comes FIRST. Parsing unverified JSON means running a parser
	// on bytes from anyone who found the URL.
	if err := verify(h.secret, body, r.Header.Get("X-Webhook-Signature")); err != nil {
		// 401 rather than 400: the sender retries any non-2xx, and a forged
		// call should look the same on every attempt.
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}

	delivery := r.Header.Get("X-Webhook-Delivery")
	eventType := r.Header.Get("X-Webhook-Event")
	if delivery == "" || eventType == "" {
		http.Error(w, "missing delivery headers", http.StatusBadRequest)
		return
	}

	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil || ev.TenantID == "" {
		// Signed but unparseable is not retryable — 200 so the sender stops.
		// Silently accepting it would be worse: log it loudly.
		logf("webhook %s: signed payload is not a content event: %v", delivery, err)
		w.WriteHeader(http.StatusOK)
		return
	}

	// At-least-once: the same delivery id arrives again whenever our previous
	// answer did not reach the sender. Recording it BEFORE acting makes the
	// action at-most-once; recording after makes it at-least-once. Which one
	// you want depends on whether your action is idempotent — a cache purge is
	// (do it again, no harm), a "send the author an email" is not.
	if !h.seen.first(delivery) {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Ack before working. See the package comment: the retry budget is shared.
	w.WriteHeader(http.StatusOK)
	h.spawn(func() { h.act(eventType, ev) })
}

// deliveryLog remembers delivery ids. Bounded on purpose: an unbounded set is
// a memory leak with a slow fuse, and the retries this exists to absorb all
// arrive within the outbox's retry window. A real consumer puts this in
// whatever durable store it already runs — the point is that it is bounded and
// that "have I seen this id" is answered before the work, not after.
type deliveryLog struct {
	mu    sync.Mutex
	limit int
	ids   map[string]struct{}
	order []string
}

func newDeliveryLog(limit int) *deliveryLog {
	return &deliveryLog{limit: limit, ids: make(map[string]struct{}, limit)}
}

// first reports whether this delivery id is new, and records it.
func (d *deliveryLog) first(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, dup := d.ids[id]; dup {
		return false
	}
	d.ids[id] = struct{}{}
	d.order = append(d.order, id)
	if len(d.order) > d.limit {
		delete(d.ids, d.order[0])
		d.order = d.order[1:]
	}
	return true
}
