package authz

# Default deny; explicit allow rules only.
default allow := false

is_admin if {
	some role in input.subject.roles
	role == "admin"
}

# Tenant-plane actions are decided ONLY by the membership role in the active
# tenant (input.subject.tenant_role, a separate claim — D6). Platform is_admin
# deliberately does NOT reach them (D12, settled in PR4): a platform operator
# touches tenant data via the platform console and explicit, auditable flows —
# never via a silent blanket. This also keeps the F1 collision closed from both
# directions: tenant "admin" never trips is_admin, and platform "admin" never
# trips tenant rules.
tenant_plane_action if startswith(input.action, "content:")

tenant_plane_action if startswith(input.action, "tenant:invite:")

# agent:* is tenant-plane for the same reason: minting a credential that acts
# inside a tenant is a power over that tenant's content, one indirection away,
# so the platform blanket must not reach it either.
tenant_plane_action if startswith(input.action, "agent:")

allow if {
	is_admin
	not tenant_plane_action
}

allow if {
	input.action in {"user:read", "user:update", "user:preferences:update"}
	input.subject.user_id == input.resource.id
}

allow if {
	input.action == "user:delete"
	is_admin
}

allow if {
	input.action in {"iam:role:read", "iam:role:assign", "iam:role:revoke"}
	is_admin
}

allow if {
	input.action == "user:list"
	is_admin
}

allow if {
	input.action in {"platform:app:list", "platform:app:create", "platform:app:update", "platform:tenant:plan:update"}
	is_admin
}

allow if {
	input.action in {"notification:read", "notification:create"}
	input.subject.user_id == input.resource.id
}

# --- content: the D3 capability matrix over the tenant plane -----------------
# owner/admin/editor: every content verb; viewer: read-only. All rules require
# an active tenant — tenant-less tokens (platform operators) get nothing here.

content_write_roles := {"owner", "admin", "editor"}

content_actions := {"content:create", "content:read", "content:update", "content:delete", "content:list"}

allow if {
	input.action in content_actions
	input.context.tenant_id != ""
	input.subject.tenant_role in content_write_roles
}

allow if {
	input.action in {"content:read", "content:list"}
	input.context.tenant_id != ""
	input.subject.tenant_role == "viewer"
}

# Putting an entry live. Same roles as the content verbs — whoever could update
# can publish, so no person's access changes (ruled 2026-08-06, ADR-014 §1).
#
# Still its OWN rule rather than a member of content_actions, for the reason
# schema:amend is: the roles happening to coincide today is not the same claim
# as the verbs being one verb, and folding it in would make "may edit, may not
# release" unexpressible the moment someone wants it. The agent gate is what
# actually consumes the split today, and it is enforced in Go before this
# policy is reached — an agent is refused the verb, not granted it here and
# taken away later.
#
# Note UNPUBLISH is not this action: it stays on content:update, because taking
# something down is stopping the bleeding (ADR-014 §1).
allow if {
	input.action == "content:publish"
	input.context.tenant_id != ""
	input.subject.tenant_role in content_write_roles
}

# The activity stream (ADR-014 §3) and the release queue (§2). Both were shipped
# on content:list and therefore reached VIEWER; ruled the other way on
# 2026-08-06, so each takes a read verb of its own and viewer loses them.
#
# THE POINT OF THESE TWO RULES IS WHAT THEY OMIT. content_actions has a
# companion rule below it granting viewer content:read and content:list, and
# these verbs are deliberately in neither set — being absent from content_actions
# is what keeps viewer out, and that omission is the whole ruling. Folding either
# verb into content_actions would silently restore the behaviour this replaced.
#
# Two rules rather than one action set with both verbs, matching the Go table:
# the roles coincide today, which is not the claim that the stream and the queue
# are one power. An agent holds neither — refused in Go by the agent gate before
# this policy is consulted.
allow if {
	input.action == "content:activity:read"
	input.context.tenant_id != ""
	input.subject.tenant_role in content_write_roles
}

allow if {
	input.action == "content:review:list"
	input.context.tenant_id != ""
	input.subject.tenant_role in content_write_roles
}

# Destructive schema verbs — deleting or renaming a field, renaming or deleting
# a content type — are owner/admin only. Deliberately NOT a member of
# content_actions: adding it there would hand it to editor through the rule
# above, which is the whole thing this separation exists to prevent.
allow if {
	input.action == "content:schema:write"
	input.context.tenant_id != ""
	input.subject.tenant_role in {"owner", "admin"}
}

# Planning a schema change writes nothing, but it reports entry counts and which
# steps are blocked. It used to carry the write verb's roles on the argument
# that this is schema administration rather than reading content; ruled the
# other way on 2026-08-30 (ADR-013 補裁 T) and it now carries the content roles,
# so an editor may plan.
#
# The reason is not that planning turned out to be harmless. It is that plan and
# propose were built FOR agents (§3) and, after 補裁 S-1 made the credential's
# tenant role a choice, owner/admin-only shut them to the only agent shape that
# own_only_roles can confine. Plan opens together with propose and never alone:
# a proposer who cannot read a plan cannot know what their proposal says.
#
# STILL ITS OWN RULE rather than a member of content_actions, for the reason
# publish and amend are: the roles coinciding today is not the claim that
# planning and editing an entry are one power, and folding it in would make
# "may edit, may not plan" unsayable the moment someone wants it. Keep this in
# lockstep with allowContentByTenantRole in rbac_authorizer.go.
allow if {
	input.action == "content:schema:plan"
	input.context.tenant_id != ""
	input.subject.tenant_role in content_write_roles
}

# Filing a schema artifact for a person to approve (ADR-013 §3 step 8). Carries
# the content roles as of 2026-08-30 (ADR-013 補裁 T) — see the plan rule above
# for why the two moved together and could not move one at a time.
#
# Still a separate action from plan because proposing WRITES a row while
# planning writes nothing — one verb for both would make "may dry-run, may not
# fill someone's queue" unsayable.
#
# This rule is not what stops an agent from applying: content:schema:write is,
# and it has its own rule above that no agent reaches. A proposal endpoint is a
# review trail, not a boundary — the caller who skips it and posts straight to
# /schema/apply is refused by the verb, not by the absence of a proposal. That
# is also what bounds today's widening: what an editor gained is the ability to
# ASK, and the queue is still read and decided by owner/admin.
allow if {
	input.action == "content:schema:propose"
	input.context.tenant_id != ""
	input.subject.tenant_role in content_write_roles
}

# Additive, reversible schema edits — add a field, change a label. Same roles as
# the content verbs (an editor keeps these, ADR-009), so this rule grants
# content_write_roles rather than the owner/admin pair above.
#
# Still its OWN action rather than a member of content_actions: sharing
# content:update with "change one entry" is what let a credential scoped to
# entry writing also reshape the type (ADR-013 §B). Kept out of the set for the
# same reason schema:write is — membership there is what hands a verb over.
allow if {
	input.action == "content:schema:amend"
	input.context.tenant_id != ""
	input.subject.tenant_role in content_write_roles
}

# --- tenant invites (PR-invite) -----------------------------------------------
# Create: owner/admin of the ACTIVE tenant only, and the resource must BE the
# active tenant (the tenant-plane role speaks for no other tenant). The service
# additionally re-checks the membership fresh from the DB.
allow if {
	input.action == "tenant:invite:create"
	input.context.tenant_id != ""
	input.resource.id == input.context.tenant_id
	input.subject.tenant_role in {"owner", "admin"}
}

# Accept: any authenticated subject — the invite token + email binding is the
# real gate, and the acceptor has no role in the target tenant yet.
allow if {
	input.action == "tenant:invite:accept"
	input.subject.user_id != ""
}

# --- agent credentials (ADR-013) ----------------------------------------------
# owner/admin of the ACTIVE tenant, for all three verbs (ruled 2026-08-06):
# deciding that something unattended may edit content is membership
# administration, not editing. No resource.id check — the caller cannot name a
# tenant; see the Go authorizer for why.
#
# Written as three rules over one `in` set rather than one rule over a set of
# actions, because the verbs are separate ON PURPOSE (issue vs revoke is the
# incident-response split) and a shared rule is the thing that would quietly
# re-fuse them the first time one of the three needs a different condition.
allow if {
	input.action == "agent:credential:issue"
	input.context.tenant_id != ""
	input.subject.tenant_role in {"owner", "admin"}
}

allow if {
	input.action == "agent:credential:list"
	input.context.tenant_id != ""
	input.subject.tenant_role in {"owner", "admin"}
}

allow if {
	input.action == "agent:credential:revoke"
	input.context.tenant_id != ""
	input.subject.tenant_role in {"owner", "admin"}
}
