package graph

import (
	"context"

	"github.com/williamlabdev/saas-forge/apps/bff/internal/domainapi"
)

func platformBillingSummary(ctx context.Context, client *domainapi.Client) (*PlatformBillingSummary, error) {
	bearer, err := bearerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := client.GetPlatformBillingSummary(ctx, bearer)
	if err != nil {
		return nil, mapAPIError(err)
	}
	return mapPlatformBillingSummary(raw), nil
}

func platformInvoices(ctx context.Context, client *domainapi.Client, limit *int) ([]*PlatformInvoice, error) {
	bearer, err := bearerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	lim := 20
	if limit != nil && *limit > 0 {
		lim = *limit
	}
	raw, err := client.ListPlatformInvoices(ctx, bearer, lim)
	if err != nil {
		return nil, mapAPIError(err)
	}
	return mapPlatformInvoices(raw), nil
}

func platformStaff(ctx context.Context, client *domainapi.Client) ([]*PlatformStaffMember, error) {
	bearer, err := bearerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := client.ListPlatformStaff(ctx, bearer)
	if err != nil {
		return nil, mapAPIError(err)
	}
	return mapPlatformStaff(raw), nil
}

func platformAlerts(ctx context.Context, client *domainapi.Client, limit *int) ([]*PlatformAlert, error) {
	bearer, err := bearerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	lim := 20
	if limit != nil && *limit > 0 {
		lim = *limit
	}
	raw, err := client.ListPlatformAlerts(ctx, bearer, lim)
	if err != nil {
		return nil, mapAPIError(err)
	}
	return mapPlatformAlerts(raw), nil
}

func platformReportsSummary(ctx context.Context, client *domainapi.Client) (*PlatformReportsSummary, error) {
	bearer, err := bearerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := client.GetPlatformReportsSummary(ctx, bearer)
	if err != nil {
		return nil, mapAPIError(err)
	}
	return mapPlatformReportsSummary(raw), nil
}
