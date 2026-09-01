package domainapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// ListTickets fetches a page of tickets from the domain REST API.
// It returns the raw list envelope ({items, total, limit, offset}); the BFF
// graph layer maps it into a TicketConnection.
func (c *Client) ListTickets(ctx context.Context, bearer string, limit, offset int) (map[string]any, error) {
	params := url.Values{}
	params.Set("limit", fmt.Sprintf("%d", limit))
	params.Set("offset", fmt.Sprintf("%d", offset))
	path := "/api/v1/tickets?" + params.Encode()
	return c.do(ctx, http.MethodGet, path, bearer, nil, http.StatusOK)
}

// GetTicket fetches a single ticket by id. Not yet exposed in the GraphQL schema
// (the slice is list-only for now) but kept here as the read-by-id reference.
func (c *Client) GetTicket(ctx context.Context, bearer, id string) (map[string]any, error) {
	return c.do(ctx, http.MethodGet, "/api/v1/tickets/"+id, bearer, nil, http.StatusOK)
}
