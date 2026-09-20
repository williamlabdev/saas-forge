package service

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// 缺口計畫 5.1 — schema proposal partial approval. steps=nil keeps the
// pre-5.1 whole-plan behaviour byte-for-byte; a non-nil selection picks a
// subset of the STORED plan's steps by index. These tests run under newSvc's
// AllowAllAuthorizer for the same reason schema_proposal_test.go's do: the
// authz matrix is somebody else's suite, and every refusal below must be
// provably the CMS chokepoint (step validity, applicability, staleness).

// artWithComponent builds a target artifact carrying both a component and
// types, directly — art() (artifact_apply_test.go) only sets Types, because
// every other test in this package predates components. A dynamiczone field
// needs a component in the SAME artifact to grade as additive rather than
// refused (domain.DiffSchemas checks the artifact's own declared components,
// not the live schema — see artifact_diff.go's addFieldChange, which takes
// toComps).
func artWithComponent(comp domain.ArtifactComponent, types ...domain.ArtifactType) domain.Artifact {
	return domain.Artifact{
		ArtifactVersion: domain.ArtifactVersion1,
		Kind:            domain.KindContentSchema,
		Components:      []domain.ArtifactComponent{comp},
		Types:           types,
	}
}

// seededType mirrors exactly what seedPostType wrote (Label == name, a
// "title" field with no Label of its own), plus whatever else the test
// wants. postType()/titleField (artifact_apply_test.go) set a Label on both
// the type and the field, which seedPostType does not — reusing them here
// against a seeded type would add an incidental update_type/update_field
// step to every diff below, and the tests in this file need EXACT step
// indices and counts to make their point.
func seededType(name string, extra ...domain.ArtifactField) domain.ArtifactType {
	fields := append([]domain.ArtifactField{
		{Key: "title", Type: domain.FieldTypeString, Required: true},
	}, extra...)
	return domain.ArtifactType{Name: name, Label: name, Fields: fields}
}

// steps=nil must reproduce the pre-5.1 contract exactly: every applicable
// step runs, the response's AppliedSteps names all of them, and a later read
// does not report the approval as partial.
func TestApproveSchemaProposal_NilStepsAppliesEverythingAndIsNotPartial(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")

	filed, err := svc.ProposeSchema(owner, art(seededType("post", bodyField())), false)
	require.NoError(t, err)

	plan, err := svc.ApproveSchemaProposal(owner, proposalID(t, filed), nil)
	require.NoError(t, err)
	assert.Equal(t, plan.Applicable, len(plan.AppliedSteps),
		"steps=nil must run every applicable step, exactly as before this feature existed")
	for _, i := range plan.AppliedSteps {
		assert.False(t, plan.Steps[i].Skipped, "an index the response says ran must not also be marked skipped")
	}

	after, err := svc.GetSchemaProposal(owner, proposalID(t, filed))
	require.NoError(t, err)
	assert.False(t, after.Partial, "a full approval must never read as partial")
	assert.Equal(t, plan.AppliedSteps, after.AppliedSteps,
		"what the approve response says ran must be what a later read says ran")
}

// A real subset: only the selected step runs, the other is reported skipped
// in the response, and only the selected step's write actually landed.
func TestApproveSchemaProposal_PartialApprovalRunsOnlySelectedSteps(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post", "invoice")

	proposed := art(
		seededType("post", bodyField()),
		seededType("invoice", domain.ArtifactField{Key: "amount", Type: domain.FieldTypeNumber, Label: "Amount"}),
	)
	filed, err := svc.ProposeSchema(owner, proposed, false)
	require.NoError(t, err)
	require.Len(t, filed.Plan.Steps, 2)

	postIdx, invoiceIdx := -1, -1
	for i, st := range filed.Plan.Steps {
		switch {
		case st.Type == "post" && st.Field == "body":
			postIdx = i
		case st.Type == "invoice" && st.Field == "amount":
			invoiceIdx = i
		}
	}
	require.NotEqual(t, -1, postIdx)
	require.NotEqual(t, -1, invoiceIdx)

	plan, err := svc.ApproveSchemaProposal(owner, proposalID(t, filed), []int{postIdx})
	require.NoError(t, err)
	assert.Equal(t, 1, plan.Applicable, "applicable must be recomputed to the number that actually ran")
	assert.Equal(t, []int{postIdx}, plan.AppliedSteps)
	assert.True(t, plan.Steps[invoiceIdx].Skipped, "the unselected step must come back marked skipped")
	assert.False(t, plan.Steps[postIdx].Skipped)

	ct, err := svc.GetContentType(owner, "post")
	require.NoError(t, err)
	var postKeys []string
	for _, f := range ct.Fields {
		postKeys = append(postKeys, f.Key)
	}
	assert.Contains(t, postKeys, "body")

	inv, err := svc.GetContentType(owner, "invoice")
	require.NoError(t, err)
	for _, f := range inv.Fields {
		assert.NotEqual(t, "amount", f.Key, "the unselected step must not have applied")
	}

	after, err := svc.GetSchemaProposal(owner, proposalID(t, filed))
	require.NoError(t, err)
	assert.Equal(t, repository.ProposalApproved, after.Status)
	assert.True(t, after.Partial, "fewer than the plan's applicable steps ran")
	assert.Equal(t, []int{postIdx}, after.AppliedSteps)
}

// The proposer's own view (OwnSchemaProposalDTO) renders Partial against the
// FULL stored plan's Applicable count, not the narrower proposer-scoped plan
// it otherwise shows — AppliedSteps indices are always relative to the full
// plan.
func TestOwnSchemaProposal_ReflectsPartialApprovalAgainstTheFullPlan(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post", "invoice")
	agent := ctxAgent("t1", "editor", uuid.New(), []string{"post", "invoice"})

	proposed := art(
		seededType("post", bodyField()),
		seededType("invoice", domain.ArtifactField{Key: "amount", Type: domain.FieldTypeNumber, Label: "Amount"}),
	)
	filed, err := svc.ProposeSchema(agent, proposed, false)
	require.NoError(t, err)

	postIdx := -1
	for i, st := range filed.Plan.Steps {
		if st.Type == "post" && st.Field == "body" {
			postIdx = i
		}
	}
	require.NotEqual(t, -1, postIdx)

	_, err = svc.ApproveSchemaProposal(owner, proposalID(t, filed), []int{postIdx})
	require.NoError(t, err)

	own, err := svc.GetOwnSchemaProposal(agent, proposalID(t, filed))
	require.NoError(t, err)
	assert.True(t, own.Partial, "the proposer must see partial=true too")
	assert.Equal(t, []int{postIdx}, own.AppliedSteps)
}

// A selected step that the full plan already refused disqualifies the whole
// approval, and the response names which index — not just that something was
// wrong. Nothing runs, including a co-selected step that was individually
// fine.
func TestApproveSchemaProposal_SelectedRefusedStepIs409WithOffendingIndex(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post", "invoice")

	proposed := art(
		seededType("post", bodyField()),
		// No Components: refused (CONTENT_ZONE_COMPONENTS_REQUIRED).
		seededType("invoice", domain.ArtifactField{Key: "zone", Type: domain.FieldTypeDynamicZone, Label: "Zone"}),
	)
	filed, err := svc.ProposeSchema(owner, proposed, false)
	require.NoError(t, err)
	require.Len(t, filed.Plan.Steps, 2)

	okIdx, refusedIdx := -1, -1
	for i, st := range filed.Plan.Steps {
		if st.Grade == domain.GradeRefused {
			refusedIdx = i
		} else {
			okIdx = i
		}
	}
	require.NotEqual(t, -1, refusedIdx)
	require.NotEqual(t, -1, okIdx)

	_, err = svc.ApproveSchemaProposal(owner, proposalID(t, filed), []int{okIdx, refusedIdx})
	require.Error(t, err)
	assert.Equal(t, "CONTENT_SCHEMA_NOT_APPLICABLE", codeOf(t, err))
	assert.Equal(t, 409, statusOf(t, err))
	var ae *apperrors.AppError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, []int{refusedIdx}, ae.Details["steps"])

	ct, err := svc.GetContentType(owner, "post")
	require.NoError(t, err)
	for _, f := range ct.Fields {
		assert.NotEqual(t, "body", f.Key,
			"a co-selected step that was individually fine must not apply when another selected step is refused")
	}
}

// A selected step that the full plan would skip (destructive, no prune)
// disqualifies the whole approval the same way a refused one does.
func TestApproveSchemaProposal_SelectedSkippedStepIs409WithOffendingIndex(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post", "invoice")
	_, err := svc.AddField(owner, "post", FieldInput{Key: "extra", Type: domain.FieldTypeText, Label: "Extra"})
	require.NoError(t, err)

	proposed := art(
		seededType("post"), // "extra" implicitly dropped -> delete_field, destructive
		seededType("invoice", domain.ArtifactField{Key: "amount", Type: domain.FieldTypeNumber, Label: "Amount"}),
	)
	filed, err := svc.ProposeSchema(owner, proposed, false) // prune=false
	require.NoError(t, err)
	require.Len(t, filed.Plan.Steps, 2)

	skippedIdx, additiveIdx := -1, -1
	for i, st := range filed.Plan.Steps {
		if st.Skipped {
			skippedIdx = i
		} else {
			additiveIdx = i
		}
	}
	require.NotEqual(t, -1, skippedIdx, "a destructive change without prune must be planned as skipped")
	require.NotEqual(t, -1, additiveIdx)

	_, err = svc.ApproveSchemaProposal(owner, proposalID(t, filed), []int{additiveIdx, skippedIdx})
	require.Error(t, err)
	assert.Equal(t, "CONTENT_SCHEMA_NOT_APPLICABLE", codeOf(t, err))
	var ae *apperrors.AppError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, []int{skippedIdx}, ae.Details["steps"])
}

// An explicit empty selection is refused rather than silently treated as
// "approve everything" — that reading belongs to steps=nil (absent), not
// steps=[] (present and empty).
func TestApproveSchemaProposal_EmptyStepsIs422(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")
	filed, err := svc.ProposeSchema(owner, art(seededType("post", bodyField())), false)
	require.NoError(t, err)

	_, err = svc.ApproveSchemaProposal(owner, proposalID(t, filed), []int{})
	assert.Equal(t, "CONTENT_PROPOSAL_STEPS_EMPTY", codeOf(t, err))
	assert.Equal(t, 422, statusOf(t, err))
}

// Out-of-range and duplicate indices are both reported, by value, in the
// order they were seen — not just "invalid".
func TestApproveSchemaProposal_InvalidStepIndicesIs422WithOffendingIndices(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")
	filed, err := svc.ProposeSchema(owner, art(seededType("post", bodyField())), false)
	require.NoError(t, err)
	require.Len(t, filed.Plan.Steps, 1)

	_, err = svc.ApproveSchemaProposal(owner, proposalID(t, filed), []int{0, 0, 5, -1})
	assert.Equal(t, "CONTENT_PROPOSAL_STEP_INVALID", codeOf(t, err))
	assert.Equal(t, 422, statusOf(t, err))
	var ae *apperrors.AppError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, []int{0, 5, -1}, ae.Details["invalid"],
		"the repeat of a valid index, the out-of-range index, and the negative index — in the order given")
}

// THE ROLLBACK CLAIM. A selected add_field(zone) depends on an unselected
// create_component(seo): the diff grades it additive (the target artifact
// declares "seo" itself, which is what DiffSchemas checks — see
// artWithComponent's doc comment), so the applicability check lets both
// selected steps through. The dependency only breaks at EXECUTION time, once
// AddField's resolveComponentRefs looks the component up in the live schema
// and does not find it, because the step that would have created it was
// excluded from this approval.
//
// The point of the test is what happens to the EARLIER selected step, which
// by itself would have succeeded: it must roll back too. All-or-nothing is
// only a real guarantee if a partial approval's failure undoes its own
// partial progress, not just the step that failed.
func TestApproveSchemaProposal_PartialApprovalRollsBackOnDependencyFailure(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")

	seo := domain.ArtifactComponent{
		Name:  "seo",
		Label: "SEO",
		Fields: []domain.ArtifactField{
			{Key: "title", Type: domain.FieldTypeString, Label: "Title"},
		},
	}
	proposed := artWithComponent(seo, seededType("post",
		domain.ArtifactField{Key: "summary", Type: domain.FieldTypeText, Label: "Summary"},
		domain.ArtifactField{Key: "zone", Type: domain.FieldTypeDynamicZone, Components: []string{"seo"}, Label: "Zone"},
	))

	filed, err := svc.ProposeSchema(owner, proposed, false)
	require.NoError(t, err)

	// Components diff before types (domain.DiffSchemas: components first),
	// and within a type, fields append in artifact order — so this is
	// [create_component(seo), add_field(summary), add_field(zone)] by
	// construction, not by luck. Asserted rather than assumed, so a diff
	// reordering fails loudly here instead of silently invalidating the
	// scenario below.
	require.Len(t, filed.Plan.Steps, 3)
	require.Equal(t, "seo", filed.Plan.Steps[0].Component, "component creation must diff first")
	require.Equal(t, "summary", filed.Plan.Steps[1].Field)
	require.Equal(t, "zone", filed.Plan.Steps[2].Field)

	// Select summary and zone; exclude the component the zone depends on.
	_, err = svc.ApproveSchemaProposal(owner, proposalID(t, filed), []int{1, 2})
	require.Error(t, err, "zone names a component this approval never created")

	ct, err := svc.GetContentType(owner, "post")
	require.NoError(t, err)
	for _, f := range ct.Fields {
		assert.NotEqual(t, "summary", f.Key,
			"the earlier, individually-successful step must roll back with the later failure")
		assert.NotEqual(t, "zone", f.Key)
	}

	after, err := svc.GetSchemaProposal(owner, proposalID(t, filed))
	require.NoError(t, err)
	assert.Equal(t, repository.ProposalPending, after.Status,
		"execute() failing must never reach the decide step")
}

// planMatches must never see AppliedSteps: ApproveSchemaProposal's stale
// check runs against the plan BEFORE approval sets AppliedSteps on its own
// separate copy (execPlan) — see schema_proposal.go. This test proves why
// that ordering matters, by showing what would happen if it did not: a
// populated AppliedSteps changes what the round-trip comparison sees, because
// nothing about json.Marshal/Unmarshal knows the field is meant to be
// response-only.
func TestPlanMatches_AppliedStepsMustNeverReachTheRerunItCompares(t *testing.T) {
	stored := PlanResult{
		Steps: []PlanStep{{SchemaChange: domain.SchemaChange{
			Op: domain.OpAddField, Type: "post", Field: "body", Grade: domain.GradeAdditive,
		}}},
		Applicable: 1,
	}
	storedBytes, err := json.Marshal(stored)
	require.NoError(t, err)

	withApplied := stored
	withApplied.AppliedSteps = []int{0}
	ok, err := planMatches(storedBytes, withApplied)
	require.NoError(t, err)
	assert.False(t, ok,
		"a populated AppliedSteps changes the round-trip comparison — which is exactly why ApproveSchemaProposal must never pass it in")

	withoutApplied := stored
	ok2, err := planMatches(storedBytes, withoutApplied)
	require.NoError(t, err)
	assert.True(t, ok2, "the actual contract: a rerun with AppliedSteps left unset matches the stored plan")
}
