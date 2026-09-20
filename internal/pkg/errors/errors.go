package errors

import (
	stderrors "errors"
	"fmt"

	pkgerrors "github.com/pkg/errors"
)

// AppError is a domain/API error with a stable code and HTTP status. It is the
// base type for business exceptions: domains declare package-level sentinels via
// New (see internal/<domain>/service/errors.go) and the HTTP layer renders Code,
// Message, HTTPStatus and any Details into the response envelope.
type AppError struct {
	Code       string
	Message    string
	HTTPStatus int
	// Details carries optional structured context (e.g. per-field validation
	// errors). It is rendered by the HTTP layer when non-empty. Set it via
	// WithDetail/WithDetails, which copy the error so shared sentinels are never
	// mutated.
	Details map[string]any
	cause   error
}

func (e *AppError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.cause)
	}
	return e.Message
}

func (e *AppError) Unwrap() error { return e.cause }

// Is matches by stable Code rather than pointer identity, so a business
// exception still matches its sentinel after being wrapped (Wrap allocates a new
// *AppError). This is invoked by errors.Is via the standard library.
func (e *AppError) Is(target error) bool {
	var t *AppError
	if stderrors.As(target, &t) {
		return e.Code == t.Code
	}
	return false
}

// WithDetail returns a copy of e with key=value added to Details. It never
// mutates the receiver, so it is safe to call on shared package-level sentinels.
func (e *AppError) WithDetail(key string, value any) *AppError {
	return e.WithDetails(map[string]any{key: value})
}

// WithDetails returns a copy of e with the given details merged into Details.
// Like WithDetail it copies the receiver rather than mutating it.
func (e *AppError) WithDetails(details map[string]any) *AppError {
	clone := *e
	clone.Details = make(map[string]any, len(e.Details)+len(details))
	for k, v := range e.Details {
		clone.Details[k] = v
	}
	for k, v := range details {
		clone.Details[k] = v
	}
	return &clone
}

// New returns a domain error without a wrapped cause.
func New(code, message string, httpStatus int) *AppError {
	return &AppError{Code: code, Message: message, HTTPStatus: httpStatus}
}

// Wrap maps infrastructure failures to a domain error while preserving the cause chain.
func Wrap(code, message string, httpStatus int, cause error) *AppError {
	return &AppError{Code: code, Message: message, HTTPStatus: httpStatus, cause: cause}
}

// Wrapf wraps a formatted infrastructure error.
func Wrapf(code, message string, httpStatus int, cause error, format string, args ...any) *AppError {
	return Wrap(code, message, httpStatus, pkgerrors.Wrapf(cause, format, args...))
}

// As extracts an AppError from the chain.
func As(err error) (*AppError, bool) {
	var ae *AppError
	if stderrors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// Is reports whether err matches target in the AppError chain.
func Is(err, target error) bool {
	return stderrors.Is(err, target)
}

// Framework-level sentinels shared across every domain. These stay in this
// package because they are not specific to any one domain. Domain-specific
// business exceptions instead live next to their service as package-level
// AppError values, named with a "DOMAIN_REASON" code (e.g. USER_EMAIL_TAKEN,
// INVOICE_INVALID_STATUS_TRANSITION) — see internal/<domain>/service/errors.go.
var (
	// ErrNotFound is the generic "resource not found" error. Repositories and
	// services in any domain return it when a lookup by id yields nothing; the
	// HTTP layer maps it to 404. It is deliberately NOT user-specific so a
	// missing invoice/notification/role does not report "user not found".
	ErrNotFound     = New("NOT_FOUND", "resource not found", 404)
	ErrUnauthorized = New("UNAUTHORIZED", "authentication required", 401)
	ErrForbidden    = New("FORBIDDEN", "insufficient permissions", 403)
)

// User service codes (arch-user.md §3.3). These are user-domain business
// exceptions; they could move to internal/user/service alongside the domain
// they belong to (kept here for now to limit churn).
var (
	ErrEmailTaken         = New("USER_EMAIL_TAKEN", "email already registered", 409)
	ErrUsernameTaken      = New("USER_USERNAME_TAKEN", "username already taken", 409)
	ErrInvalidUsername    = New("USER_INVALID_USERNAME", "invalid username", 400)
	ErrInvalidEmail       = New("USER_INVALID_EMAIL", "invalid email", 400)
	ErrInvalidPreferences = New("USER_INVALID_PREFERENCES", "invalid preferences", 400)
	ErrSuspended          = New("USER_SUSPENDED", "user account is suspended", 403)
)
