package response

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

type Envelope struct {
	Data  any        `json:"data"`
	Error *ErrorBody `json:"error"`
	Meta  Meta       `json:"meta"`
}

type ErrorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type Meta struct {
	Timestamp string `json:"timestamp"`
	Page      any    `json:"page,omitempty"`
}

func JSON(w http.ResponseWriter, status int, data any) {
	JSONWithMeta(w, status, data, Meta{Timestamp: time.Now().UTC().Format(time.RFC3339)})
}

// JSONWithMeta writes a success envelope with optional pagination meta.
func JSONWithMeta(w http.ResponseWriter, status int, data any, meta Meta) {
	if meta.Timestamp == "" {
		meta.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	write(w, status, Envelope{
		Data:  data,
		Error: nil,
		Meta:  meta,
	})
}

func Error(w http.ResponseWriter, err error) {
	ae, ok := apperrors.As(err)
	if !ok {
		ae = apperrors.New("INTERNAL_ERROR", "internal server error", 500)
	}
	// Server errors must not vanish silently: log the underlying error so the
	// cause is recoverable even though the client only sees a generic 5xx body.
	if ae.HTTPStatus >= 500 {
		log.Printf("response: %d %s: %v", ae.HTTPStatus, ae.Code, err)
	}
	write(w, ae.HTTPStatus, Envelope{
		Data: nil,
		Error: &ErrorBody{
			Code:    ae.Code,
			Message: ae.Message,
			Details: ae.Details,
		},
		Meta: metaNow(),
	})
}

// write renders the envelope BEFORE committing a status code.
//
// The order matters. Encoding straight into the ResponseWriter sends the status
// line first, so a body that fails to marshal leaves the caller holding a 200
// with nothing after it — a success no consumer can distinguish from an empty
// resource. That is not hypothetical: a DTO whose MarshalJSON refuses to render
// (see the content service's audience projection, which fails closed rather
// than guess) reaches exactly this path, and a silent 200 would turn a
// deliberate refusal into the quietest possible bug.
//
// Marshalling first costs one buffer and makes the failure a 500 that says so.
func write(w http.ResponseWriter, status int, body Envelope) {
	b, err := json.Marshal(body)
	if err != nil {
		log.Printf("response: encode failed for %d: %v", status, err)
		b, status = encodeFailureBody(), http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

// encodeFailureBody is built by hand: the envelope that just failed to marshal
// is not a witness worth trusting to marshal a second time.
func encodeFailureBody() []byte {
	b, err := json.Marshal(Envelope{
		Error: &ErrorBody{Code: "RESPONSE_ENCODE_FAILED", Message: "internal server error"},
		Meta:  metaNow(),
	})
	if err != nil {
		return []byte(`{"data":null,"error":{"code":"RESPONSE_ENCODE_FAILED","message":"internal server error"},"meta":{}}`)
	}
	return b
}

func metaNow() Meta {
	return Meta{Timestamp: time.Now().UTC().Format(time.RFC3339)}
}

// MetaWithPage builds meta including cursor pagination fields.
func MetaWithPage(page any) Meta {
	m := metaNow()
	m.Page = page
	return m
}
