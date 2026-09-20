package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Schema proposals (ADR-013 §3 step 8): an agent files a schema change, a
// person approves it, and the approval is what applies it.
//
// THE FLOW IS NOT THE BOUNDARY. content:schema:write is — the ADR's first draft
// had the proposal endpoint doing that job and it could not, because a caller
// who skips the endpoint and posts to /schema/apply never touches it. What
// lives here is the approval trail: who asked, for what, and who answered.
//
// So an agent holds content:schema:propose and nothing here lets it approve:
// every read and both decisions authorize content:schema:write with an empty
// content type, which is the same untyped call ApplySchema makes and which
// ADR-013 §4 refuses to any agent.

// proposalTTL is how long a filed proposal stays approvable.
//
// Seven days, matching inviteTTL — a proposal is the same shape as an invite:
// something filed for somebody else to act on, which stops meaning what it said
// once it is stale. The number is a constant here rather than a DEFAULT in
// 000037 so there is one copy of it (the 000035 expires_at precedent).
const proposalTTL = 7 * 24 * time.Hour

// proposalListLimit caps the queue, following 000034's LIMIT precedent: what is
// past the cap is HIDDEN, never deleted. A proposal nobody answered is a record
// of a question that went unanswered, and purging it would erase the evidence
// of the thing worth noticing.
const proposalListLimit = 50

// ErrProposalStale is the refusal ADR-013 §3 asks for by name: "核准時必須重跑
// plan 並比對,不一致即拒絕核准".
//
// It fires when re-running the plan at approval time produces anything other
// than what was filed — the schema moved underneath the proposal, or the entry
// counts that decide a guarded step did. Approving anyway would apply a change
// nobody reviewed, which is the entire failure the review exists to prevent.
var ErrProposalStale = apperrors.New(
	"CONTENT_SCHEMA_PROPOSAL_STALE",
	"the schema changed since this was proposed; the plan no longer matches and it must be filed again",
	http.StatusConflict,
)

// ErrProposalExpired is the TTL answer (william ruled 2026-08-06). The row
// survives as history; what expires is the permission to act on it.
var ErrProposalExpired = apperrors.New(
	"CONTENT_SCHEMA_PROPOSAL_EXPIRED",
	"this proposal expired before anyone answered it; file it again",
	http.StatusConflict,
)

// ErrProposalDecided is the concurrency answer: somebody already approved or
// rejected this one.
var ErrProposalDecided = apperrors.New(
	"CONTENT_SCHEMA_PROPOSAL_DECIDED",
	"this proposal was already decided",
	http.StatusConflict,
)

// ErrProposalStepsEmpty is 缺口計畫 5.1's answer to `{"steps": []}`: an empty
// array is not the same request as an absent body. Absent means "approve
// everything" (unchanged pre-5.1 behaviour); an empty array names a selection
// that selects nothing, which is never what an approve call means to do, so it
// is refused rather than silently treated as either extreme.
var ErrProposalStepsEmpty = apperrors.New(
	"CONTENT_PROPOSAL_STEPS_EMPTY",
	"steps was given but empty; omit the field to approve everything, or name at least one step index",
	http.StatusUnprocessableEntity,
)

// ProposalStatusExpired is a DERIVED status, never a stored one (000037). A
// pending row past its deadline reads as expired everywhere it is rendered, so
// the API and the table can never disagree about it — which they would the
// moment expiry needed a sweeper to be written down.
const ProposalStatusExpired = "expired"

// SchemaProposalDTO is one proposal as the API renders it.
type SchemaProposalDTO struct {
	ID string `json:"id"`
	// Status carries the derived value: pending / approved / rejected / expired.
	Status   string          `json:"status"`
	Artifact domain.Artifact `json:"artifact"`
	Prune    bool            `json:"prune"`
	// Plan is the stored plan — the approver's view of what applying this
	// document would do. It is NOT recomputed on read: a queue that showed a
	// freshly computed plan would quietly repair a proposal that had gone stale,
	// and the approver would press a button on a diff that no longer matched
	// what was filed.
	Plan            PlanResult `json:"plan"`
	ProposedBy      *uuid.UUID `json:"proposed_by,omitempty"`
	ProposedByKind  string     `json:"proposed_by_kind"`
	ProposedByAgent *string    `json:"proposed_by_agent,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	ExpiresAt       time.Time  `json:"expires_at"`
	DecidedAt       *time.Time `json:"decided_at,omitempty"`
	DecidedBy       *uuid.UUID `json:"decided_by,omitempty"`
	// AppliedSteps is the indices into Plan.Steps that an approval actually ran
	// (缺口計畫 5.1). Omitted for a pending, rejected or expired row, and for a
	// row approved before this field existed — unrecorded, never empty; see
	// 000053's own comment on the column this is read from.
	AppliedSteps []int `json:"applied_steps,omitempty"`
	// Partial is derived on read, never stored: true when the row is approved
	// and AppliedSteps is a strict subset of what a full approval would have run
	// (Plan.Applicable, computed once at proposal time and never recomputed
	// here — see the doc comment on Plan above for why this reads the stored
	// count rather than a fresh one). False for a full approval and the zero
	// value (omitted) for anything not approved or not recorded.
	Partial bool `json:"partial,omitempty"`
}

func proposalDTO(p *repository.SchemaProposal, now time.Time) (SchemaProposalDTO, error) {
	var art domain.Artifact
	if err := json.Unmarshal(p.Artifact, &art); err != nil {
		return SchemaProposalDTO{}, err
	}
	var plan PlanResult
	if err := json.Unmarshal(p.Plan, &plan); err != nil {
		return SchemaProposalDTO{}, err
	}
	status := p.Status
	if p.Expired(now) {
		status = ProposalStatusExpired
	}
	by := p.ProposedBy
	dto := SchemaProposalDTO{
		ID: p.ID.String(), Status: status, Artifact: art, Prune: p.Prune, Plan: plan,
		ProposedBy: &by, ProposedByKind: p.ProposedByKind, ProposedByAgent: p.ProposedByAgent,
		CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt,
		DecidedAt: p.DecidedAt, DecidedBy: p.DecidedBy,
	}
	if p.AppliedSteps != nil {
		var applied []int
		if err := json.Unmarshal(p.AppliedSteps, &applied); err != nil {
			return SchemaProposalDTO{}, err
		}
		dto.AppliedSteps = applied
		if status == repository.ProposalApproved {
			dto.Partial = len(applied) < plan.Applicable
		}
	}
	return dto, nil
}

// ProposeSchema files an artifact for approval and returns the proposal.
//
// THE STORED PLAN AND THE RETURNED PLAN ARE DIFFERENT VIEWS OF THE SAME
// ARTIFACT, and that is the ruling this method exists to implement (william,
// 2026-08-06). planWith narrows the live side of the diff to an agent's
// whitelist, so an agent's plan omits a delete step for every type it may not
// see. Store that and the approval re-run — which runs in the approver's full
// scope — differs in every tenant with a second content type, and no agent
// proposal is ever approvable.
//
// So the row carries the FULL-SCOPE plan (what applying this document would
// actually do, which is what an approver has to be shown), and the response
// carries the proposer's own narrowed view (which is all §4 lets it see). The
// queue is closed to agents for the same reason: the stored plan names types
// outside the whitelist.
func (s *contentService) ProposeSchema(ctx context.Context, art domain.Artifact, prune bool) (_ SchemaProposalDTO, err error) {
	act := s.activityWrite(ctx, domain.ActivitySchemaPropose, "")
	defer func() { act.finish(ctx, err) }()

	// authorizeArtifact, not authorize: the types are in the document, so an
	// agent's whitelist is enforced against THEM (補裁 E). Passing "" here would
	// refuse every agent the verb was created for — the mistake §3 made once.
	sub, err := s.authorizeArtifact(ctx, ActionContentSchemaPropose, "collection", art)
	if err != nil {
		return SchemaProposalDTO{}, err
	}

	approverView, err := s.planScoped(ctx, sub, art, prune, false)
	if err != nil {
		return SchemaProposalDTO{}, err
	}
	proposerView, err := s.planScoped(ctx, sub, art, prune, true)
	if err != nil {
		return SchemaProposalDTO{}, err
	}

	artJSON, err := json.Marshal(art)
	if err != nil {
		return SchemaProposalDTO{}, err
	}
	planJSON, err := json.Marshal(approverView)
	if err != nil {
		return SchemaProposalDTO{}, err
	}
	// The proposer's view is stored too (000038). Both are computed here already;
	// before 000038 this one was returned and then thrown away, which is why a
	// proposer could never look its own proposal up again — the only view it is
	// allowed to see existed solely in the response to this one call.
	proposerPlanJSON, err := json.Marshal(proposerView)
	if err != nil {
		return SchemaProposalDTO{}, err
	}
	by, kind, agentID := provenanceOf(sub)
	if by == nil {
		// Every caller that reaches here has a responsible user: a delivery
		// credential was refused above, and an agent token carries its principal.
		// Refusing rather than storing NULL keeps "who asked" answerable, which
		// is the only thing this table is for.
		return SchemaProposalDTO{}, apperrors.ErrForbidden
	}
	now := time.Now().UTC()
	rec := &repository.SchemaProposal{
		TenantID: sub.TenantID, Artifact: artJSON, Prune: prune, Plan: planJSON,
		PlanProposer: proposerPlanJSON,
		ProposedBy:   *by, ProposedByKind: kind, ProposedByAgent: agentID,
		ExpiresAt: now.Add(proposalTTL),
	}
	if err := s.repo.CreateSchemaProposal(ctx, rec); err != nil {
		return SchemaProposalDTO{}, err
	}
	dto, err := proposalDTO(rec, now)
	if err != nil {
		return SchemaProposalDTO{}, err
	}
	// After the row is committed, never before: a notification about a proposal
	// that failed to land is a lie, and this call cannot fail the request.
	s.notifyProposalFiled(ctx, sub, rec, approverView)
	// The proposer sees its own scope, never the stored one.
	dto.Plan = proposerView
	return dto, nil
}

// ListSchemaProposals returns the tenant's queue, newest first.
//
// It authorizes content:schema:write — the APPROVER's verb — because the queue
// is the approvers' worklist and the stored plans in it are full-scope. An
// agent is refused twice over: the verb is not on its whitelist, and the call
// names no content type (§4). A proposer that wants to know what it filed has
// the response from ProposeSchema.
func (s *contentService) ListSchemaProposals(ctx context.Context) ([]SchemaProposalDTO, error) {
	sub, err := s.authorize(ctx, ActionContentSchemaWrite, "collection", "")
	if err != nil {
		return nil, err
	}
	rows, err := s.repo.ListSchemaProposals(ctx, sub.TenantID, proposalListLimit)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]SchemaProposalDTO, 0, len(rows))
	for _, r := range rows {
		dto, err := proposalDTO(r, now)
		if err != nil {
			return nil, err
		}
		out = append(out, dto)
	}
	return out, nil
}

// OwnSchemaProposalDTO is one proposal as its PROPOSER may see it, and it is a
// separate type from SchemaProposalDTO rather than a field subset of it because
// the two answer different questions to different audiences. Sharing the struct
// would mean every field added for the queue arrives here too, visible to an
// agent, unless somebody remembers to blank it.
//
// What is deliberately NOT here:
//   - proposed_by / kind / agent: the row is the caller's own by construction.
//   - decided_by: who answered is the queue's information. "Was mine approved"
//     is answerable without naming a person, and §4 is a rule about reach — the
//     narrow version can be widened by a ruling, a leak cannot be taken back.
type OwnSchemaProposalDTO struct {
	ID string `json:"id"`
	// Status carries the same derived value the queue shows: pending / approved
	// / rejected / expired.
	Status   string          `json:"status"`
	Artifact domain.Artifact `json:"artifact"`
	Prune    bool            `json:"prune"`
	// Plan is the PROPOSER's view as stored when the proposal was filed (000038),
	// never the approver's and never recomputed. nil when the row predates
	// 000038 and was filed by an agent.
	Plan *PlanResult `json:"plan"`
	// PlanRecorded separates "no plan stored for this row" from "a plan that
	// changes nothing". Without it both render as an absent or empty object and
	// a client cannot tell a gap in the record from a no-op change.
	PlanRecorded bool      `json:"plan_recorded"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	// DecidedAt is when it was answered, if it was.
	DecidedAt *time.Time `json:"decided_at,omitempty"`
	// AppliedSteps and Partial carry the same meaning SchemaProposalDTO's fields
	// do, and are exposed here for the same reason plan_recorded already is: a
	// proposer that filed something is entitled to know what happened to it,
	// including a partial approval. The indices are into the STORED (approver)
	// plan's Steps, not into Plan above — the two can differ in length when the
	// proposer's whitelist narrowed the diff (ProposeSchema's own comment), so
	// this is deliberately a fact about the row rather than an index into the
	// narrower view rendered alongside it.
	AppliedSteps []int `json:"applied_steps,omitempty"`
	Partial      bool  `json:"partial,omitempty"`
}

// GetOwnSchemaProposal answers "was the one I filed approved yet" for the
// credential that filed it (ADR-013 未解項, 000038).
//
// IT IS A NARROWED SINGLE READ, NOT THE QUEUE OPENED UP. The queue stays closed
// to agents for the reason 補裁 Q gave: its stored plans are full-scope and name
// content types outside the caller's whitelist, so listing it would hand over
// the tenant's type list. This returns one row, matched on the caller's own
// credential, carrying the plan in the caller's own scope.
//
// §4 IS ENFORCED IN TWO PARTS HERE, and the split is forced by where the types
// live: they are inside a row that has not been read yet, so there is nothing
// for the usual chokepoint to check. Passing "" to authorize() would apply the
// untyped rule and refuse every agent — the mistake §3 made once and 補裁 E
// fixed for the methods that take an artifact in the body. This method takes an
// id, so neither existing shape fits.
//
// Part two runs against the CURRENT whitelist rather than the one in force when
// the row was filed, and that is the point of re-checking at all: a credential
// re-minted under the same agent name with a narrower scope must not be able to
// read back the wider proposal its predecessor filed.
func (s *contentService) GetOwnSchemaProposal(ctx context.Context, id uuid.UUID) (_ OwnSchemaProposalDTO, err error) {
	act := s.activityRead(ctx, domain.ActivitySchemaProposalRead, "")
	defer func() { act.finish(ctx, err) }()

	// Part one: subject, tenant, the delivery-credential rule and the verb —
	// everything that does not need the types. The whitelist check is passed as
	// a no-op with its second half named below, so a reader of this call site
	// sees that §4 was deferred rather than dropped.
	sub, err := s.authorizeAgentScope(ctx, ActionContentSchemaPropose, "collection",
		func(authn.Subject) error { return nil })
	if err != nil {
		return OwnSchemaProposalDTO{}, err
	}
	by, kind, agentID := provenanceOf(sub)
	if by == nil {
		// The same refusal ProposeSchema makes: a caller with no responsible user
		// cannot have filed anything, so it can own nothing here either.
		return OwnSchemaProposalDTO{}, apperrors.ErrForbidden
	}
	rec, err := s.repo.GetOwnSchemaProposal(ctx, sub.TenantID, id, *by, kind, agentID)
	if err != nil {
		return OwnSchemaProposalDTO{}, err
	}
	var art domain.Artifact
	if err := json.Unmarshal(rec.Artifact, &art); err != nil {
		return OwnSchemaProposalDTO{}, err
	}
	// Part two: the whitelist, against the types the row actually names.
	if err := refuseArtifactOutsideAgentScope(sub, art); err != nil {
		return OwnSchemaProposalDTO{}, err
	}
	return ownProposalDTO(rec, art, time.Now().UTC())
}

// ownProposalDTO renders one row as its proposer may see it, and is shared by
// the single read and the list so the two cannot drift into showing different
// things about the same row — the drift that matters being the plan: this is
// the one place plan_proposer is read, and a second hand-written copy is where
// somebody eventually reaches for `rec.Plan` because it is the shorter name.
//
// It takes the artifact already parsed rather than parsing it itself, because
// every caller has had to parse it first to run the §4 check against the types
// inside it. Parsing twice would invite the version that checks one document
// and returns another.
func ownProposalDTO(rec *repository.SchemaProposal, art domain.Artifact, now time.Time) (OwnSchemaProposalDTO, error) {
	out := OwnSchemaProposalDTO{
		ID: rec.ID.String(), Artifact: art, Prune: rec.Prune,
		CreatedAt: rec.CreatedAt, ExpiresAt: rec.ExpiresAt, DecidedAt: rec.DecidedAt,
	}
	out.Status = rec.Status
	if rec.Expired(now) {
		out.Status = ProposalStatusExpired
	}
	if rec.PlanProposer != nil {
		var plan PlanResult
		if err := json.Unmarshal(rec.PlanProposer, &plan); err != nil {
			return OwnSchemaProposalDTO{}, err
		}
		out.Plan, out.PlanRecorded = &plan, true
	}
	if rec.AppliedSteps != nil {
		var applied []int
		if err := json.Unmarshal(rec.AppliedSteps, &applied); err != nil {
			return OwnSchemaProposalDTO{}, err
		}
		out.AppliedSteps = applied
		if rec.Status == repository.ProposalApproved {
			// The FULL stored plan, not PlanProposer above: AppliedSteps indexes
			// it, so "strict subset" has to be measured against the same plan the
			// indices name — see the field's doc comment.
			var full PlanResult
			if err := json.Unmarshal(rec.Plan, &full); err != nil {
				return OwnSchemaProposalDTO{}, err
			}
			out.Partial = len(applied) < full.Applicable
		}
	}
	return out, nil
}

// ListOwnSchemaProposals answers "what did I file, and what happened to it" —
// the other half of 000038 (ADR-013 補裁 T).
//
// It exists because the single read is unusable on its own: it takes an id the
// proposer has no way to get back once the response to ProposeSchema is gone.
// 補裁 T opened content:schema:propose to the editor role, so a proposer can
// now file something and then be refused by every surface that could tell it
// what became of it — the queue authorizes content:schema:write and its stored
// plans are full-scope. This is the proposer's own list, and nothing else.
//
// The authorization is GetOwnSchemaProposal's, deliberately identical down to
// the deferred §4 check, and NOT ListSchemaProposals'. Authorizing the queue's
// verb here would mean the one door built for proposers is shut to the role the
// ruling opened proposing to.
func (s *contentService) ListOwnSchemaProposals(ctx context.Context) (_ []OwnSchemaProposalDTO, err error) {
	// The same verb the single read files under. A second one would split "did
	// this credential look at its own proposals" across two rows of the log for
	// no difference an operator cares about.
	act := s.activityRead(ctx, domain.ActivitySchemaProposalRead, "")
	defer func() { act.finish(ctx, err) }()

	// Part one, as in GetOwnSchemaProposal: everything that does not need the
	// types. The whitelist is a no-op here and applied per row below.
	sub, err := s.authorizeAgentScope(ctx, ActionContentSchemaPropose, "collection",
		func(authn.Subject) error { return nil })
	if err != nil {
		return nil, err
	}
	// A credential that is BROKEN rather than merely narrow is refused for the
	// whole request, and this line is what keeps the per-row rule below from
	// swallowing it: an agent token minted with no whitelist at all fails the
	// row filter on every row, and the caller would be handed an empty list —
	// told it had filed nothing, which is a different and false answer. Narrow
	// scope hides rows; a malformed credential is refused.
	if err := refuseUnscopedAgentCredential(sub); err != nil {
		return nil, err
	}
	by, kind, agentID := provenanceOf(sub)
	if by == nil {
		// The same refusal ProposeSchema and GetOwnSchemaProposal make: a caller
		// with no responsible user cannot have filed anything.
		return nil, apperrors.ErrForbidden
	}
	rows, err := s.repo.ListOwnSchemaProposals(ctx, sub.TenantID, *by, kind, agentID, proposalListLimit)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]OwnSchemaProposalDTO, 0, len(rows))
	for _, r := range rows {
		var art domain.Artifact
		if err := json.Unmarshal(r.Artifact, &art); err != nil {
			return nil, err
		}
		// §4 AGAINST THE CURRENT WHITELIST, AND A ROW THAT FAILS IT IS DROPPED
		// RATHER THAN FAILING THE CALL. This is a ruling, not a shortcut.
		//
		// The single read refuses, and is right to: it was asked about one row,
		// so the only answers are that row or nothing. A list is asked about
		// many, and refusing all of them because one is out of scope turns the
		// page off entirely — a credential re-minted under the same agent name
		// with one type dropped would stop being able to see any of its
		// proposals, including the ones still inside its scope. Fail closed per
		// ROW, not per request: what is out of scope is invisible, and what is
		// in scope still answers.
		//
		// Omitted, never rendered blank: a placeholder saying "a proposal you
		// may not see" would leak the count, and the count of an agent's own
		// filings against types since removed from its whitelist is exactly the
		// kind of shape 補裁 Q-2 closed.
		if err := refuseArtifactOutsideAgentScope(sub, art); err != nil {
			continue
		}
		dto, err := ownProposalDTO(r, art, now)
		if err != nil {
			return nil, err
		}
		out = append(out, dto)
	}
	return out, nil
}

func (s *contentService) GetSchemaProposal(ctx context.Context, id uuid.UUID) (SchemaProposalDTO, error) {
	sub, err := s.authorize(ctx, ActionContentSchemaWrite, "collection", "")
	if err != nil {
		return SchemaProposalDTO{}, err
	}
	rec, err := s.repo.GetSchemaProposal(ctx, sub.TenantID, id)
	if err != nil {
		return SchemaProposalDTO{}, err
	}
	return proposalDTO(rec, time.Now().UTC())
}

// resolveStepSelection validates a partial-approval request against the
// STORED plan and returns which indices were selected (缺口計畫 5.1).
//
// steps == nil means the request named none at all — absent or JSON null body
// — and is returned as a nil map: "no explicit selection", the signal
// ApproveSchemaProposal uses to take the untouched pre-5.1 code path. This is
// deliberately a different return value from "selected every index", which a
// caller can still ask for explicitly and which behaves differently (see the
// doc comment on ApproveSchemaProposal): the two are not the same request even
// when they happen to name the same steps.
//
// steps == []int{} is refused rather than treated as either extreme, because
// silently mapping it to one of them would guess at a request that named
// nothing.
//
// An index outside [0, len(plan.Steps)) or repeated is collected into the
// refusal's details rather than failing on the first one, so a caller fixing
// its request sees every problem in one round trip instead of one per retry.
func resolveStepSelection(plan PlanResult, steps []int) (map[int]bool, error) {
	if steps == nil {
		return nil, nil
	}
	if len(steps) == 0 {
		return nil, ErrProposalStepsEmpty
	}
	selected := make(map[int]bool, len(steps))
	var invalid []int
	for _, i := range steps {
		if i < 0 || i >= len(plan.Steps) || selected[i] {
			invalid = append(invalid, i)
			continue
		}
		selected[i] = true
	}
	if len(invalid) > 0 {
		return nil, apperrors.New(
			"CONTENT_PROPOSAL_STEP_INVALID",
			"one or more step indices are out of range for the stored plan, or repeated",
			http.StatusUnprocessableEntity,
		).WithDetails(map[string]any{"invalid": invalid})
	}
	return selected, nil
}

// ApproveSchemaProposal re-runs the plan, refuses if it has moved, applies the
// artifact and records the decision — all in ONE transaction.
//
// The transaction is not an optimisation. Applying and deciding in two of them
// gives two failure states that both lie: a schema changed by a proposal still
// marked pending (approvable a second time), or a proposal marked approved by an
// apply that rolled back.
//
// STEPS SELECTS A SUBSET OF THE STORED PLAN (缺口計畫 5.1). nil is "approve
// everything" and is byte-for-byte the pre-5.1 code path below: the same
// canApply() call, ignoring a skipped destructive step exactly as it always
// has, over the plan exactly as stored. That is the compatibility promise —
// every caller that never learns about steps sees no change at all, including
// one that read the response shape apart before this landed.
//
// An explicit list is judged differently, and the difference is deliberate
// rather than an oversight of the sentence above: canApply() ignores a skipped
// step because skipping an unrequested destructive change and applying the
// rest of the plan around it is the whole point of prune=false. Naming that
// same step's index and asking for it specifically is a different request — it
// says "run this one" — and a caller that lists a step nobody can run is told
// so (409, with the index), not silently given a partial result it did not
// ask for. So a SELECTED step that is refused, blocked, or would otherwise be
// skipped disqualifies the approval; an UNSELECTED one never does, selected or
// not being the only question this path asks.
//
// EXECUTION STAYS ON THE UNCHANGED execute(): what changes is which PlanResult
// it is handed. The stored plan's step order is preserved — DiffSchemas emits
// components before the types and fields that can reference them, which is the
// ordering guarantee a partial selection can BREAK by omitting an earlier step
// a later selected one depends on (a create_component a selected add_field
// needs). execute() does not know about dependencies; the transaction is what
// makes that safe. It fails the one verb that needed the missing piece, every
// earlier verb in the same transaction rolls back with it, and the proposal is
// left pending — exactly as if the whole apply had failed, because as far as
// the schema is concerned it did.
func (s *contentService) ApproveSchemaProposal(ctx context.Context, id uuid.UUID, steps []int) (_ PlanResult, err error) {
	// Recorded as schema.apply, not as a verb of its own: approving IS applying,
	// and the stream's readers care what happened to the schema rather than
	// which door it came through. The proposal row records the door.
	act := s.activityWrite(ctx, domain.ActivitySchemaApply, "")
	defer func() { act.finish(ctx, err) }()

	// The same authorize call ApplySchema makes, deliberately identical: an
	// approval that authorized anything weaker would be a second way to apply a
	// schema change, and the weaker one is the one attackers and mistakes find.
	sub, err := s.authorize(ctx, ActionContentSchemaWrite, "collection", "")
	if err != nil {
		return PlanResult{}, err
	}

	now := time.Now().UTC()
	var result PlanResult
	err = s.repo.WithTx(ctx, sub.TenantID, func(r repository.ContentRepository) error {
		bound := *s
		bound.repo = r
		rec, err := r.GetSchemaProposal(ctx, sub.TenantID, id)
		if err != nil {
			return err
		}
		if rec.Status != repository.ProposalPending {
			return ErrProposalDecided
		}
		if rec.Expired(now) {
			return ErrProposalExpired
		}
		var art domain.Artifact
		if err := json.Unmarshal(rec.Artifact, &art); err != nil {
			return err
		}
		// Full scope, matching what was stored. Comparing an approver-scope
		// re-run against a proposer-scope baseline would compare two different
		// questions — see ProposeSchema. The stale check always runs against
		// this FULL re-run, never the selection below: whether the plan moved
		// is a question about the schema, not about which of its steps a
		// caller happened to ask for.
		plan, err := bound.planScoped(ctx, sub, art, rec.Prune, false)
		if err != nil {
			return err
		}
		same, err := planMatches(rec.Plan, plan)
		if err != nil {
			return err
		}
		if !same {
			return ErrProposalStale
		}

		selected, err := resolveStepSelection(plan, steps)
		if err != nil {
			return err
		}

		// execPlan is what execute() runs and what the response returns. Steps
		// is copied before any mutation below — plan.Steps must not be touched,
		// since plan (not execPlan) is what a retried planMatches elsewhere
		// would compare against.
		execPlan := plan
		execPlan.Steps = append([]PlanStep(nil), plan.Steps...)

		var appliedIdx []int
		if selected == nil {
			// UNCHANGED PATH: no steps named, so this is the pre-5.1 whole-plan
			// approval, verbatim — same check, same plan, same response shape.
			if !plan.canApply() {
				return apperrors.New("CONTENT_SCHEMA_NOT_APPLICABLE", "the artifact cannot be applied as-is", http.StatusConflict).
					WithDetails(map[string]any{"refused": plan.Refused, "blocked": plan.Blocked, "steps": plan.Steps})
			}
			for i, st := range execPlan.Steps {
				if !st.Skipped {
					appliedIdx = append(appliedIdx, i)
				}
			}
		} else {
			// PARTIAL PATH: applicability is judged on the SELECTED subset only.
			var offending []int
			for i, st := range execPlan.Steps {
				if !selected[i] {
					continue
				}
				if st.Grade == domain.GradeRefused || st.Blocked || st.Skipped {
					offending = append(offending, i)
				}
			}
			if len(offending) > 0 {
				return apperrors.New("CONTENT_SCHEMA_NOT_APPLICABLE", "one or more selected steps cannot be applied as-is", http.StatusConflict).
					WithDetails(map[string]any{"steps": offending})
			}
			// Every selected step passed the check above, so marking everything
			// ELSE skipped is exactly "run only the selection" — in the stored
			// plan's own order, never the request's, which is what keeps a
			// create_component ahead of the add_field that needs it.
			for i := range execPlan.Steps {
				if selected[i] {
					appliedIdx = append(appliedIdx, i)
				} else {
					execPlan.Steps[i].Skipped = true
				}
			}
			// Recomputed to be the count that will actually run. Refused and
			// Blocked are left as the full plan's counts — see the doc comment
			// on PlanResult.AppliedSteps for why those two stay put.
			execPlan.Applicable = len(appliedIdx)
		}

		if err := bound.execute(ctx, art, execPlan); err != nil {
			return err
		}

		var appliedJSON []byte
		if appliedIdx != nil {
			appliedJSON, err = json.Marshal(appliedIdx)
			if err != nil {
				return err
			}
		}
		if err := r.DecideSchemaProposal(ctx, sub.TenantID, id,
			repository.ProposalApproved, sub.UserID, now, appliedJSON); err != nil {
			return err
		}
		partial := selected != nil && len(appliedIdx) < plan.Applicable
		act.withDetails(domain.SchemaApplyDetails(id, appliedIdx, partial))
		execPlan.AppliedSteps = appliedIdx
		result = execPlan
		return nil
	})
	if errors.Is(err, repository.ErrProposalNotPending) {
		// Lost the race: another approver decided it between the read above and
		// the UPDATE. The whole transaction rolled back, so nothing was applied.
		return PlanResult{}, ErrProposalDecided
	}
	if err != nil {
		return PlanResult{}, err
	}
	return result, nil
}

// RejectSchemaProposal answers no. It writes no schema, and the row it leaves
// is the whole record of the decision — the activity stream has no verb for it
// because nothing happened to the content.
func (s *contentService) RejectSchemaProposal(ctx context.Context, id uuid.UUID) error {
	sub, err := s.authorize(ctx, ActionContentSchemaWrite, "collection", "")
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	rec, err := s.repo.GetSchemaProposal(ctx, sub.TenantID, id)
	if err != nil {
		return err
	}
	if rec.Status != repository.ProposalPending {
		return ErrProposalDecided
	}
	if rec.Expired(now) {
		return ErrProposalExpired
	}
	// nil: a rejection applies nothing, so there is nothing to name (缺口計畫
	// 5.1) — see the doc comment on repository.DecideSchemaProposal.
	err = s.repo.DecideSchemaProposal(ctx, sub.TenantID, id, repository.ProposalRejected, sub.UserID, now, nil)
	if errors.Is(err, repository.ErrProposalNotPending) {
		return ErrProposalDecided
	}
	return err
}

// planMatches compares a re-run against the plan stored when the proposal was
// filed, in full: every step, and the three counts.
//
// BOTH SIDES GO THROUGH JSON, and that is not ceremony. The stored bytes came
// back out of a JSONB column, which does not preserve key order — comparing
// them to freshly marshalled bytes would fail on documents that are identical.
// Round-tripping the re-run gives the two sides the same normalisation, so what
// is left to differ is what actually differs.
func planMatches(stored []byte, rerun PlanResult) (bool, error) {
	var before PlanResult
	if err := json.Unmarshal(stored, &before); err != nil {
		return false, err
	}
	raw, err := json.Marshal(rerun)
	if err != nil {
		return false, err
	}
	var after PlanResult
	if err := json.Unmarshal(raw, &after); err != nil {
		return false, err
	}
	return reflect.DeepEqual(before, after), nil
}
