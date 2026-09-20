package authz

import (
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
)

// OPAInput is the JSON document passed to Rego (stable contract for policies).
type OPAInput struct {
	Subject  OPASubject `json:"subject"`
	Action   string     `json:"action"`
	Resource Resource   `json:"resource"`
	Context  OPAContext `json:"context"`
}

type OPASubject struct {
	UserID string   `json:"user_id"`
	Roles  []string `json:"roles"`
	// TenantRole is the membership role in the active tenant. It is a separate
	// claim, never merged into Roles: is_admin only reads Roles, so a tenant
	// "admin" can never trip platform-plane rules (D6/F1). Content policies
	// read input.subject.tenant_role.
	TenantRole string `json:"tenant_role,omitempty"`
}

type OPAContext struct {
	TenantID    string `json:"tenant_id,omitempty"`
	Region      string `json:"region,omitempty"`
	MFAVerified bool   `json:"mfa_verified"`
}

// BuildOPAInput maps authn subject + request into the policy document.
func BuildOPAInput(sub authn.Subject, in Input, extraRoles []string) OPAInput {
	roles := mergeRoles(sub.Roles, extraRoles)
	return OPAInput{
		Subject: OPASubject{
			UserID:     sub.UserID.String(),
			Roles:      roles,
			TenantRole: sub.TenantRole,
		},
		Action:   in.Action,
		Resource: in.Resource,
		Context: OPAContext{
			TenantID:    sub.TenantID,
			Region:      sub.Region,
			MFAVerified: sub.MFAVerified,
		},
	}
}

func mergeRoles(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, r := range list {
			if r == "" {
				continue
			}
			if _, ok := seen[r]; ok {
				continue
			}
			seen[r] = struct{}{}
			out = append(out, r)
		}
	}
	return out
}
