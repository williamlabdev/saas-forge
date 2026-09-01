package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
)

// Apply, at the orchestration layer: which steps run, which are refused, and
// what a second run does.
//
// memRepo cannot roll back — its WithTx runs the callback and keeps the writes —
// so nothing here asserts atomicity. That claim is pinned against Postgres in
// repository/withtx_integration_test.go. What IS asserted here is the logic
// above the transaction: grading, prune gating, and idempotence.

func applySvc(t *testing.T) (ContentService, context.Context) {
	t.Helper()
	return NewContentService(&memRepo{}, authz.NewRBACAuthorizer(), staticPlan(Quota{})), ctxRole("t1", "owner")
}

func art(types ...domain.ArtifactType) domain.Artifact {
	return domain.NewArtifactFromTypes(domain.Artifact{
		ArtifactVersion: domain.ArtifactVersion1, Kind: domain.KindContentSchema, Types: types,
	})
}

func postType(fields ...domain.ArtifactField) domain.ArtifactType {
	return domain.ArtifactType{Name: "post", Label: "Post", Fields: fields}
}

var titleField = domain.ArtifactField{Key: "title", Type: domain.FieldTypeString, Required: true, Label: "Title"}

func TestApplySchema_IsIdempotent(t *testing.T) {
	svc, ctx := applySvc(t)
	a := art(postType(titleField, domain.ArtifactField{Key: "body", Type: domain.FieldTypeText}))

	first, err := svc.ApplySchema(ctx, a, false)
	require.NoError(t, err)
	require.Equal(t, 1, first.Applicable, "a fresh tenant should need exactly one step: create the type")

	// The whole claim of a state document: re-applying it is a no-op. If this
	// ever produces work, the exporter and the differ have drifted apart and
	// every artifact in git is a permanent diff.
	plan, err := svc.PlanSchema(ctx, a, false)
	require.NoError(t, err)
	require.Empty(t, plan.Steps, "re-planning the same artifact produced work: %+v", plan.Steps)

	again, err := svc.ApplySchema(ctx, a, false)
	require.NoError(t, err)
	require.Zero(t, again.Applicable)
}

func TestApplySchema_ExportedArtifactReappliesClean(t *testing.T) {
	svc, ctx := applySvc(t)
	a := art(
		postType(titleField,
			domain.ArtifactField{Key: "state", Type: domain.FieldTypeEnum, EnumValues: []string{"draft", "live"}},
			domain.ArtifactField{Key: "tags", Type: domain.FieldTypeString, Multiple: true},
		),
		domain.ArtifactType{Name: "author", Label: "Author", Fields: []domain.ArtifactField{
			{Key: "name", Type: domain.FieldTypeString, Required: true},
		}},
	)
	_, err := svc.ApplySchema(ctx, a, false)
	require.NoError(t, err)

	// Round-trip through the EXPORT rather than re-using the input: this is
	// what catches a property the writer drops on the way out. A test that
	// re-applies its own input can never see that.
	exported, err := svc.ExportSchema(ctx)
	require.NoError(t, err)
	plan, err := svc.PlanSchema(ctx, exported, false)
	require.NoError(t, err)
	require.Empty(t, plan.Steps, "the exported artifact does not describe the schema it came from: %+v", plan.Steps)
}

func TestApplySchema_DeletionsWaitForPrune(t *testing.T) {
	svc, ctx := applySvc(t)
	full := art(postType(titleField, domain.ArtifactField{Key: "body", Type: domain.FieldTypeText}))
	_, err := svc.ApplySchema(ctx, full, false)
	require.NoError(t, err)

	trimmed := art(postType(titleField))

	// Without prune the missing field means "this document does not describe
	// it", not "delete it". Reported, and skipped.
	plan, err := svc.PlanSchema(ctx, trimmed, false)
	require.NoError(t, err)
	require.Len(t, plan.Steps, 1)
	require.True(t, plan.Steps[0].Skipped, "a dropped field was scheduled for deletion without prune")

	// A SKIPPED STEP IS IN NONE OF THE THREE COUNTERS, and that has to be
	// asserted rather than left as a property of the switch, because a reader
	// downstream cannot see the switch — only the numbers. The console reads
	// this plan (ADR-014 §6) and a summary built from applicable/refused/blocked
	// alone would report "0 changes, nothing blocked" for a plan holding a
	// pending deletion: the reader would be told an artifact does nothing while
	// it is in fact waiting on a prune flag to drop a field.
	require.Equal(t, 0, plan.Applicable+plan.Refused+plan.Blocked,
		"the counters must not silently absorb a skipped step")
	require.Len(t, plan.Steps, 1, "and the step itself is still reported")

	_, err = svc.ApplySchema(ctx, trimmed, false)
	require.NoError(t, err)
	dto, err := svc.GetContentType(ctx, "post")
	require.NoError(t, err)
	require.Len(t, dto.Fields, 2, "apply removed a field that prune had not authorised")

	// With prune it is deliberate, and runs.
	_, err = svc.ApplySchema(ctx, trimmed, true)
	require.NoError(t, err)
	dto, err = svc.GetContentType(ctx, "post")
	require.NoError(t, err)
	require.Len(t, dto.Fields, 1)
}

func TestApplySchema_RefusedArtifactChangesNothing(t *testing.T) {
	svc, ctx := applySvc(t)
	_, err := svc.ApplySchema(ctx, art(postType(titleField)), false)
	require.NoError(t, err)

	// Changing a field's type is refused by ADR-007 whatever the data says, so
	// apply must not perform the OTHER, legal step in the same document either
	// — that is what all-or-nothing means at this layer.
	bad := art(postType(
		domain.ArtifactField{Key: "title", Type: domain.FieldTypeNumber, Required: true, Label: "Title"},
		domain.ArtifactField{Key: "body", Type: domain.FieldTypeText},
	))
	_, err = svc.ApplySchema(ctx, bad, false)
	mustCode(t, err, "CONTENT_SCHEMA_NOT_APPLICABLE", 409)

	dto, err := svc.GetContentType(ctx, "post")
	require.NoError(t, err)
	require.Len(t, dto.Fields, 1, "the legal half of a refused artifact was applied anyway")
	require.Equal(t, domain.FieldTypeString, dto.Fields[0].Type)
}

// Cardinality is decided in two places by two different callers: FieldInput goes
// through buildField, and an ArtifactField goes through addFieldChange. Both ask
// domain.MultipleAllowedFor today, and that is exactly the arrangement worth
// pinning — the failure it guards against is someone inlining the list on one
// side, which drifts silently because each door keeps passing its own tests.
//
// The expectations are SPELLED OUT rather than derived from AllowedMultipleTypes.
// That list is the thing under test; a table built from it agrees with itself no
// matter what it says, which is the vacuous pass this whole suite is written to
// avoid. Adding a field type makes this table fail to compile-or-cover, which is
// the point: the decision gets made once, here, deliberately.
func TestMultipleCardinality_ArtifactDoorAgreesWithTheFieldDoor(t *testing.T) {
	for _, tc := range []struct {
		fieldType string
		allowed   bool
	}{
		{domain.FieldTypeString, true},
		{domain.FieldTypeEnum, true},
		{domain.FieldTypeRelation, true},
		{domain.FieldTypeNumber, true},
		{domain.FieldTypeText, false},
		{domain.FieldTypeBoolean, false},
		{domain.FieldTypeDate, false},
		{domain.FieldTypeDateTime, false},
		{domain.FieldTypeFile, false},
		{domain.FieldTypeRichText, false},
	} {
		t.Run(tc.fieldType, func(t *testing.T) {
			multi := func(key string) FieldInput {
				in := FieldInput{Key: key, Type: tc.fieldType, Multiple: true}
				switch tc.fieldType {
				case domain.FieldTypeEnum:
					in.EnumValues = []string{"a", "b"}
				case domain.FieldTypeRelation:
					in.RelationEntity = "post"
				}
				return in
			}

			// Door one: the field API.
			svc, ctx := applySvc(t)
			_, fieldErr := svc.CreateContentType(ctx, CreateTypeInput{
				Name: "post", Label: "Post", Fields: []FieldInput{multi("xs")},
			})

			// Door two: the same shape arriving as an artifact.
			af := domain.ArtifactField{Key: "xs", Type: tc.fieldType, Label: "Xs", Multiple: true}
			switch tc.fieldType {
			case domain.FieldTypeEnum:
				af.EnumValues = []string{"a", "b"}
			case domain.FieldTypeRelation:
				af.RelationEntity = "post"
			}
			svc2, ctx2 := applySvc(t)
			plan, planErr := svc2.PlanSchema(ctx2, art(postType(titleField, af)), false)
			require.NoError(t, planErr)

			if tc.allowed {
				require.NoError(t, fieldErr, "the field API refused a cardinality the artifact door allows")
				require.Zero(t, plan.Refused, "the artifact door refused a cardinality the field API allows")
				return
			}
			require.Error(t, fieldErr, "the field API allowed a cardinality the artifact door refuses")
			require.Equal(t, "CONTENT_FIELD_MULTIPLE_UNSUPPORTED", codeOf(t, fieldErr))
			require.NotZero(t, plan.Refused, "the artifact door allowed a cardinality the field API refuses")
		})
	}
}

func TestApplySchema_EditorCannotApply(t *testing.T) {
	svc := NewContentService(&memRepo{}, authz.NewRBACAuthorizer(), staticPlan(Quota{}))
	owner := ctxRole("t1", "owner")
	_, err := svc.ApplySchema(owner, art(postType(titleField)), false)
	require.NoError(t, err)

	// content:schema:write, not content:update — apply can delete a type, and
	// the authorisation cannot depend on what a particular document happens to
	// contain, or the caller's own data decides their permissions.
	editor := ctxRole("t1", "editor")
	_, err = svc.ApplySchema(editor, art(postType(titleField)), false)
	require.Error(t, err, "an editor applied a schema artifact")

	// AND PLANS, since 2026-08-30 (ADR-013 補裁 T). This line asserted the
	// opposite until then, on the argument that planning is schema
	// administration; the ruling separated the two because plan writes nothing
	// and exists so that a proposer can read what it is about to propose.
	//
	// Kept in this test rather than moved to one of its own: what makes apply's
	// refusal meaningful is that the SAME caller gets a plan back. Two tests in
	// two files could both be edited to agree and neither would notice.
	_, err = svc.PlanSchema(editor, art(postType(titleField)), false)
	require.NoError(t, err, "an editor was refused a plan — 補裁 T opened content:schema:plan")
}

func TestFromIR_ReportsWhatItCannotCarry(t *testing.T) {
	ir := []byte(`{
	  "version": "0.1", "app_id": "demo", "locale": "zh-TW",
	  "entities": [{
	    "name": "reservation", "label": "Reservation",
	    "fields": [
	      {"key": "guest", "type": "string", "required": true, "description": "who booked"},
	      {"key": "status", "type": "enum", "enum_values": ["draft", "live"]}
	    ],
	    "state_model": {"field": "status", "initial": "draft", "states": ["draft", "live"],
	      "transitions": [{"from_state": "draft", "to_state": "live", "action": "Publish"}]}
	  }],
	  "ui_bindings": {"/book": {"entity": "reservation", "mode": "booking", "access": "public"}}
	}`)
	a, dropped, err := domain.FromIR(ir)
	require.NoError(t, err)
	require.Len(t, a.Types, 1)
	require.Len(t, a.Types[0].Fields, 2)

	// The assertion is on the REPORT, not just on the output. A converter that
	// silently drops half the document also produces a correct-looking
	// artifact; what distinguishes the two is whether the caller was told.
	what := map[string]bool{}
	for _, d := range dropped {
		what[d.What] = true
	}
	for _, want := range []string{"description", "state_model.transitions", "ui_bindings"} {
		require.True(t, what[want], "loss not reported: %s (got %+v)", want, dropped)
	}

	// The state SET survives as the enum it already was; only the transitions
	// are lost.
	require.Equal(t, []string{"draft", "live"}, a.Types[0].Fields[1].EnumValues)
}

func TestFromIR_RefusesATypeTheRuntimeCannotStore(t *testing.T) {
	// `time` is the concrete case: the generator emitted it until 2026-08-01,
	// and an artifact carrying it must fail loudly at the diff rather than be
	// half-applied.
	ir := []byte(`{"entities":[{"name":"booking","fields":[{"key":"slot","type":"time"}]}]}`)
	a, _, err := domain.FromIR(ir)
	require.NoError(t, err, "conversion itself is not where the type set is policed")

	svc, ctx := applySvc(t)
	_, err = svc.ApplySchema(ctx, a, false)
	ae := mustCode(t, err, "CONTENT_SCHEMA_NOT_APPLICABLE", 409)
	require.Equal(t, 1, ae.Details["refused"])
}
