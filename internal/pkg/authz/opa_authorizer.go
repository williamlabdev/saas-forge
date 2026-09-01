package authz

import (
	"context"
	"fmt"

	"github.com/open-policy-agent/opa/v1/rego"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

const opaQuery = "data.authz.allow"

// OPAAuthorizer evaluates embedded Rego via the OPA SDK (Phase 1).
type OPAAuthorizer struct {
	query rego.PreparedEvalQuery
	facts RoleFactsLoader
}

// NewOPAAuthorizer compiles the embedded policy bundle at startup.
func NewOPAAuthorizer(facts RoleFactsLoader) (*OPAAuthorizer, error) {
	r := rego.New(
		rego.Query(opaQuery),
		rego.Module("authz.rego", embeddedPolicy),
	)
	pq, err := r.PrepareForEval(context.Background())
	if err != nil {
		return nil, fmt.Errorf("authz: prepare OPA: %w", err)
	}
	return &OPAAuthorizer{query: pq, facts: facts}, nil
}

func (o *OPAAuthorizer) Allow(ctx context.Context, in Input) error {
	sub, ok := authn.SubjectFromContext(ctx)
	if !ok {
		return apperrors.ErrUnauthorized
	}

	// Same gate as the RBAC authorizer, before the policy is consulted. It is
	// Go-side in both rather than a rego rule so that the two modes cannot
	// disagree about it, and so that a policy bundle change cannot widen it.
	if err := refuseUnlistedAgentAction(sub, in.Action); err != nil {
		return err
	}

	var extra []string
	if o.facts != nil {
		roles, err := o.facts.RolesForUser(ctx, sub.UserID)
		if err != nil {
			return fmt.Errorf("authz: load IAM facts: %w", err)
		}
		extra = roles
	}

	input := BuildOPAInput(sub, in, extra)
	rs, err := o.query.Eval(ctx, rego.EvalInput(input))
	if err != nil {
		return fmt.Errorf("authz: OPA eval: %w", err)
	}

	allowed, err := regoAllowed(rs)
	if err != nil {
		return err
	}
	if !allowed {
		return apperrors.ErrForbidden
	}
	return nil
}

func regoAllowed(rs rego.ResultSet) (bool, error) {
	if len(rs) == 0 || len(rs[0].Expressions) == 0 {
		return false, nil
	}
	v := rs[0].Expressions[0].Value
	switch t := v.(type) {
	case bool:
		return t, nil
	default:
		return false, fmt.Errorf("authz: unexpected OPA result type %T", v)
	}
}
