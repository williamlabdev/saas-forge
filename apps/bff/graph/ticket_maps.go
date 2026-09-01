package graph

// mapTicket converts a domain REST ticket row into the GraphQL Ticket model.
// Field names are the REST JSON keys (snake_case); GraphQL camelCase is applied
// by the generated model tags.
func mapTicket(m map[string]any) *Ticket {
	return &Ticket{
		ID:        str(m["id"]),
		Name:      str(m["name"]),
		Status:    str(m["status"]),
		CreatedAt: str(m["created_at"]),
		UpdatedAt: str(m["updated_at"]),
	}
}

// mapTicketConnection maps the REST list envelope ({items,total,limit,offset})
// into a TicketConnection.
func mapTicketConnection(m map[string]any) *TicketConnection {
	itemsRaw, ok := m["items"].([]any)
	if !ok {
		return &TicketConnection{}
	}
	items := make([]*Ticket, 0, len(itemsRaw))
	for _, it := range itemsRaw {
		row, ok := it.(map[string]any)
		if !ok {
			continue
		}
		items = append(items, mapTicket(row))
	}
	return &TicketConnection{
		Items:  items,
		Total:  intFromAny(m["total"]),
		Limit:  intFromAny(m["limit"]),
		Offset: intFromAny(m["offset"]),
	}
}
