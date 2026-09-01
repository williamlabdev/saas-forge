package response

import (
	"encoding/json"
	stderrors "errors"
	"net/http/httptest"
	"strings"
	"testing"

	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

func decode(t *testing.T, body []byte) Envelope {
	t.Helper()
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return env
}

func TestError_RendersDetails(t *testing.T) {
	rec := httptest.NewRecorder()
	err := apperrors.New("VALIDATION_FAILED", "validation failed", 400).
		WithDetail("email", "must be a valid address")

	Error(rec, err)

	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	env := decode(t, rec.Body.Bytes())
	if env.Error == nil {
		t.Fatal("expected error body")
	}
	if env.Error.Code != "VALIDATION_FAILED" {
		t.Fatalf("code = %q", env.Error.Code)
	}
	if got := env.Error.Details["email"]; got != "must be a valid address" {
		t.Fatalf("details[email] = %v", got)
	}
}

func TestError_OmitsDetailsWhenEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	Error(rec, apperrors.ErrNotFound)

	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	// details should be omitted from the JSON entirely when nil.
	if got := rec.Body.String(); strings.Contains(got, "\"details\"") {
		t.Fatalf("expected details to be omitted, got %s", got)
	}
}

func TestError_NonAppErrorMapsTo500(t *testing.T) {
	rec := httptest.NewRecorder()
	Error(rec, stderrors.New("boom"))

	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	env := decode(t, rec.Body.Bytes())
	if env.Error == nil || env.Error.Code != "INTERNAL_ERROR" {
		t.Fatalf("expected INTERNAL_ERROR, got %+v", env.Error)
	}
}

// refusesToMarshal stands in for a DTO that declines to render — the content
// service's audience projection refuses when nobody decided which audience a
// response is for, rather than guessing and risking a draft leak.
type refusesToMarshal struct{}

func (refusesToMarshal) MarshalJSON() ([]byte, error) {
	return nil, stderrors.New("no audience")
}

// A body that cannot be rendered must not arrive as a success.
//
// Encoding straight into the ResponseWriter commits the status line first, so
// the failure used to surface as 200 with an empty body — indistinguishable
// from a legitimately empty resource, and therefore the quietest possible way
// for a deliberate refusal to be mistaken for data. The envelope is marshalled
// before any header is written so the refusal can still become a 500.
func TestJSON_UnrenderableBodyBecomes500NotEmpty200(t *testing.T) {
	rec := httptest.NewRecorder()
	JSON(rec, 200, refusesToMarshal{})

	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500 — an unrenderable body must not report success", rec.Code)
	}
	env := decode(t, rec.Body.Bytes())
	if env.Error == nil || env.Error.Code != "RESPONSE_ENCODE_FAILED" {
		t.Fatalf("expected RESPONSE_ENCODE_FAILED, got %+v", env.Error)
	}
	if env.Data != nil {
		t.Fatalf("a failed render must carry no data, got %v", env.Data)
	}
}
