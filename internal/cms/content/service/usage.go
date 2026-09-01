package service

import (
	"context"
	"fmt"
)

// UsageWarning signals a tenant crossed a plan's soft threshold on a count
// dimension (TKT-R4b D3). It is advisory — the write still succeeds; the
// handler surfaces it as an X-Content-Usage-Warning header.
type UsageWarning struct {
	Resource string `json:"resource"`
	Used     int    `json:"used"`
	Limit    int    `json:"limit"`
}

// String renders the header value, e.g. "entries=812/1000".
func (w UsageWarning) String() string {
	return fmt.Sprintf("%s=%d/%d", w.Resource, w.Used, w.Limit)
}

// warningCollector gathers soft-threshold warnings raised deep in the service
// during one request. The handler installs it in the context and reads it back
// after the call, so the ContentService interface stays free of a transport
// concern (the header) — the warning is a per-request side output.
type warningCollector struct{ warnings []UsageWarning }

type collectorKey struct{}

// WithUsageWarnings returns a context carrying a fresh warning collector.
func WithUsageWarnings(ctx context.Context) context.Context {
	return context.WithValue(ctx, collectorKey{}, &warningCollector{})
}

// UsageWarningsFrom returns any warnings raised under a WithUsageWarnings ctx.
func UsageWarningsFrom(ctx context.Context) []UsageWarning {
	c, ok := ctx.Value(collectorKey{}).(*warningCollector)
	if !ok {
		return nil
	}
	return c.warnings
}

func addWarning(ctx context.Context, w UsageWarning) {
	if c, ok := ctx.Value(collectorKey{}).(*warningCollector); ok {
		c.warnings = append(c.warnings, w)
	}
}

// softWarn records a warning if the post-write count crossed the plan's soft
// threshold. limit==0 (unlimited) or softPct==0 never warns.
func softWarn(ctx context.Context, resource string, used, limit, softPct int) {
	if limit <= 0 || softPct <= 0 {
		return
	}
	if used*100 >= limit*softPct {
		addWarning(ctx, UsageWarning{Resource: resource, Used: used, Limit: limit})
	}
}

// UsageDTO is the GET /usage response: the tenant's plan, its limits, and the
// live counts (TKT-R4b D8; counts are real-time, same source as the hard check).
type UsageDTO struct {
	Plan             string         `json:"plan"`
	SoftThresholdPct int            `json:"soft_threshold_pct"`
	Types            UsageDimension `json:"types"`
	Entries          UsageDimension `json:"entries"`
	// DeliveryReadsToday is the public delivery read volume recorded for the
	// current UTC day. It has no plan limit — it is reported so a tenant can see
	// the traffic attributed to them; the actual protection is the rate limit at
	// the delivery edge. Approximate by construction (see DeliveryCounter).
	DeliveryReadsToday int64 `json:"delivery_reads_today"`
}

// UsageDimension reports used vs limit for one dimension. Limit is nil when the
// plan sets that dimension to unlimited (0).
type UsageDimension struct {
	Used  int  `json:"used"`
	Limit *int `json:"limit"`
}

func dimension(used, limit int) UsageDimension {
	d := UsageDimension{Used: used}
	if limit > 0 {
		l := limit
		d.Limit = &l
	}
	return d
}
