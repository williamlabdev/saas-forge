package authz

import (
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// refuseUnlistedAgentAction applies ADR-013 §1's third invariant: an agent
// credential's actions are allowed by EXPLICIT ENUMERATION, and an unclassified
// verb is refused.
//
// WHY IT LIVES IN THE AUTHORIZER AND NOT IN THE CMS SERVICE. The ADR describes
// the enumeration "in the style of isReadAction", which is a CMS-local
// function, and §4 pins the AllowedTypes check to the CMS chokepoint. But an
// agent token is a downgraded TENANT credential: it carries the minter's
// TenantRole, and Allow is also the gate for the tenant, user, notification,
// ticket, iam and platformops planes. An enumeration living only in the CMS
// service would leave the same token behaving as an ordinary tenant credential
// at, say, tenant:invite:create — the invariant is "reaches nothing the minter
// could not", and every plane is where that has to hold (ruled 2026-08-05).
//
// The list is CMS-only on purpose: those are the verbs ADR-013 §5's tools need,
// and §3 splits schema:plan from schema:write precisely so that plan can appear
// here while apply cannot. Adding a plane later is a deliberate act, which is
// the property the default case buys.
//
// Kind is checked, not AllowedTypes: a credential that is not an agent passes
// through untouched, so this function is invisible to every existing caller.
func refuseUnlistedAgentAction(sub authn.Subject, action string) error {
	if !sub.IsAgent() {
		return nil
	}
	switch action {
	case ActionContentList, ActionContentRead, ActionContentCreate, ActionContentUpdate, ActionContentSchemaPlan,
		ActionContentSchemaPropose:
		return nil
	default:
		// Deliberately NOT enumerated: content:delete (ADR-013 §5 — deletion is
		// blocked on version history), content:schema:write (§3 — apply is the
		// boundary the proposal flow sits behind), content:schema:amend (§B — an
		// agent that may write entries of a type must not thereby reshape the
		// type), content:publish (see below), and every verb of every other
		// plane.
		//
		// content:schema:propose IS listed above, and it does not move that
		// boundary: an agent may file a schema change and may not approve one,
		// the same shape as writing a working copy it cannot publish. The
		// proposal row is a review trail; the refusal that matters is this
		// one, and it holds whether or not a proposal was ever filed.
		//
		// content:publish is THE gate of ADR-014 §1: an agent writes the working
		// copy unattended and a person releases it. It is refused here rather
		// than by omitting a tool, because "the tool list is UX, not
		// authorization" (ADR-013 §支配規則) — the verification plan drives an
		// agent credential at the HTTP endpoint directly, with no tool surface
		// in the path, and this line is what answers it.
		//
		// UNPUBLISH is still allowed, through ActionContentUpdate above. That is
		// not a hole in the gate but its precondition: retract is how an agent
		// stops bad content that is already out, and refusing it would mean the
		// harm continues until a person shows up. It is only safe because
		// ADR-014 §5.1 made retract keep the snapshot — while unpublish cleared
		// it, "rewrite the payload, then unpublish" was a two-step way to change
		// what the public had seen without ever holding this verb.
		//
		// content:activity:read and content:review:list are also absent, and
		// this is the SECOND layer that refuses them — not the first, which is
		// worth writing down because the obvious reading is wrong. The CMS
		// chokepoint applies the per-credential whitelist BEFORE it calls this
		// authorizer, so ADR-013 §4's untyped rule gets there first: the stream
		// and the queue span every content type, so they name none, so an agent
		// is refused a call that names no type. That is still what produces the
		// error today, and a black-box test at the service cannot tell the two
		// apart (the service suite's TestCrossTypeReadsUseTheirOwnVerbs says so
		// and asserts only that the call is refused).
		//
		// This layer is what the refusal RESTS ON rather than what delivers it.
		// §4's rule is the one §A records as a standing temptation to relax — it
		// is what shut schema:plan out of the agents it was built for — and on
		// the day it is relaxed these two lines are the whole answer. Before
		// 2026-08-06 there were no such lines and no verb to write them against.
		//
		// The reason itself is unchanged and worth keeping written down: the
		// record of what an agent did is FOR the person answerable for it, and an
		// agent that could read the stream could see which of its refusals were
		// noticed. The queue is the same shape from the other side — it is the
		// list of an agent's output waiting on a person, and an agent that could
		// read it could watch how long its own work sits unreviewed.
		//
		// SINGLE-ENTRY ATTRIBUTION IS NOT THIS, and points the other way on
		// purpose (ruled in the same breath): GET /entries/{id}/attribution stays
		// open to an agent under the ordinary content read rules. The boundary is
		// what the endpoint ANSWERS — "who wrote this one entry" travels with the
		// entry, "what happened in this tenant" does not.

		return apperrors.ErrForbidden
	}
}
