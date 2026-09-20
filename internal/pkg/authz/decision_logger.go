package authz

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// slogKey* are the structured-log keys a decision is rendered under. They are
// a stable, greppable contract for operators (see docs/DEPLOYMENT_HARDENING.md
// §1) — renaming one is a breaking change to whoever built a saved search on
// it.
const (
	slogKeyMode          = "authz.mode"
	slogKeyPolicyVersion = "authz.policy_version"
	slogKeyPolicyLabel   = "authz.policy_label"
	slogKeyAction        = "authz.action"
	slogKeyResourceType  = "authz.resource_type"
	slogKeyResourceID    = "authz.resource_id"
	slogKeyTenantID      = "authz.tenant_id"
	slogKeyUserID        = "authz.user_id"
	slogKeyRoles         = "authz.roles"
	slogKeyTenantRole    = "authz.tenant_role"
	slogKeyAllowed       = "authz.allowed"
	slogKeyReason        = "authz.reason"
	slogKeyDurationMS    = "authz.duration_ms"
	slogKeyError         = "authz.error"
	slogKeyRequestID     = "authz.request_id"
)

const decisionLogMessage = "authz decision"

// SlogDecisionLogger renders a Decision as one structured slog record. It
// logs ONLY the fields already present on Decision — which is itself built
// from Input/OPAInput (see decision.go) — so it can never emit a header, a
// token, or anything else the authorizer was not already given.
//
// Level follows the outcome: an evaluation error (Decision.Err != nil) is
// ERROR, a refusal is WARN, an allow is DEBUG — so `AUTHZ_DECISION_LOG=deny`
// (which never constructs an allow Decision at all, see DecoratedAuthorizer)
// plus a WARN-or-above log level shows exactly the lines worth waking up for.
type SlogDecisionLogger struct {
	logger *slog.Logger
}

// NewSlogDecisionLogger wraps logger. A nil logger falls back to
// slog.Default() rather than panicking — a decision logger that cannot be
// constructed without wiring a *slog.Logger through every call site would be
// exactly the friction AUTHZ_DECISION_LOG=off exists to let an operator skip.
func NewSlogDecisionLogger(logger *slog.Logger) *SlogDecisionLogger {
	if logger == nil {
		logger = slog.Default()
	}
	return &SlogDecisionLogger{logger: logger}
}

func (l *SlogDecisionLogger) Log(ctx context.Context, d Decision) {
	level := slog.LevelDebug
	switch {
	case d.Err != nil:
		level = slog.LevelError
	case !d.Allowed:
		level = slog.LevelWarn
	}

	errStr := ""
	if d.Err != nil {
		errStr = d.Err.Error()
	}

	l.logger.LogAttrs(ctx, level, decisionLogMessage,
		slog.String(slogKeyMode, d.Mode),
		slog.String(slogKeyPolicyVersion, d.PolicyVersion),
		slog.String(slogKeyPolicyLabel, d.PolicyLabel),
		slog.String(slogKeyAction, d.Input.Action),
		slog.String(slogKeyResourceType, d.Input.Resource.Type),
		slog.String(slogKeyResourceID, d.Input.Resource.ID),
		slog.String(slogKeyTenantID, d.Input.Context.TenantID),
		slog.String(slogKeyUserID, d.Input.Subject.UserID),
		slog.Any(slogKeyRoles, d.Input.Subject.Roles),
		slog.String(slogKeyTenantRole, d.Input.Subject.TenantRole),
		slog.Bool(slogKeyAllowed, d.Allowed),
		slog.String(slogKeyReason, d.Reason),
		slog.Int64(slogKeyDurationMS, d.Duration.Milliseconds()),
		slog.String(slogKeyError, errStr),
		slog.String(slogKeyRequestID, d.RequestID),
	)
}

// AllowSampleLimit is the default per-second cap NewAllowSampler is
// constructed with (see cmd/server's provideAuthorizer) for ALLOW records
// under AUTHZ_DECISION_LOG=all. 100/s is generous for an operator reading
// logs by eye and cheap for any log pipeline behind it, while still keeping
// a hot path from writing thousands of DEBUG lines per second per process —
// see the "Sampling guard" note in the ADR-009 amendment.
const AllowSampleLimit = 100

// AllowSampler wraps a DecisionLogger and caps how many ALLOW decisions (no
// error) it forwards per process per second. Denies and evaluation errors
// are never sampled — they always reach inner unchanged, because those are
// exactly the records AUTHZ_DECISION_LOG=deny exists to never miss.
//
// It is a fixed-window counter (same shape as internal/pkg/ratelimit.
// IPLimiter), not a true token bucket: simplicity matters more than burst
// smoothness for a log volume guard. Dropped allow records are counted and
// reported lazily — the next call after a window rolls over emits one
// "dropped N allow records" WARN line for the window that just closed, via
// the same *slog.Logger a SlogDecisionLogger was given (no background
// goroutine to manage or leak).
type AllowSampler struct {
	inner  DecisionLogger
	logger *slog.Logger // for the periodic "dropped" line; nil = silent
	limit  int

	mu        sync.Mutex
	windowEnd time.Time
	count     int
	dropped   int64
}

// NewAllowSampler returns a DecisionLogger that forwards denies/errors
// unconditionally and caps allows to limitPerSecond. limitPerSecond <= 0
// disables sampling (every allow is forwarded) — used for
// AUTHZ_DECISION_LOG=deny, where no allow Decision is ever built anyway, and
// as an explicit escape hatch if an operator ever needs unsampled `all`.
func NewAllowSampler(inner DecisionLogger, logger *slog.Logger, limitPerSecond int) *AllowSampler {
	return &AllowSampler{inner: inner, logger: logger, limit: limitPerSecond}
}

func (s *AllowSampler) Log(ctx context.Context, d Decision) {
	if s.limit <= 0 || d.Err != nil || !d.Allowed {
		s.inner.Log(ctx, d)
		return
	}

	now := time.Now()
	var droppedToReport int64
	var forward bool

	s.mu.Lock()
	if s.windowEnd.IsZero() || now.After(s.windowEnd) {
		droppedToReport = s.dropped
		s.dropped = 0
		s.count = 0
		s.windowEnd = now.Add(time.Second)
	}
	if s.count < s.limit {
		s.count++
		forward = true
	} else {
		s.dropped++
	}
	s.mu.Unlock()

	if droppedToReport > 0 && s.logger != nil {
		s.logger.LogAttrs(ctx, slog.LevelWarn, "authz decision log: dropped allow records",
			slog.Int64("authz.dropped_allow_records", droppedToReport))
	}
	if forward {
		s.inner.Log(ctx, d)
	}
}
