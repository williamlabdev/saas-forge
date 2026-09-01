package authz

import (
	"context"
)

// Action names are stable contracts for policies (Rego/RBAC facade).
const (
	ActionUserList              = "user:list"
	ActionUserRead              = "user:read"
	ActionUserUpdate            = "user:update"
	ActionUserDelete            = "user:delete"
	ActionUserPreferencesUpdate = "user:preferences:update"
	ActionIAMRoleRead           = "iam:role:read"
	ActionIAMRoleAssign         = "iam:role:assign"
	ActionIAMRoleRevoke         = "iam:role:revoke"
	ActionNotificationRead      = "notification:read"
	ActionNotificationCreate    = "notification:create"
	ActionPlatformAppList       = "platform:app:list"
	ActionPlatformAppCreate     = "platform:app:create"
	ActionPlatformAppUpdate     = "platform:app:update"
	ActionPlatformTenantPlanSet = "platform:tenant:plan:update"
	ActionContentList           = "content:list"
	ActionContentRead           = "content:read"
	ActionContentCreate         = "content:create"
	ActionContentUpdate         = "content:update"
	ActionContentDelete         = "content:delete"
	// ActionContentPublish covers putting an entry's working copy live. It is
	// split from content:update because "may change the working copy" and "may
	// put it in front of the public" are different powers, and until this verb
	// existed no authorization checkpoint could see which one a call was asking
	// for: publish and unpublish share one method behind one action
	// (SetEntryStatus), so an authorizer had exactly one answer for both.
	//
	// Same tenant roles as the content verbs, so NO PERSON'S ACCESS CHANGES
	// (ruled 2026-08-06: whoever could update can publish). The split is what
	// makes ADR-014 §1's gate expressible at the authorization layer rather
	// than in a tool list — an agent may write the working copy and may not
	// release it, and that sentence has no server-side referent without a verb
	// of its own.
	//
	// UNPUBLISH DELIBERATELY STAYS ON content:update. Taking something down is
	// stopping the bleeding, and gating it would require the harm to continue
	// until a person arrives (ADR-014 §1). That is only sound because §5.1 made
	// retract non-destructive — before it, unpublish cleared the snapshot, so
	// "edit the payload, then unpublish" was a two-step route around this gate.
	ActionContentPublish = "content:publish"
	// ActionContentSchemaWrite covers the DESTRUCTIVE schema verbs — deleting or
	// renaming a field, renaming or deleting a content type. It is separate from
	// content:update because the blast radius is different in kind: a content
	// verb changes one entry, these change every entry of a type at once, and an
	// editor who mistypes a type name should not be able to drop a collection.
	// Adding a field and editing its label stay on content:update — they are
	// additive and reversible.
	ActionContentSchemaWrite = "content:schema:write"
	// ActionContentSchemaPlan covers reporting what a schema artifact WOULD do
	// without writing anything. It is split from content:schema:write so that a
	// caller can be granted the dry run without the apply — the two share the
	// same tenant roles today, but a shared verb makes "may plan, may not apply"
	// unexpressible, and an authorizer that cannot express it cannot enforce it
	// no matter which endpoint the caller goes through (ADR-013 §3).
	ActionContentSchemaPlan = "content:schema:plan"
	// ActionContentSchemaPropose covers filing a schema artifact for a person to
	// approve (ADR-013 §3 step 8). It is NOT the authorization boundary — that is
	// content:schema:write, which the proposal flow sits behind and which no
	// agent holds; a proposal endpoint could never stop a caller that goes
	// straight to /schema/apply, and the first draft of the ADR was wrong about
	// exactly that.
	//
	// So why a verb of its own rather than riding on plan? Because proposing
	// WRITES A ROW and planning writes nothing. Sharing the verb would mean
	// "may see what this document would do" and "may put work in a person's
	// queue" are the same sentence, and there is no way to withdraw the second
	// from a credential that has been noisy without also blinding it.
	//
	// Same tenant roles as plan and write, so no person's access changes: an
	// editor cannot propose, because an editor cannot do schema administration
	// today at all. If a lower-privileged PERSON ever needs to ask for a schema
	// change, this is the verb to widen — and widening it is safe in a way that
	// widening write is not, which is the point of having it.
	ActionContentSchemaPropose = "content:schema:propose"
	// ActionContentSchemaAmend covers the ADDITIVE, reversible schema verbs —
	// adding a field, editing a type's or a field's label. These used to ride on
	// content:update, which is defensible for a person (ADR-009: additive and
	// reversible, so an editor keeps them) but stops being defensible once a
	// credential can be issued for entry writing alone: sharing the verb with
	// "change one entry" means anything granted entry writes on a type can also
	// reshape that type, whatever the caller was nominally handed (ADR-013 §B).
	//
	// Same tenant roles as the content verbs, so no person's access changes.
	// Permission-list edits are NOT here — they escalate to schema:write, which
	// is what keeps this verb from becoming a privilege-escalation route.
	ActionContentSchemaAmend = "content:schema:amend"
	// ActionContentActivityRead covers reading the tenant-wide activity stream
	// (ADR-014 §3) — who did what to which entry, successes and refusals.
	//
	// It exists because the stream first shipped on content:list, which handed it
	// to every role that can list content, VIEWER INCLUDED. That was a deliberate
	// trade at the time (no new verb means no RBAC row and no rego rule) and it
	// was ruled the other way on 2026-08-06: the consequence is not acceptable and
	// the cost is paid. Roles are owner/admin/editor — viewer loses the stream,
	// and NOBODY ELSE'S ACCESS CHANGES, which is the narrowest landing that
	// answers the ruling.
	//
	// SEPARATE FROM ActionContentReviewList below even though the two carry the
	// same roles today, for the reason schema:plan is separate from schema:write:
	// roles coinciding today is not the claim that the verbs are one verb. The
	// stream is a tenant-wide audit record and the queue is a work list, and
	// "sees what is waiting for them, does not see the tenant's audit log" is an
	// ordinary role to want. One verb makes that unexpressible, and an authorizer
	// that cannot express a rule cannot enforce it (ADR-013 §3).
	//
	// An AGENT is refused it twice: ADR-013 §4's untyped rule at the CMS
	// chokepoint, which fires first and is what actually returns the error, and
	// refuseUnlistedAgentAction here, which does not enumerate the verb. The
	// second one is the point of having a verb at all — §4's rule is a rule about
	// how the stream is CALLED (it names no content type), and §A records
	// relaxing it as a standing temptation. Without a verb of its own there was
	// nothing to write the durable half of the answer against.
	ActionContentActivityRead = "content:activity:read"
	// ActionContentReviewList covers reading the release queue (ADR-014 §2) —
	// everything in the tenant waiting on a person to publish it.
	//
	// Same history, same ruling and same roles as content:activity:read above;
	// see there for why the two are separate verbs. owner/admin/editor is also
	// exactly the set that holds content:publish, which is the point of the queue:
	// it routes work to the people who can clear it.
	ActionContentReviewList  = "content:review:list"
	ActionTenantInviteCreate = "tenant:invite:create"
	ActionTenantInviteAccept = "tenant:invite:accept"
	// ActionAgentCredentialIssue covers minting an agent credential — handing a
	// non-human a token that acts inside the tenant (ADR-013 §1).
	//
	// Roles are owner/admin, NOT the content-write set (ruled 2026-08-06).
	// Issuing a credential is not editing content, it is deciding that something
	// unattended may edit content, and that is the same shape as inviting a
	// member: tenant:invite:create carries exactly these two roles for exactly
	// this reason.
	//
	// THIS VERB DECIDES WHO MAY MINT; IT DOES NOT DECIDE WHAT THEY MAY MINT.
	// The second question is answered by domain.CanMintAgentRole (ADR-013 補裁
	// S-1, ruled 2026-08-20): owner grants {admin, editor, viewer}, admin grants
	// {editor, viewer}, and neither may grant its own role. Until then the role
	// was COPIED off the minter, so every credential in existence carried owner
	// or admin and own_only_roles naming only "editor" could confine no agent at
	// all — the credential was bounded by refuseUnlistedAgentAction and its type
	// whitelist, but never by its role.
	//
	// That table is a table and not an ordering over the four roles on purpose:
	// a general rank() would be reachable from every other decision here, and
	// each caller would be betting that "greater than" means the same thing in
	// its context as it does in this one.
	ActionAgentCredentialIssue = "agent:credential:issue" //nolint:gosec // G101: an action NAME, not a credential — the identifier merely contains "credential".
	// ActionAgentCredentialList covers reading the tenant's minted credentials.
	// It is what makes revocation usable rather than merely possible: without a
	// way to see what is outstanding, revoking by id is a verb nobody can reach.
	ActionAgentCredentialList = "agent:credential:list" //nolint:gosec // G101: an action NAME, not a credential — the identifier merely contains "credential".
	// ActionAgentCredentialRevoke covers turning a live agent credential off
	// before it expires. It is the whole reason the credential has a row in the
	// database at all — a JWT is otherwise valid until it expires no matter what
	// anyone decides afterwards, and a 30-day TTL without revocation would mean a
	// leaked agent token stays good for a month (ruled 2026-08-06).
	//
	// Separate from issue rather than riding on it, even though the roles
	// coincide today, for the reason schema:plan is separate from schema:write:
	// "may stop an agent, may not create one" is an ordinary thing to want —
	// it is the incident-response shape — and an authorizer that cannot express
	// a rule cannot enforce it (ADR-013 §3).
	ActionAgentCredentialRevoke = "agent:credential:revoke" //nolint:gosec // G101: an action NAME, not a credential — the identifier merely contains "credential".
)

// Resource describes the object being accessed.
type Resource struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Input is the policy evaluation request (ABAC-ready).
type Input struct {
	Action   string   `json:"action"`
	Resource Resource `json:"resource"`
}

// Authorizer is the single authorization decision point (OPA-ready).
// PolicyEvaluator in HLD maps to this interface.
type Authorizer interface {
	Allow(ctx context.Context, in Input) error
}
