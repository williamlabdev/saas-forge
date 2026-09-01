package graph

import (
	"context"

	"github.com/williamlabdev/saas-forge/apps/bff/internal/domainapi"
)

// listTickets resolves the `tickets` query: it authenticates from context,
// applies limit/offset defaults, calls the domain REST API and maps the result.
// Mirrors listPlatformApps; the resolver stub in ticket.resolvers.go delegates here.
func listTickets(
	ctx context.Context,
	client *domainapi.Client,
	limit *int,
	offset *int,
) (*TicketConnection, error) {
	bearer, err := bearerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	lim := 20
	if limit != nil && *limit > 0 {
		lim = *limit
	}
	off := 0
	if offset != nil && *offset >= 0 {
		off = *offset
	}
	raw, err := client.ListTickets(ctx, bearer, lim, off)
	if err != nil {
		return nil, mapAPIError(err)
	}
	return mapTicketConnection(raw), nil
}
