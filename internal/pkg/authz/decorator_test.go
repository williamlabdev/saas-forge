package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

var _ Authorizer = (*DecoratedAuthorizer)(nil)

// countingAuthorizer counts calls and always returns err.
type countingAuthorizer struct {
	calls int
	err   error
}

func (c *countingAuthorizer) Allow(context.Context, Input) error {
	c.calls++
	return c.err
}

func TestParseDecisionLogPolicy(t *testing.T) {
	cases := []struct {
		raw  string
		want DecisionLogPolicy
		ok   bool
	}{
		{"", DecisionLogDeny, true},
		{"deny", DecisionLogDeny, true},
		{"DENY", DecisionLogDeny, true},
		{" deny ", DecisionLogDeny, true},
		{"off", DecisionLogOff, true},
		{"all", DecisionLogAll, true},
		{"ALL", DecisionLogAll, true},
		{"bogus", "", false},
		{"al", "", false},
	}
	for _, tc := range cases {
		t.Run("raw="+tc.raw, func(t *testing.T) {
			got, err := ParseDecisionLogPolicy(tc.raw)
			if tc.ok {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
			} else {
				require.Error(t, err, "unknown AUTHZ_DECISION_LOG values must fail, not silently pick a mode")
				assert.Contains(t, err.Error(), "AUTHZ_DECISION_LOG")
			}
		})
	}
}

func TestDecoratedAuthorizer_CallsInnerExactlyOnce(t *testing.T) {
	inner := &countingAuthorizer{}
	logger := newSpyLogger()
	d := NewDecoratedAuthorizer(inner, ModeRBAC, logger, DecisionLogAll)

	ctx := authn.WithSubject(context.Background(), authn.Subject{UserID: uuid.New()})
	err := d.Allow(ctx, Input{Action: ActionUserRead, Resource: Resource{Type: "user", ID: uuid.NewString()}})

	require.NoError(t, err)
	assert.Equal(t, 1, inner.calls)
}

func TestDecoratedAuthorizer_ReturnsInnerErrorUnchanged(t *testing.T) {
	inner := &countingAuthorizer{err: apperrors.ErrForbidden}
	logger := newSpyLogger()
	d := NewDecoratedAuthorizer(inner, ModeRBAC, logger, DecisionLogDeny)

	ctx := authn.WithSubject(context.Background(), authn.Subject{UserID: uuid.New()})
	err := d.Allow(ctx, Input{Action: ActionUserRead, Resource: Resource{Type: "user", ID: uuid.NewString()}})

	assert.ErrorIs(t, err, apperrors.ErrForbidden)
}

func TestDecoratedAuthorizer_PolicyOff_NeverLogs(t *testing.T) {
	logger := newSpyLogger()
	ctx := authn.WithSubject(context.Background(), authn.Subject{UserID: uuid.New()})

	allow := NewDecoratedAuthorizer(&countingAuthorizer{}, ModeRBAC, logger, DecisionLogOff)
	deny := NewDecoratedAuthorizer(&countingAuthorizer{err: apperrors.ErrForbidden}, ModeRBAC, logger, DecisionLogOff)
	evalErr := NewDecoratedAuthorizer(&countingAuthorizer{err: errors.New("boom")}, ModeRBAC, logger, DecisionLogOff)

	require.NoError(t, allow.Allow(ctx, Input{Action: ActionUserRead, Resource: Resource{Type: "user", ID: uuid.NewString()}}))
	_ = deny.Allow(ctx, Input{Action: ActionUserRead, Resource: Resource{Type: "user", ID: uuid.NewString()}})
	_ = evalErr.Allow(ctx, Input{Action: ActionUserRead, Resource: Resource{Type: "user", ID: uuid.NewString()}})

	assert.Empty(t, logger.logs, "off must log nothing at all — not deny, not allow, not eval error")
}

func TestDecoratedAuthorizer_PolicyDeny_LogsOnlyDenyAndError(t *testing.T) {
	logger := newSpyLogger()
	ctx := authn.WithSubject(context.Background(), authn.Subject{UserID: uuid.New()})

	allow := NewDecoratedAuthorizer(&countingAuthorizer{}, ModeRBAC, logger, DecisionLogDeny)
	require.NoError(t, allow.Allow(ctx, Input{Action: ActionUserRead, Resource: Resource{Type: "user", ID: uuid.NewString()}}))
	assert.Empty(t, logger.logs, "an allow must not be logged under the deny policy")

	deny := NewDecoratedAuthorizer(&countingAuthorizer{err: apperrors.ErrForbidden}, ModeRBAC, logger, DecisionLogDeny)
	err := deny.Allow(ctx, Input{Action: ActionUserRead, Resource: Resource{Type: "user", ID: uuid.NewString()}})
	require.Error(t, err)
	require.Len(t, logger.logs, 1)
	assert.False(t, logger.logs[0].Allowed)
	assert.Nil(t, logger.logs[0].Err, "a deny is Allowed=false, Err=nil")
	assert.Equal(t, "FORBIDDEN", logger.logs[0].Reason)

	evalErr := NewDecoratedAuthorizer(&countingAuthorizer{err: errors.New("authz: OPA eval: boom")}, ModeOPA, logger, DecisionLogDeny)
	err = evalErr.Allow(ctx, Input{Action: ActionUserRead, Resource: Resource{Type: "user", ID: uuid.NewString()}})
	require.Error(t, err)
	require.Len(t, logger.logs, 2)
	assert.False(t, logger.logs[1].Allowed)
	assert.Error(t, logger.logs[1].Err, "a non-AppError is an evaluation error, distinct from a deny")
}

func TestDecoratedAuthorizer_PolicyAll_LogsAllowsToo(t *testing.T) {
	logger := newSpyLogger()
	ctx := authn.WithSubject(context.Background(), authn.Subject{UserID: uuid.New()})

	allow := NewDecoratedAuthorizer(&countingAuthorizer{}, ModeRBAC, logger, DecisionLogAll)
	require.NoError(t, allow.Allow(ctx, Input{Action: ActionUserRead, Resource: Resource{Type: "user", ID: uuid.NewString()}}))
	require.Len(t, logger.logs, 1)
	assert.True(t, logger.logs[0].Allowed)
	assert.Equal(t, ModeRBAC, logger.logs[0].Mode)
}

func TestDecoratedAuthorizer_PopulatesPolicyVersionAndRequestID(t *testing.T) {
	logger := newSpyLogger()
	ctx := authn.WithSubject(context.Background(), authn.Subject{UserID: uuid.New()})
	ctx = context.WithValue(ctx, middleware.RequestIDKey, "req-42")

	d := NewDecoratedAuthorizer(&countingAuthorizer{err: apperrors.ErrForbidden}, ModeOPA, logger, DecisionLogDeny)
	_ = d.Allow(ctx, Input{Action: ActionUserRead, Resource: Resource{Type: "user", ID: uuid.NewString()}})

	require.Len(t, logger.logs, 1)
	assert.Equal(t, PolicyVersion(), logger.logs[0].PolicyVersion)
	assert.Equal(t, PolicyLabel(), logger.logs[0].PolicyLabel)
	assert.Equal(t, "req-42", logger.logs[0].RequestID)
}

func TestDecoratedAuthorizer_NilLoggerIsSafe(t *testing.T) {
	d := NewDecoratedAuthorizer(&countingAuthorizer{err: apperrors.ErrForbidden}, ModeRBAC, nil, DecisionLogDeny)
	ctx := authn.WithSubject(context.Background(), authn.Subject{UserID: uuid.New()})
	assert.NotPanics(t, func() {
		_ = d.Allow(ctx, Input{Action: ActionUserRead, Resource: Resource{Type: "user", ID: uuid.NewString()}})
	})
}
