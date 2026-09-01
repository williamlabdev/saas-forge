package service

import apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"

// Business exceptions for the ticket domain.
//
// Declare domain-specific, user-facing error conditions here as package-level
// AppError values — this is the extension point for this domain's business
// rules. Convention:
//   - code  : "TICKET_<REASON>" (screaming snake, domain-namespaced)
//   - status: reflects the semantics (409 conflict, 422 unprocessable, 403 ...)
//
// Return them directly from service methods; response.Error maps them to the
// API shape, and callers can match them with apperrors.Is(err, ErrTicketX).
//
// Example usage inside a service method:
//
//	if !allowed(cur, next) {
//	    return TicketDTO{}, ErrTicketInvalidStatusTransition
//	}
var (
	// ErrTicketInvalidStatusTransition is returned when a status change is not allowed.
	ErrTicketInvalidStatusTransition = apperrors.New(
		"TICKET_INVALID_STATUS_TRANSITION", "invalid status transition", 409)
)
