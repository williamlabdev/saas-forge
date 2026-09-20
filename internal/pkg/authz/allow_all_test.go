package authz

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
)

// Compile-time proof the dev authorizer satisfies the interface.
var _ Authorizer = (*AllowAllAuthorizer)(nil)

func TestAllowAllAuthorizer_PermitsEveryAction(t *testing.T) {
	auth := NewAllowAllAuthorizer()
	actions := []string{
		ActionUserList, ActionUserRead, ActionUserUpdate, ActionUserDelete,
		ActionIAMRoleAssign, ActionPlatformAppCreate,
		"", "totally:unknown:action",
	}
	for _, action := range actions {
		t.Run(action, func(t *testing.T) {
			// No subject in context on purpose: AllowAll ignores identity.
			err := auth.Allow(context.Background(), Input{
				Action:   action,
				Resource: Resource{Type: "user", ID: uuid.NewString()},
			})
			require.NoError(t, err)
		})
	}
}

// TestAuthorizers_SharedContract runs identical requests through all three
// implementations to document where they agree and where they diverge:
// AllowAll always permits (dev-only), while RBAC and OPA enforce identity and
// permissions in lockstep.
func TestAuthorizers_SharedContract(t *testing.T) {
	opa, err := NewOPAAuthorizer(nil)
	require.NoError(t, err)
	allow := NewAllowAllAuthorizer()
	rbac := NewRBACAuthorizer()

	adminCtx := authn.WithSubject(context.Background(),
		authn.Subject{UserID: uuid.New(), Roles: []string{"admin"}})
	memberCtx := authn.WithSubject(context.Background(),
		authn.Subject{UserID: uuid.New(), Roles: []string{"member"}})
	noSubjectCtx := context.Background()

	cases := []struct {
		name     string
		ctx      context.Context
		in       Input
		realDeny bool // whether the real (RBAC + OPA) authorizers deny
	}{
		{
			name:     "admin deletes another user is allowed",
			ctx:      adminCtx,
			in:       Input{Action: ActionUserDelete, Resource: Resource{Type: "user", ID: uuid.NewString()}},
			realDeny: false,
		},
		{
			name:     "member deletes another user is denied",
			ctx:      memberCtx,
			in:       Input{Action: ActionUserDelete, Resource: Resource{Type: "user", ID: uuid.NewString()}},
			realDeny: true,
		},
		{
			name:     "no subject is denied",
			ctx:      noSubjectCtx,
			in:       Input{Action: ActionUserRead, Resource: Resource{Type: "user", ID: uuid.NewString()}},
			realDeny: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// AllowAll permits regardless of subject or permission.
			require.NoError(t, allow.Allow(tc.ctx, tc.in), "AllowAll must permit everything")

			rbacErr := rbac.Allow(tc.ctx, tc.in)
			opaErr := opa.Allow(tc.ctx, tc.in)
			if tc.realDeny {
				require.Error(t, rbacErr, "RBAC should deny")
				require.Error(t, opaErr, "OPA should deny")
			} else {
				require.NoError(t, rbacErr, "RBAC should allow")
				require.NoError(t, opaErr, "OPA should allow")
			}
		})
	}
}
