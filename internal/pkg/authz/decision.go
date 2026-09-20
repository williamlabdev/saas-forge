package authz

import (
	"context"
	"time"
)

// Mode names a decorated Authorizer's underlying engine. Stable strings —
// they are logged, so renaming one changes what an operator greps for.
const (
	ModeOPA   = "opa"
	ModeRBAC  = "rbac"
	ModeAllow = "allow"
)

// Decision is one authorization call, normalised across all three engines so
// a single logger and a single test suite cover them. It carries exactly the
// fields already present in Input/OPAInput plus the outcome — nothing pulled
// from the request beyond what the authorizer itself was given, so it can
// never leak a header or a token (see DecisionLogger).
type Decision struct {
	At time.Time
	// Mode is which engine decided this (ModeOPA/ModeRBAC/ModeAllow).
	Mode string
	// PolicyVersion/PolicyLabel are always populated (all three modes embed
	// the same policy source, even when RBAC or Allow-All do not consult it)
	// so a decision from any mode can be correlated to the rego revision that
	// was live at the time.
	PolicyVersion string
	PolicyLabel   string
	// Input is the same normalised document OPA evaluates (OPASubject +
	// action + resource + context), built from the authenticated subject and
	// the caller's Input regardless of which engine actually ran. RBAC has no
	// document of its own; reusing OPAInput's shape means one struct, one set
	// of log keys, and one test suite for all three modes.
	//
	// Its Subject.Roles is the subject's own roles (JWT claims / dev headers)
	// only — it does NOT include IAM facts-loader roles that OPAAuthorizer
	// additionally merges in before evaluation (RolesForUser). Re-fetching
	// those here, purely to log them, would mean a second facts-store round
	// trip (and a second place it could fail) for every decision; the
	// evaluated decision itself is unaffected either way.
	Input OPAInput
	// Allowed is the outcome. False on any refusal AND on an evaluation
	// error — Err is what tells the two apart.
	Allowed bool
	// Reason is best-effort and may be empty. Today it is the AppError code
	// that produced a refusal (e.g. "FORBIDDEN", "UNAUTHORIZED") — the RBAC
	// and Allow-All authorizers return only an error, not a named branch, so
	// that is the finest grain available without instrumenting each
	// authorizer internally. It stays empty for Err (evaluation failures).
	Reason string
	// Duration is how long Allow() took end to end (roles lookup + eval).
	Duration time.Duration
	// Err is the evaluation failure (OPA prepare/eval error, IAM facts
	// lookup error, ...) — distinct from a deny. A deny is Allowed=false,
	// Err=nil; an evaluation error is Allowed=false, Err!=nil.
	Err error
	// RequestID correlates a decision with the request that triggered it, via
	// go-chi's middleware.RequestID (internal/platform/router.go). Empty
	// when the call has no request in flight (e.g. a background worker) or
	// the request-id middleware did not run.
	RequestID string
}

// DecisionLogger is where a Decision goes once it is made. Implementations
// must never read anything off ctx or Decision beyond what Decision already
// carries — in particular, never headers or tokens; Decision.Input is built
// from the same struct OPA evaluates and contains none.
type DecisionLogger interface {
	Log(ctx context.Context, d Decision)
}

// NopDecisionLogger discards every decision. Used for AUTHZ_DECISION_LOG=off.
type NopDecisionLogger struct{}

func (NopDecisionLogger) Log(context.Context, Decision) {}
