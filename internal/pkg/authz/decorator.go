package authz

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// DecisionLogPolicy is a parsed AUTHZ_DECISION_LOG value.
type DecisionLogPolicy string

const (
	// DecisionLogOff logs nothing. DecoratedAuthorizer skips building a
	// Decision at all in this mode, not just skips logging one.
	DecisionLogOff DecisionLogPolicy = "off"
	// DecisionLogDeny logs refusals and evaluation errors only. This is the
	// default: it is the useful-by-default setting (an operator greps for
	// why something was refused) without paying for an allow record on
	// every request.
	DecisionLogDeny DecisionLogPolicy = "deny"
	// DecisionLogAll additionally logs every allow, at DEBUG, subject to
	// AllowSampler.
	DecisionLogAll DecisionLogPolicy = "all"
)

// ParseDecisionLogPolicy validates an AUTHZ_DECISION_LOG value. Empty means
// unset, which defaults to "deny" — the same "declared default, not a silent
// one" shape LoadRuntimeFromEnv uses for every other knob (see
// config.Runtime). Anything else unrecognised is a startup error: unlike
// AUTHZ_MODE's default-to-allow-all fallback (a pre-existing, narrower gap —
// see ValidateRuntime's own AUTHZ_MODE=="allow" check), a typoed
// AUTHZ_DECISION_LOG has no safe silent interpretation between "log nothing"
// and "log everything", so it must fail loudly rather than guess.
func ParseDecisionLogPolicy(raw string) (DecisionLogPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "deny":
		return DecisionLogDeny, nil
	case "off":
		return DecisionLogOff, nil
	case "all":
		return DecisionLogAll, nil
	default:
		return "", fmt.Errorf("config: AUTHZ_DECISION_LOG=%q is invalid — must be off, deny, or all", raw)
	}
}

// DecoratedAuthorizer wraps another Authorizer and logs every decision
// through a DecisionLogger. It is a decorator rather than a hook inside each
// of OPAAuthorizer/RBACAuthorizer/AllowAllAuthorizer so that record-building
// (subject → OPAInput, error → Decision) exists exactly once: it reads the
// same authn.Subject those three already require off ctx, so it needs no
// cooperation from the wrapped Authorizer beyond the Authorizer interface
// itself.
//
// One consequence of that: the Input.Subject.Roles it logs for OPA mode is
// the subject's own roles, not the IAM-facts-augmented set OPAAuthorizer
// evaluates against (see Decision.Input's doc comment) — the decorator has
// no access to the facts loader and re-querying it purely to log would add a
// second failure mode to every decision for a cosmetic difference.
type DecoratedAuthorizer struct {
	inner  Authorizer
	mode   string
	logger DecisionLogger
	policy DecisionLogPolicy
}

// NewDecoratedAuthorizer wraps inner. mode is one of ModeOPA/ModeRBAC/
// ModeAllow, recorded on every Decision. A nil logger is treated as
// NopDecisionLogger.
func NewDecoratedAuthorizer(inner Authorizer, mode string, logger DecisionLogger, policy DecisionLogPolicy) *DecoratedAuthorizer {
	if logger == nil {
		logger = NopDecisionLogger{}
	}
	return &DecoratedAuthorizer{inner: inner, mode: mode, logger: logger, policy: policy}
}

func (d *DecoratedAuthorizer) Allow(ctx context.Context, in Input) error {
	start := time.Now()
	err := d.inner.Allow(ctx, in)

	if d.policy == DecisionLogOff {
		return err
	}
	if err == nil && d.policy != DecisionLogAll {
		// Allowed, and this policy does not log allows: skip building a
		// Decision at all — not merely skip logging one — so the common case
		// (deny-only logging, everything succeeds) costs nothing beyond the
		// policy comparison above.
		return nil
	}

	dur := time.Since(start)
	dec := Decision{
		At:            start,
		Mode:          d.mode,
		PolicyVersion: PolicyVersion(),
		PolicyLabel:   PolicyLabel(),
		Duration:      dur,
		RequestID:     middleware.GetReqID(ctx),
	}
	if sub, ok := authn.SubjectFromContext(ctx); ok {
		dec.Input = BuildOPAInput(sub, in, nil)
	} else {
		// No authenticated subject (should not happen in practice — every
		// authorizer refuses first thing without one) — still record what
		// was asked for.
		dec.Input = OPAInput{Action: in.Action, Resource: in.Resource}
	}

	switch {
	case err == nil:
		dec.Allowed = true
	case isEvalError(err):
		dec.Err = err
	default:
		dec.Reason = reasonFromError(err)
	}

	d.logger.Log(ctx, dec)
	return err
}

// isEvalError reports whether err is an evaluation failure (OPA prepare/eval
// error, IAM facts lookup error, ...) rather than a deny. Every deny in this
// package is an *apperrors.AppError (ErrUnauthorized/ErrForbidden or a
// business sentinel); anything else reaching here is infrastructure failing,
// not the policy speaking.
func isEvalError(err error) bool {
	_, ok := apperrors.As(err)
	return !ok
}

// reasonFromError extracts the AppError code as the best-effort Reason for a
// deny (see Decision.Reason's doc comment for why this is the finest grain
// available without instrumenting each authorizer).
func reasonFromError(err error) string {
	if ae, ok := apperrors.As(err); ok {
		return ae.Code
	}
	return ""
}

// WrapWithDecisionLog parses rawPolicy (an AUTHZ_DECISION_LOG value) and, if
// valid, wraps inner in a DecoratedAuthorizer configured to log through it.
// mode is one of ModeOPA/ModeRBAC/ModeAllow, recorded on every Decision.
//
// This is the single place that turns "AUTHZ_MODE-selected engine" +
// "AUTHZ_DECISION_LOG value" into a logged Authorizer — both composition
// roots (cmd/server/providers.go's provideAuthorizer and
// internal/platform/app.go's provideAuthorizer) call it after their own,
// separately-implemented AUTHZ_MODE switch, so the decision-log wrapping
// itself cannot drift between them even though engine selection lives twice.
func WrapWithDecisionLog(inner Authorizer, mode string, rawPolicy string) (Authorizer, error) {
	policy, err := ParseDecisionLogPolicy(rawPolicy)
	if err != nil {
		return nil, err
	}
	return NewDecoratedAuthorizer(inner, mode, decisionLoggerFor(policy), policy), nil
}

// decisionLoggerFor builds the DecisionLogger for policy. "off" gets
// NopDecisionLogger (DecoratedAuthorizer would skip building a Decision for
// it anyway; this is belt-and-braces). "all" additionally wraps the slog
// sink in an AllowSampler, since that is the one setting where a hot path
// could otherwise write an ALLOW line per request.
func decisionLoggerFor(policy DecisionLogPolicy) DecisionLogger {
	if policy == DecisionLogOff {
		return NopDecisionLogger{}
	}
	slogger := slog.Default()
	var logger DecisionLogger = NewSlogDecisionLogger(slogger)
	if policy == DecisionLogAll {
		logger = NewAllowSampler(logger, slogger, AllowSampleLimit)
	}
	return logger
}
