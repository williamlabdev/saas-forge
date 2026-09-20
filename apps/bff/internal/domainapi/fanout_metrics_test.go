package domainapi

import (
	"strings"
	"testing"
	"time"
)

// T002: aggregation + render format. Red until T003 lands.
func TestFanoutMetrics_AggregatesOneRequest(t *testing.T) {
	resetBFFMetrics()

	s := &FanoutStats{}
	s.record(300 * time.Millisecond)
	s.record(200 * time.Millisecond)
	RecordGraphQLRequest(s)

	out := RenderMetrics()
	for _, want := range []string{
		"bff_graphql_requests_total 1\n",
		"bff_domain_fanout_calls_total 2\n",
		"bff_domain_seconds_total 0.5\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing %q, got:\n%s", want, out)
		}
	}
}

func TestFanoutMetrics_ZeroTrafficRendersZeros(t *testing.T) {
	resetBFFMetrics()

	out := RenderMetrics()
	for _, want := range []string{
		"bff_graphql_requests_total 0\n",
		"bff_domain_fanout_calls_total 0\n",
		"bff_domain_seconds_total 0\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing %q, got:\n%s", want, out)
		}
	}
}

func TestFanoutMetrics_NilStatsCountsRequestOnly(t *testing.T) {
	resetBFFMetrics()

	RecordGraphQLRequest(nil)

	out := RenderMetrics()
	if !strings.Contains(out, "bff_graphql_requests_total 1\n") {
		t.Fatalf("request not counted, got:\n%s", out)
	}
	if !strings.Contains(out, "bff_domain_fanout_calls_total 0\n") {
		t.Fatalf("nil stats must not add calls, got:\n%s", out)
	}
}
