package authz

import rego.v1

test_member_self_read if {
	allow with input as {
		"subject": {"user_id": "11111111-1111-1111-1111-111111111111", "roles": ["member"]},
		"action": "user:read",
		"resource": {"type": "user", "id": "11111111-1111-1111-1111-111111111111"},
		"context": {},
	}
}

test_admin_delete_other if {
	allow with input as {
		"subject": {"user_id": "22222222-2222-2222-2222-222222222222", "roles": ["admin"]},
		"action": "user:delete",
		"resource": {"type": "user", "id": "11111111-1111-1111-1111-111111111111"},
		"context": {},
	}
}

test_member_cannot_delete if {
	not allow with input as {
		"subject": {"user_id": "11111111-1111-1111-1111-111111111111", "roles": ["member"]},
		"action": "user:delete",
		"resource": {"type": "user", "id": "11111111-1111-1111-1111-111111111111"},
		"context": {},
	}
}

# --- tenant plane: D3 content matrix ------------------------------------------

test_editor_creates_content if {
	allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "editor"},
		"action": "content:create",
		"resource": {"type": "content", "id": "collection"},
		"context": {"tenant_id": "t_a"},
	}
}

test_viewer_reads_content if {
	allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "viewer"},
		"action": "content:read",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": "t_a"},
	}
}

test_viewer_cannot_write_content if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "viewer"},
		"action": "content:create",
		"resource": {"type": "content", "id": "collection"},
		"context": {"tenant_id": "t_a"},
	}
}

test_no_membership_no_content if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": ""},
		"action": "content:read",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": "t_a"},
	}
}

# Destructive schema verbs are owner/admin only. The editor case is the one that
# matters: content:schema:write must NOT be a member of content_actions, or the
# write-roles rule would hand it over.

test_admin_writes_schema if {
	allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "admin"},
		"action": "content:schema:write",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": "t_a"},
	}
}

test_editor_cannot_write_schema if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "editor"},
		"action": "content:schema:write",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": "t_a"},
	}
}

# Planning is a separate action carrying the CONTENT roles as of 2026-08-30
# (ADR-013 補裁 T); it used to carry the write verb's owner/admin pair.
#
# THE LOAD-BEARING CASE MOVED, it did not disappear. It used to be the editor
# line; it is now the VIEWER line below. "Editor may plan" and "anyone with a
# membership may plan" are one edit apart — putting the verb into
# content_actions would produce both, and only the viewer case can tell them
# apart. Keep it.

test_admin_plans_schema if {
	allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "admin"},
		"action": "content:schema:plan",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": "t_a"},
	}
}

test_editor_plans_schema if {
	allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "editor"},
		"action": "content:schema:plan",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": "t_a"},
	}
}

test_viewer_cannot_plan_schema if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "viewer"},
		"action": "content:schema:plan",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": "t_a"},
	}
}

test_schema_plan_needs_a_tenant if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": ["platform_admin"], "tenant_role": "admin"},
		"action": "content:schema:plan",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": ""},
	}
}

# Proposing is a third action carrying the content roles too (ADR-013 §3 step 8,
# 補裁 T). Editors filling the approvers' queue is now the intended behaviour
# rather than the leak this pair used to guard against — what bounds it is that
# the queue is still READ and DECIDED by owner/admin only.
#
# The two editor cases (here and for plan) are a PAIR by construction: the verbs
# were opened in one edit because a proposer who cannot read a plan cannot know
# what the proposal says. Either one going red alone means they have drifted.

test_admin_proposes_schema if {
	allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "admin"},
		"action": "content:schema:propose",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": "t_a"},
	}
}

test_editor_proposes_schema if {
	allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "editor"},
		"action": "content:schema:propose",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": "t_a"},
	}
}

test_viewer_cannot_propose_schema if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "viewer"},
		"action": "content:schema:propose",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": "t_a"},
	}
}

test_schema_propose_needs_a_tenant if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": ["platform_admin"], "tenant_role": "admin"},
		"action": "content:schema:propose",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": ""},
	}
}

# Additive schema edits keep the content roles (ADR-013 §B). The editor pair is
# the load-bearing one in both directions: it must still hold amend, and must
# still not hold write.

test_editor_amends_schema if {
	allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "editor"},
		"action": "content:schema:amend",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": "t_a"},
	}
}

test_viewer_cannot_amend_schema if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "viewer"},
		"action": "content:schema:amend",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": "t_a"},
	}
}

test_schema_amend_needs_a_tenant if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": ["platform_admin"], "tenant_role": "editor"},
		"action": "content:schema:amend",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": ""},
	}
}

# content:publish — ADR-014 §1. The editor case is the load-bearing one: the
# verb split off content:update must not have narrowed any PERSON's access, and
# an editor losing publish here is how that would first show up. The gate the
# split exists for is enforced in Go against agent credentials, which this
# policy cannot see — so a green suite here is not evidence the gate works.
test_editor_publishes if {
	allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "editor"},
		"action": "content:publish",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": "t_a"},
	}
}

test_viewer_cannot_publish if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "viewer"},
		"action": "content:publish",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": "t_a"},
	}
}

test_publish_needs_a_tenant if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": ["platform_admin"], "tenant_role": "editor"},
		"action": "content:publish",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": ""},
	}
}

# The activity stream and the release queue (ADR-014 §3/§2), ruled 2026-08-06.
#
# THE VIEWER TESTS ARE THE POINT. Both verbs replace a use of content:list, and
# viewer holds content:list through the rule beside content_actions — so if
# either verb were ever folded into that set, or a rule for it written with the
# viewer branch's roles, the narrowing would silently undo itself. These two
# tests are what notices.
#
# The editor tests are the other half: the ruling took the stream away from
# viewer and from NOBODY ELSE, and an editor losing it is how a narrowing done
# by the wrong instrument would show up here first.
test_editor_reads_activity_stream if {
	allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "editor"},
		"action": "content:activity:read",
		"resource": {"type": "content", "id": "activity"},
		"context": {"tenant_id": "t_a"},
	}
}

test_viewer_cannot_read_activity_stream if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "viewer"},
		"action": "content:activity:read",
		"resource": {"type": "content", "id": "activity"},
		"context": {"tenant_id": "t_a"},
	}
}

test_editor_reads_release_queue if {
	allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "editor"},
		"action": "content:review:list",
		"resource": {"type": "content", "id": "pending-review"},
		"context": {"tenant_id": "t_a"},
	}
}

test_viewer_cannot_read_release_queue if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "viewer"},
		"action": "content:review:list",
		"resource": {"type": "content", "id": "pending-review"},
		"context": {"tenant_id": "t_a"},
	}
}

test_activity_stream_needs_a_tenant if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": ["platform_admin"], "tenant_role": "editor"},
		"action": "content:activity:read",
		"resource": {"type": "content", "id": "activity"},
		"context": {"tenant_id": ""},
	}
}

test_schema_write_needs_a_tenant if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": ["platform_admin"], "tenant_role": "admin"},
		"action": "content:schema:write",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": ""},
	}
}

test_empty_tenant_no_content if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "owner"},
		"action": "content:read",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": ""},
	}
}

# D12 (settled here): platform admin gets NO blanket over the tenant plane.
test_platform_admin_no_content_blanket if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": ["admin"], "tenant_role": ""},
		"action": "content:read",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": "t_a"},
	}
}

test_platform_admin_keeps_platform_actions if {
	allow with input as {
		"subject": {"user_id": "u1", "roles": ["admin"], "tenant_role": ""},
		"action": "platform:app:create",
		"resource": {"type": "platform_app", "id": "x"},
		"context": {},
	}
}

# F1 regression lock (plan §7 #4): tenant admin must never trip is_admin.
test_tenant_admin_is_not_platform_admin if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "admin"},
		"action": "platform:app:create",
		"resource": {"type": "platform_app", "id": "x"},
		"context": {"tenant_id": "t_a"},
	}
}

test_tenant_admin_full_content if {
	allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "admin"},
		"action": "content:delete",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": "t_a"},
	}
}

# --- tenant invites -------------------------------------------------------------

test_owner_creates_invite if {
	allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "owner"},
		"action": "tenant:invite:create",
		"resource": {"type": "tenant", "id": "t_a"},
		"context": {"tenant_id": "t_a"},
	}
}

test_editor_cannot_create_invite if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "editor"},
		"action": "tenant:invite:create",
		"resource": {"type": "tenant", "id": "t_a"},
		"context": {"tenant_id": "t_a"},
	}
}

test_invite_create_bound_to_active_tenant if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "owner"},
		"action": "tenant:invite:create",
		"resource": {"type": "tenant", "id": "t_other"},
		"context": {"tenant_id": "t_a"},
	}
}

test_platform_admin_no_invite_blanket if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": ["admin"], "tenant_role": ""},
		"action": "tenant:invite:create",
		"resource": {"type": "tenant", "id": "t_a"},
		"context": {"tenant_id": "t_a"},
	}
}

test_any_subject_accepts_invite if {
	allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": ""},
		"action": "tenant:invite:accept",
		"resource": {"type": "tenant", "id": "invite"},
		"context": {},
	}
}

# Platform admin who ALSO holds a membership acts through that membership's
# role — allowed via the editor rule, never via blanket.
test_platform_admin_with_editor_membership_uses_membership if {
	allow with input as {
		"subject": {"user_id": "u1", "roles": ["admin"], "tenant_role": "editor"},
		"action": "content:create",
		"resource": {"type": "content", "id": "collection"},
		"context": {"tenant_id": "t_a"},
	}
}

test_platform_admin_with_viewer_membership_cannot_write if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": ["admin"], "tenant_role": "viewer"},
		"action": "content:create",
		"resource": {"type": "content", "id": "collection"},
		"context": {"tenant_id": "t_a"},
	}
}

test_unknown_tenant_role_rejected if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "superuser"},
		"action": "content:read",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": "t_a"},
	}
}

# Fail-closed for tenant-plane verbs without an allow rule (both modes).
# The sample verb must be one that does not exist: content:publish sat here
# until ADR-014 §1 shipped it (2026-08-06), at which point this test would have
# been asserting the opposite of the feature. Swapped, not deleted.
test_unknown_content_verb_denied_even_for_admin if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": ["admin"], "tenant_role": "owner"},
		"action": "content:archive",
		"resource": {"type": "content", "id": "article"},
		"context": {"tenant_id": "t_a"},
	}
}

# platform:tenant:plan:update — platform admin only (billing admin, D7).
test_platform_admin_sets_tenant_plan if {
	allow with input as {
		"subject": {"user_id": "u1", "roles": ["admin"], "tenant_role": ""},
		"action": "platform:tenant:plan:update",
		"resource": {"type": "tenant", "id": "t_a"},
		"context": {},
	}
}

test_non_admin_cannot_set_tenant_plan if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "owner"},
		"action": "platform:tenant:plan:update",
		"resource": {"type": "tenant", "id": "t_a"},
		"context": {"tenant_id": "t_a"},
	}
}

# --- agent credentials (ADR-013) ------------------------------------------------

test_owner_issues_agent_credential if {
	allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "owner"},
		"action": "agent:credential:issue",
		"resource": {"type": "agent_credential", "id": ""},
		"context": {"tenant_id": "t_a"},
	}
}

test_admin_issues_agent_credential if {
	allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "admin"},
		"action": "agent:credential:issue",
		"resource": {"type": "agent_credential", "id": ""},
		"context": {"tenant_id": "t_a"},
	}
}

# An editor may write every entry an agent could — and still may not decide that
# something unattended gets to (ruled 2026-08-06). This is the assertion that
# fails if the three rules are ever folded into content_write_roles.
test_editor_cannot_issue_agent_credential if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "editor"},
		"action": "agent:credential:issue",
		"resource": {"type": "agent_credential", "id": ""},
		"context": {"tenant_id": "t_a"},
	}
}

test_viewer_cannot_list_agent_credentials if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "viewer"},
		"action": "agent:credential:list",
		"resource": {"type": "agent_credential", "id": ""},
		"context": {"tenant_id": "t_a"},
	}
}

test_owner_revokes_agent_credential if {
	allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "owner"},
		"action": "agent:credential:revoke",
		"resource": {"type": "agent_credential", "id": "cred-1"},
		"context": {"tenant_id": "t_a"},
	}
}

test_editor_cannot_revoke_agent_credential if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "editor"},
		"action": "agent:credential:revoke",
		"resource": {"type": "agent_credential", "id": "cred-1"},
		"context": {"tenant_id": "t_a"},
	}
}

# No active tenant: a stale or forged role claim does not stand in for one.
test_agent_credential_needs_active_tenant if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": [], "tenant_role": "owner"},
		"action": "agent:credential:issue",
		"resource": {"type": "agent_credential", "id": ""},
		"context": {"tenant_id": ""},
	}
}

# D12 over the new prefix: minting inside a tenant is a power over that tenant's
# content one indirection away, so the platform blanket must not reach it.
test_platform_admin_no_agent_credential_blanket if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": ["admin"], "tenant_role": ""},
		"action": "agent:credential:issue",
		"resource": {"type": "agent_credential", "id": ""},
		"context": {"tenant_id": "t_a"},
	}
}

# An agent:* verb nobody wrote a rule for fails CLOSED, platform admin included
# — the same property the content: and tenant:invite: prefixes have.
test_unknown_agent_verb_fails_closed if {
	not allow with input as {
		"subject": {"user_id": "u1", "roles": ["admin"], "tenant_role": "owner"},
		"action": "agent:credential:rotate",
		"resource": {"type": "agent_credential", "id": "cred-1"},
		"context": {"tenant_id": "t_a"},
	}
}
