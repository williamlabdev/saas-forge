package graph

import (
	"errors"
	"net/http"
	"testing"

	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/williamlabdev/saas-forge/apps/bff/internal/domainapi"
)

// The console tells 403 apart from 404 by reading extensions.code, and its own
// tests answer with a HAND-BUILT copy of this envelope. Two halves of one
// contract, written twice — so this pins the half the fixture cannot reach. If
// only the fixture existed, both could drift together and keep agreeing.
func TestMapAPIError_CarriesCodeAndStatus(t *testing.T) {
	err := mapAPIError(&domainapi.APIError{
		Code:       "FORBIDDEN",
		Message:    "insufficient permissions",
		HTTPStatus: http.StatusForbidden,
	})

	var ge *gqlerror.Error
	if !errors.As(err, &ge) {
		t.Fatalf("want *gqlerror.Error, got %T", err)
	}
	// The message keeps its old shape on purpose: anything already reading it
	// predates the extensions and must not break.
	if ge.Message != "FORBIDDEN: insufficient permissions" {
		t.Errorf("message=%q", ge.Message)
	}
	if ge.Extensions["code"] != "FORBIDDEN" {
		t.Errorf("code=%v", ge.Extensions["code"])
	}
	if ge.Extensions["status"] != http.StatusForbidden {
		t.Errorf("status=%v", ge.Extensions["status"])
	}
}

// Not every failure comes from the domain — a transport error has no code to
// carry, and dressing it up as one would let a network fault render as a
// verdict about permissions.
func TestMapAPIError_LeavesNonAPIErrorsAlone(t *testing.T) {
	plain := errors.New("dial tcp: connection refused")
	if got := mapAPIError(plain); got != plain {
		t.Fatalf("got %v, want the original error", got)
	}
}
