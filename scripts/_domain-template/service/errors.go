package service

import apperrors "__MODULE__/internal/pkg/errors"

// Business exceptions for the __domain__ domain.
//
// Declare domain-specific, user-facing error conditions here as package-level
// AppError values — this is the extension point for this domain's business
// rules. Convention:
//   - code  : "__DOMAIN___<REASON>" (screaming snake, domain-namespaced)
//   - status: reflects the semantics (409 conflict, 422 unprocessable, 403 ...)
//
// Return them directly from service methods; response.Error maps them to the
// API shape, and callers can match them with apperrors.Is(err, Err__Domain__X).
//
// Example usage inside a service method:
//
//	if !allowed(cur, next) {
//	    return __Domain__DTO{}, Err__Domain__InvalidStatusTransition
//	}
var (
	// Err__Domain__InvalidStatusTransition is returned when a status change is not allowed.
	Err__Domain__InvalidStatusTransition = apperrors.New(
		"__DOMAIN___INVALID_STATUS_TRANSITION", "invalid status transition", 409)
)
