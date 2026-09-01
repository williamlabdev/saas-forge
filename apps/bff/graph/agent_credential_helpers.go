package graph

import (
	"context"

	"github.com/williamlabdev/saas-forge/apps/bff/internal/domainapi"
)

// Agent credentials over the BFF (ADR-015 乙案, ruled 2026-08-20).
//
// These three used to be the console's second direct connection to the domain
// API. They came back here because the ONE reason the content REST surface
// stays direct — a content schema that changes at runtime, which gqlgen's
// build-time codegen cannot model (ADR-007) — does not apply to them: a
// credential's shape is fixed, so GraphQL holds it fine.
//
// The mint path is the one worth reading twice: the caller's own token is not
// merely how the request authenticates, it is the INPUT that gets downgraded
// (ADR-013 補裁 O-3). The BFF already forwards it on every call, which is why
// this move cost three resolvers instead of an auth design.

func mapAgentCredential(m map[string]any) *AgentCredential {
	c := &AgentCredential{
		ID:           str(m["id"]),
		AgentID:      str(m["agent_id"]),
		PrincipalID:  str(m["principal_id"]),
		TenantRole:   str(m["tenant_role"]),
		AllowedTypes: strSlice(m["allowed_types"]),
		ExpiresAt:    str(m["expires_at"]),
		CreatedAt:    str(m["created_at"]),
		Active:       m["active"] == true,
	}
	// Absent and null must both read as "nobody revoked it". Passing str()'s
	// empty string through would turn that into a timestamp of "" — which the
	// console reads as a revocation, because it only asks whether the field is
	// set (credentialState).
	if r := str(m["revoked_at"]); r != "" {
		c.RevokedAt = &r
	}
	return c
}

func mapIssuedAgentCredential(m map[string]any) *IssuedAgentCredential {
	return &IssuedAgentCredential{
		ID:           str(m["id"]),
		Token:        str(m["token"]),
		AgentID:      str(m["agent_id"]),
		AllowedTypes: strSlice(m["allowed_types"]),
		ExpiresAt:    str(m["expires_at"]),
	}
}

// strSlice keeps a missing list and an empty list the same answer, because the
// registry renders both as "no types" and neither is an error here. The domain
// is where an empty allowed_types is refused, and it refuses it on the way IN.
func strSlice(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(raw))
	for _, it := range raw {
		out = append(out, str(it))
	}
	return out
}

func listAgentCredentials(ctx context.Context, client *domainapi.Client) ([]*AgentCredential, error) {
	bearer, err := bearerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := client.ListAgentCredentials(ctx, bearer)
	if err != nil {
		return nil, mapAPIError(err)
	}
	out := make([]*AgentCredential, 0, len(rows))
	for _, row := range rows {
		out = append(out, mapAgentCredential(row))
	}
	return out, nil
}

func issueAgentCredentialRecord(ctx context.Context, client *domainapi.Client, input IssueAgentCredentialInput) (*IssuedAgentCredential, error) {
	bearer, err := bearerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	// allowed_types and tenant_role are sent even when empty: the domain owns
	// both refusals and answers them with messages that name the field. Which
	// roles are legal depends on the caller's own role (補裁 S-1), so a check
	// here would be a second copy of a table that lives on the other side —
	// and the copy is what goes stale.
	row, err := client.IssueAgentCredential(ctx, bearer, map[string]any{
		"agent_id":      input.AgentID,
		"tenant_role":   input.TenantRole,
		"allowed_types": input.AllowedTypes,
	})
	if err != nil {
		return nil, mapAPIError(err)
	}
	return mapIssuedAgentCredential(row), nil
}

func revokeAgentCredentialRecord(ctx context.Context, client *domainapi.Client, id string) (bool, error) {
	bearer, err := bearerFromContext(ctx)
	if err != nil {
		return false, err
	}
	if err := client.RevokeAgentCredential(ctx, bearer, id); err != nil {
		return false, mapAPIError(err)
	}
	// true is the only value this can return: every other outcome left through
	// the error above. It is a receipt that the domain answered 204, not a
	// judgement made here.
	return true, nil
}
