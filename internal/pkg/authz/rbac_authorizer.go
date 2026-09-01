package authz

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

const roleAdmin = "admin"

// RBACAuthorizer is an MVP facade: role checks today, OPA replacement tomorrow.
type RBACAuthorizer struct{}

func NewRBACAuthorizer() *RBACAuthorizer {
	return &RBACAuthorizer{}
}

func (r *RBACAuthorizer) Allow(ctx context.Context, in Input) error {
	sub, ok := authn.SubjectFromContext(ctx)
	if !ok {
		return apperrors.ErrUnauthorized
	}

	// Before any role is consulted: an agent credential may only exercise the
	// verbs ADR-013 enumerates. Placed above the tenant-plane branch AND above
	// the platform-admin bypass, because both would otherwise decide the answer
	// from the role the agent inherited from its minter.
	if err := refuseUnlistedAgentAction(sub, in.Action); err != nil {
		return err
	}

	// Tenant-plane actions (matched by PREFIX, same as the rego policy's
	// tenant_plane_action) are decided ONLY by the membership role — evaluated
	// BEFORE the platform-admin bypass (D12, settled in PR4): platform admin
	// gets no blanket over tenant data, and an unmatched tenant-plane verb
	// fails CLOSED here just like in rego — adding a new content:*/invite verb
	// without a rule denies it for everyone in both modes. Isolation itself is
	// enforced by repository scoping on subject.TenantID regardless of the
	// verb decision here.
	if isTenantPlaneAction(in.Action) {
		if isContentAction(in.Action) {
			if sub.TenantID == "" {
				return apperrors.ErrForbidden
			}
			return allowContentByTenantRole(sub.TenantRole, in.Action)
		}
		// Membership management (D3: owner/admin 管成員). Accepting an invite
		// needs authentication only — the invite token + email binding is the
		// actual gate, and the acceptor has no role in the tenant yet. The
		// tenant service additionally re-checks the membership from the DB.
		switch in.Action {
		case ActionTenantInviteCreate:
			// The tenant-plane role only speaks for the subject's ACTIVE
			// tenant — refuse if the caller targets any other tenant.
			if sub.TenantID == "" || in.Resource.ID != sub.TenantID {
				return apperrors.ErrForbidden
			}
			if sub.TenantRole == "owner" || sub.TenantRole == "admin" {
				return nil
			}
			return apperrors.ErrForbidden
		case ActionTenantInviteAccept:
			return nil
		case ActionAgentCredentialIssue, ActionAgentCredentialList, ActionAgentCredentialRevoke:
			// owner/admin of the ACTIVE tenant (ruled 2026-08-06). Unlike
			// invite:create there is no resource.ID == tenant check, and that is
			// not an omission: the minter cannot NAME a tenant. IssueAgentToken
			// copies TenantID off the minter's own claims, and revoke/list are
			// scoped by `WHERE tenant_id = $subject` in the repository, so the
			// active tenant is the only one any of these three verbs can reach.
			// A check against a value the caller never supplies would read as if
			// it were load-bearing.
			if sub.TenantID == "" {
				return apperrors.ErrForbidden
			}
			if sub.TenantRole == "owner" || sub.TenantRole == "admin" {
				return nil
			}
			return apperrors.ErrForbidden
		}
		return apperrors.ErrForbidden
	}

	if sub.HasRole(roleAdmin) {
		return nil
	}

	if isIAMAction(in.Action) || isPlatformAction(in.Action) {
		return apperrors.ErrForbidden
	}

	resourceID, err := uuid.Parse(in.Resource.ID)
	if err != nil {
		return apperrors.ErrForbidden
	}

	switch in.Action {
	case ActionNotificationRead, ActionNotificationCreate:
		if sub.UserID == resourceID {
			return nil
		}
		return apperrors.ErrForbidden
	case ActionUserRead, ActionUserUpdate, ActionUserPreferencesUpdate:
		if sub.UserID == resourceID {
			return nil
		}
		return apperrors.ErrForbidden
	case ActionUserDelete:
		return apperrors.ErrForbidden
	default:
		return apperrors.ErrForbidden
	}
}

func isIAMAction(action string) bool {
	switch action {
	case ActionIAMRoleRead, ActionIAMRoleAssign, ActionIAMRoleRevoke:
		return true
	default:
		return false
	}
}

func isPlatformAction(action string) bool {
	switch action {
	case ActionPlatformAppList, ActionPlatformAppCreate, ActionPlatformAppUpdate, ActionPlatformTenantPlanSet:
		return true
	default:
		return false
	}
}

// isTenantPlaneAction mirrors the rego policy's tenant_plane_action prefixes;
// keep the two in lockstep.
func isTenantPlaneAction(action string) bool {
	return strings.HasPrefix(action, "content:") ||
		strings.HasPrefix(action, "tenant:invite:") ||
		// agent:* is tenant-plane so that the platform-admin blanket above does
		// NOT reach it (D12). A platform operator holding the power to mint a
		// credential inside any tenant is the silent blanket that rule exists to
		// refuse — and it would be a blanket over CONTENT, one indirection away.
		strings.HasPrefix(action, "agent:")
}

func isContentAction(action string) bool {
	switch action {
	case ActionContentList, ActionContentRead, ActionContentCreate, ActionContentUpdate, ActionContentDelete,
		ActionContentPublish, ActionContentSchemaWrite, ActionContentSchemaPlan, ActionContentSchemaPropose,
		ActionContentSchemaAmend, ActionContentActivityRead, ActionContentReviewList:
		return true
	default:
		return false
	}
}

// allowContentByTenantRole applies the D3 capability matrix: owner/admin/editor
// get all content verbs, viewer reads only, no membership gets nothing.
func allowContentByTenantRole(tenantRole, action string) error {
	// Destructive schema changes are owner/admin only. An editor deleting a
	// field rewrites every entry of the type, and deleting a type cascades the
	// entries away entirely — a scale of loss the content verbs cannot reach.
	//
	// PLAN AND PROPOSE USED TO RIDE HERE AND NO LONGER DO (ruled 2026-08-30,
	// ADR-013 補裁 T). They now fall through to the switch below, alongside
	// publish and amend.
	//
	// The argument for keeping them shut was sound and is worth restating,
	// because it is what made the two verbs open TOGETHER rather than one at a
	// time: a person who cannot see a plan has no way to have meant what a
	// proposal says, so opening propose alone would have manufactured proposals
	// their own author could not read. That argument was never wrong; it only
	// ever settled the SHAPE of the change, not whether to make it.
	//
	// What it could not survive was the cost of the pair staying shut. 補裁 S-1
	// made the minter choose a credential's tenant role, and with plan and
	// propose owner/admin only that choice was "an agent that can propose" OR
	// "an agent own_only_roles can confine" — never both, because own_only_roles
	// is a set-membership test (activity_repository.go) that an owner/admin
	// agent never satisfies. The two verbs were created FOR agents (ADR-013 §3)
	// and were shut to the only shape of agent that can be contained.
	//
	// Neither verb applies anything, and that is why the widening is narrow:
	// schema:write stays above and no agent reaches it, while a proposal is a
	// review trail rather than a boundary (a caller who skips it and posts
	// straight to /schema/apply is refused by the verb, not by the missing
	// proposal). What opens is "may dry-run, may fill a queue somebody else
	// decides" — to people already trusted with every entry of every type by
	// the switch below.
	//
	// ⚠️ The widening fired a trigger the ADR wrote down in advance:
	// notifyProposalFiled notifies only for AGENT proposals, on the reasoning
	// that a human proposer was by construction an approver. An editor is not.
	// See the ⚠️ in proposal_notify.go — that rule is now knowingly stale and
	// is tracked as an open item, not fixed here.
	if action == ActionContentSchemaWrite {
		if tenantRole == "owner" || tenantRole == "admin" {
			return nil
		}
		return apperrors.ErrForbidden
	}
	// content:publish deliberately has NO branch of its own: it rides the
	// switch below, which is precisely the ruling — whoever could update can
	// publish, so no person's access changes when the verb splits off
	// (2026-08-06). Writing it out as `if action == ActionContentPublish` with
	// the same three roles would be a second copy of the answer, free to drift
	// from this one, and this repo's rule is that a redundant layer with no
	// reachable case of its own is decoration.
	//
	// What the split buys is spent elsewhere: refuseUnlistedAgentAction does
	// not enumerate the verb, so an AGENT credential is refused before this
	// function is consulted. Separating editor from publisher for PEOPLE is a
	// further increment and belongs here when there is a real role for it.
	//
	// content:activity:read and content:review:list have no branch of their own
	// EITHER, and here that is not merely economy — it is the ruling. The switch
	// below gives them to owner/admin/editor and refuses viewer, and refusing
	// viewer is the entire behavioural change (ruled 2026-08-06). Anyone who had
	// the stream or the queue yesterday still has it; the one role that loses it
	// is the one the ruling named.
	//
	// Tightening the STREAM further — owner/admin only, on the argument that a
	// tenant-wide audit log is administration and not editing — is one line here:
	// an `if action == ActionContentActivityRead` branch above the switch. It is
	// deliberately absent because it would take something away from editors that
	// the ruling did not ask to take, and editors are the people reviewing the
	// agent output the stream exists to explain.
	switch tenantRole {
	case "owner", "admin", "editor":
		return nil
	case "viewer":
		if action == ActionContentList || action == ActionContentRead {
			return nil
		}
		return apperrors.ErrForbidden
	default:
		return apperrors.ErrForbidden
	}
}
