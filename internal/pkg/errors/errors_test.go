package errors

import (
	stderrors "errors"
	"fmt"
	"testing"
)

func TestIs_MatchesByCodeThroughWrap(t *testing.T) {
	sentinel := New("INVOICE_INVALID_STATUS_TRANSITION", "invalid status transition", 409)

	// A fresh AppError with the same code matches the sentinel even though it is
	// a different pointer.
	same := New("INVOICE_INVALID_STATUS_TRANSITION", "different message", 409)
	if !Is(same, sentinel) {
		t.Fatal("expected same-code AppError to match sentinel")
	}

	// Wrapped in a cause chain, it still matches via errors.Is.
	wrapped := Wrap("DB_ERROR", "query failed", 500, sentinel)
	if !Is(wrapped, sentinel) {
		t.Fatal("expected wrapped error to match sentinel through the cause chain")
	}

	// Wrapped under a non-stdlib formatting wrapper too.
	fwrapped := fmt.Errorf("context: %w", sentinel)
	if !Is(fwrapped, sentinel) {
		t.Fatal("expected fmt-wrapped error to match sentinel")
	}
}

func TestIs_DistinctCodesDoNotMatch(t *testing.T) {
	if Is(ErrNotFound, ErrUnauthorized) {
		t.Fatal("errors with distinct codes must not match")
	}
	if Is(ErrUnauthorized, ErrNotFound) {
		t.Fatal("errors with distinct codes must not match (reverse)")
	}
}

func TestIs_NonAppErrorTarget(t *testing.T) {
	plain := stderrors.New("plain")
	if Is(ErrNotFound, plain) {
		t.Fatal("AppError must not match a non-AppError target")
	}
}

func TestWithDetails_DoesNotMutateSentinel(t *testing.T) {
	withField := ErrInvalidEmail.WithDetail("field", "email")

	if ErrInvalidEmail.Details != nil {
		t.Fatalf("WithDetail mutated the shared sentinel: %v", ErrInvalidEmail.Details)
	}
	if got := withField.Details["field"]; got != "email" {
		t.Fatalf("expected detail field=email, got %v", got)
	}
	// Code/message/status are preserved on the copy, and Is still matches.
	if withField.Code != ErrInvalidEmail.Code || withField.HTTPStatus != ErrInvalidEmail.HTTPStatus {
		t.Fatal("WithDetail should preserve code and status")
	}
	if !Is(withField, ErrInvalidEmail) {
		t.Fatal("a detail-augmented error must still match its sentinel")
	}
}

func TestWithDetails_Merges(t *testing.T) {
	base := New("VALIDATION_FAILED", "validation failed", 400).
		WithDetail("a", 1).
		WithDetails(map[string]any{"b": 2, "c": 3})

	for k, want := range map[string]any{"a": 1, "b": 2, "c": 3} {
		if base.Details[k] != want {
			t.Fatalf("detail %q = %v, want %v", k, base.Details[k], want)
		}
	}
}
