package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Compile-time proof both implementations satisfy DecisionLogger.
var (
	_ DecisionLogger = (*SlogDecisionLogger)(nil)
	_ DecisionLogger = NopDecisionLogger{}
	_ DecisionLogger = (*AllowSampler)(nil)
)

func sampleDecision(allowed bool, reason string, err error) Decision {
	return Decision{
		At:            time.Unix(0, 0),
		Mode:          ModeOPA,
		PolicyVersion: "abc123def456abc123def456abc123def456abc123def456abc123def45678",
		PolicyLabel:   "2026-09-10",
		Input: OPAInput{
			Subject: OPASubject{UserID: "user-1", Roles: []string{"editor"}, TenantRole: "editor"},
			Action:  ActionContentUpdate,
			Resource: Resource{
				Type: "content_entry",
				ID:   "entry-1",
			},
			Context: OPAContext{TenantID: "tenant-1"},
		},
		Allowed:   allowed,
		Reason:    reason,
		Duration:  42 * time.Millisecond,
		Err:       err,
		RequestID: "req-1",
	}
}

func TestSlogDecisionLogger_EmitsExactKeys(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := NewSlogDecisionLogger(slog.New(handler))

	logger.Log(context.Background(), sampleDecision(false, "FORBIDDEN", nil))

	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))

	wantKeys := map[string]any{
		"authz.mode":           "opa",
		"authz.policy_version": "abc123def456abc123def456abc123def456abc123def456abc123def45678",
		"authz.policy_label":   "2026-09-10",
		"authz.action":         ActionContentUpdate,
		"authz.resource_type":  "content_entry",
		"authz.resource_id":    "entry-1",
		"authz.tenant_id":      "tenant-1",
		"authz.user_id":        "user-1",
		"authz.tenant_role":    "editor",
		"authz.allowed":        false,
		"authz.reason":         "FORBIDDEN",
		"authz.duration_ms":    float64(42),
		"authz.error":          "",
		"authz.request_id":     "req-1",
	}
	for k, want := range wantKeys {
		assert.Equal(t, want, rec[k], "key %q", k)
	}
	roles, ok := rec["authz.roles"].([]any)
	require.True(t, ok, "authz.roles must be an array")
	require.Len(t, roles, 1)
	assert.Equal(t, "editor", roles[0])
}

func TestSlogDecisionLogger_Levels(t *testing.T) {
	cases := []struct {
		name string
		dec  Decision
		want slog.Level
	}{
		{"deny is WARN", sampleDecision(false, "FORBIDDEN", nil), slog.LevelWarn},
		{"allow is DEBUG", sampleDecision(true, "", nil), slog.LevelDebug},
		{"eval error is ERROR", sampleDecision(false, "", errors.New("opa: boom")), slog.LevelError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
			logger := NewSlogDecisionLogger(slog.New(handler))
			logger.Log(context.Background(), tc.dec)

			var rec map[string]any
			require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
			assert.Equal(t, tc.want.String(), rec["level"])
		})
	}
}

func TestSlogDecisionLogger_ErrorFieldCarriesMessage(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := NewSlogDecisionLogger(slog.New(handler))
	logger.Log(context.Background(), sampleDecision(false, "", errors.New("authz: OPA eval: boom")))

	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
	assert.Equal(t, "authz: OPA eval: boom", rec["authz.error"])
}

func TestNopDecisionLogger_DoesNothing(t *testing.T) {
	// Nothing to assert beyond "does not panic" — Nop has no observable
	// effect by design (AUTHZ_DECISION_LOG=off).
	NopDecisionLogger{}.Log(context.Background(), sampleDecision(true, "", nil))
}

// --- AllowSampler ---

func newSpyLogger() *fakeLogger {
	return &fakeLogger{}
}

type fakeLogger struct {
	logs []Decision
}

func (f *fakeLogger) Log(_ context.Context, d Decision) {
	f.logs = append(f.logs, d)
}

func TestAllowSampler_ForwardsDeniesAndErrorsUnconditionally(t *testing.T) {
	inner := newSpyLogger()
	sampler := NewAllowSampler(inner, nil, 1) // limit=1, well below the volume below

	for range 10 {
		sampler.Log(context.Background(), sampleDecision(false, "FORBIDDEN", nil))
	}
	for range 10 {
		sampler.Log(context.Background(), sampleDecision(false, "", errors.New("eval error")))
	}
	assert.Len(t, inner.logs, 20, "denies and eval errors must never be dropped, regardless of the allow cap")
}

func TestAllowSampler_CapsAllowsPerWindow(t *testing.T) {
	inner := newSpyLogger()
	sampler := NewAllowSampler(inner, nil, 5)

	for range 50 {
		sampler.Log(context.Background(), sampleDecision(true, "", nil))
	}
	assert.Len(t, inner.logs, 5, "only the first 5 allows in the window should be forwarded")
}

func TestAllowSampler_ZeroLimitDisablesSampling(t *testing.T) {
	inner := newSpyLogger()
	sampler := NewAllowSampler(inner, nil, 0)

	for range 50 {
		sampler.Log(context.Background(), sampleDecision(true, "", nil))
	}
	assert.Len(t, inner.logs, 50, "limit<=0 must forward every allow, unsampled")
}

func TestAllowSampler_ReportsDroppedCountOnNextWindow(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	reportLogger := slog.New(handler)

	inner := newSpyLogger()
	sampler := NewAllowSampler(inner, reportLogger, 2)

	for range 5 {
		sampler.Log(context.Background(), sampleDecision(true, "", nil))
	}
	require.Len(t, inner.logs, 2, "2 allowed, 3 dropped in the first window")
	assert.Empty(t, buf.String(), "the drop report is lazy: nothing is emitted before the window rolls over")

	// Force the window to roll over and send one more allow — this is what
	// triggers the deferred "dropped N" report for the window that closed.
	sampler.windowEnd = time.Now().Add(-time.Millisecond)
	sampler.Log(context.Background(), sampleDecision(true, "", nil))

	require.NotEmpty(t, buf.String(), "expected a dropped-records report once the window rolled over")
	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
	assert.Equal(t, float64(3), rec["authz.dropped_allow_records"])
}
