package graph

import (
	"context"

	"github.com/williamlabdev/saas-forge/apps/bff/internal/domainapi"
)

func listPlatformApps(
	ctx context.Context,
	client *domainapi.Client,
	query *string,
	status *string,
	limit *int,
	offset *int,
) (*PlatformAppConnection, error) {
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
	q := ""
	if query != nil {
		q = *query
	}
	st := ""
	if status != nil {
		st = *status
	}
	raw, err := client.ListPlatformApps(ctx, bearer, q, st, lim, off)
	if err != nil {
		return nil, mapAPIError(err)
	}
	return mapPlatformAppConnection(raw), nil
}

func createPlatformAppRecord(
	ctx context.Context,
	client *domainapi.Client,
	input CreatePlatformAppInput,
) (*PlatformApp, error) {
	bearer, err := bearerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"name":      input.Name,
		"tenant_id": input.TenantID,
	}
	if input.Owner != nil && *input.Owner != "" {
		body["owner"] = *input.Owner
	}
	row, err := client.CreatePlatformApp(ctx, bearer, body)
	if err != nil {
		return nil, mapAPIError(err)
	}
	return mapPlatformApp(row), nil
}

func updatePlatformAppStatusRecord(
	ctx context.Context,
	client *domainapi.Client,
	id string,
	status string,
) (*PlatformApp, error) {
	bearer, err := bearerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	row, err := client.UpdatePlatformAppStatus(ctx, bearer, id, status)
	if err != nil {
		return nil, mapAPIError(err)
	}
	return mapPlatformApp(row), nil
}

func listViewerNotifications(
	ctx context.Context,
	client *domainapi.Client,
	limit *int,
	unread *bool,
) ([]*Notification, error) {
	bearer, err := bearerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	n := 20
	if limit != nil && *limit > 0 {
		n = *limit
	}
	u := unread != nil && *unread
	rows, err := client.ListNotifications(ctx, bearer, n, u)
	if err != nil {
		return nil, mapAPIError(err)
	}
	out := make([]*Notification, len(rows))
	for i, row := range rows {
		out[i] = mapNotification(row)
	}
	return out, nil
}

func viewerUnreadNotificationCount(ctx context.Context, client *domainapi.Client) (int, error) {
	bearer, err := bearerFromContext(ctx)
	if err != nil {
		return 0, err
	}
	n, err := client.UnreadNotificationCount(ctx, bearer)
	if err != nil {
		return 0, mapAPIError(err)
	}
	return n, nil
}

func markViewerNotificationRead(ctx context.Context, client *domainapi.Client, id string) (*Notification, error) {
	bearer, err := bearerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	row, err := client.MarkNotificationRead(ctx, bearer, id)
	if err != nil {
		return nil, mapAPIError(err)
	}
	return mapNotification(row), nil
}

func markAllViewerNotificationsRead(ctx context.Context, client *domainapi.Client) (int, error) {
	bearer, err := bearerFromContext(ctx)
	if err != nil {
		return 0, err
	}
	n, err := client.MarkAllNotificationsRead(ctx, bearer)
	if err != nil {
		return 0, mapAPIError(err)
	}
	return n, nil
}

func viewerMe(ctx context.Context, client *domainapi.Client) (*User, error) {
	bearer, err := bearerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	row, err := client.GetMe(ctx, bearer)
	if err != nil {
		return nil, mapAPIError(err)
	}
	return mapUser(row), nil
}
