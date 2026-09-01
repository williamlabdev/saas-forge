package graph

func mapPlatformBillingSummary(m map[string]any) *PlatformBillingSummary {
	return &PlatformBillingSummary{
		PlanName:      str(m["plan_name"]),
		RenewsAt:      str(m["renews_at"]),
		PaymentStatus: str(m["payment_status"]),
		AppsUsed:      intFromAny(m["apps_used"]),
		AppsQuota:     intFromAny(m["apps_quota"]),
		SeatsUsed:     intFromAny(m["seats_used"]),
		SeatsQuota:    intFromAny(m["seats_quota"]),
	}
}

func mapPlatformInvoices(raw map[string]any) []*PlatformInvoice {
	itemsRaw, ok := raw["items"].([]any)
	if !ok {
		return nil
	}
	out := make([]*PlatformInvoice, 0, len(itemsRaw))
	for _, it := range itemsRaw {
		row, ok := it.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, &PlatformInvoice{
			ID:       str(row["id"]),
			IssuedAt: str(row["issued_at"]),
			Amount:   str(row["amount"]),
			Status:   str(row["status"]),
		})
	}
	return out
}

func mapPlatformStaff(raw map[string]any) []*PlatformStaffMember {
	itemsRaw, ok := raw["items"].([]any)
	if !ok {
		return nil
	}
	out := make([]*PlatformStaffMember, 0, len(itemsRaw))
	for _, it := range itemsRaw {
		row, ok := it.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, &PlatformStaffMember{
			ID:    str(row["id"]),
			Name:  str(row["name"]),
			Email: str(row["email"]),
			Role:  str(row["role"]),
		})
	}
	return out
}

func mapPlatformAlerts(raw map[string]any) []*PlatformAlert {
	itemsRaw, ok := raw["items"].([]any)
	if !ok {
		return nil
	}
	out := make([]*PlatformAlert, 0, len(itemsRaw))
	for _, it := range itemsRaw {
		row, ok := it.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, &PlatformAlert{
			ID:        str(row["id"]),
			Title:     str(row["title"]),
			AlertType: str(row["alert_type"]),
			Read:      row["read"] == true,
			CreatedAt: str(row["created_at"]),
		})
	}
	return out
}

func mapPlatformReportsSummary(m map[string]any) *PlatformReportsSummary {
	return &PlatformReportsSummary{
		Mrr:        str(m["mrr"]),
		ActiveApps: intFromAny(m["active_apps"]),
		PausedApps: intFromAny(m["paused_apps"]),
	}
}
