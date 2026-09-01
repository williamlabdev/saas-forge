package service

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// ADR-013 補裁 E: the whole-schema artifact paths, where the content types
// arrive in the BODY and §4's parameter-based gate therefore cannot see them.
//
// These run under newSvc's AllowAllAuthorizer for the reason agent_scope_test.go
// gives: it removes the verb enumeration from the picture, so every refusal
// below is provably the credential-scope gate rather than the RBAC matrix.

func invoiceType() domain.ArtifactType {
	return domain.ArtifactType{Name: "invoice", Label: "Invoice", Fields: []domain.ArtifactField{titleField}}
}

func statusOf(t *testing.T, err error) int {
	t.Helper()
	require.Error(t, err)
	var ae *apperrors.AppError
	require.ErrorAs(t, err, &ae)
	return ae.HTTPStatus
}

// The guard, both directions. The expected code and the expected type name are
// written here as literals; asking the service which type it rejected would make
// this pass against a gate that rejected the wrong one.
func TestAgentPlanIsRefusedAnArtifactOutsideItsWhitelist(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post", "invoice")

	agent := ctxAgent("t1", "editor", uuid.New(), []string{"post"})

	_, err := svc.PlanSchema(agent, art(postType(titleField), invoiceType()), false)
	assert.Equal(t, "CONTENT_AGENT_TYPE_NOT_ALLOWED", codeOf(t, err),
		"one type outside the whitelist refuses the whole document — a partly-planned artifact is not a plan for what was submitted")
	assert.Equal(t, "invoice", detailsOf(t, err)["content_type"])

	// The positive control. Without it, a service that refused every agent's
	// plan would pass the assertion above — which is exactly the state 補裁 E
	// was written to leave behind.
	_, err = svc.PlanSchema(agent, art(postType(titleField)), false)
	require.NoError(t, err, "an artifact wholly inside the whitelist must plan; this is the verb §3 created for agents")

	// And a human is unaffected by any of it.
	_, err = svc.PlanSchema(owner, art(postType(titleField), invoiceType()), false)
	require.NoError(t, err)
}

// The credential-level refusals reach the artifact path too, and in the same
// order: a credential with no scope at all is answered as unscoped, not as
// "that type is not yours".
func TestAgentPlanRefusesAnUnscopedCredentialBeforeLookingAtTheArtifact(t *testing.T) {
	svc, _ := newSvc()
	seedPostType(t, svc, ctxRole("t1", "owner"), "post")

	unset := ctxAgent("t1", "editor", uuid.New(), nil)
	_, err := svc.PlanSchema(unset, art(postType(titleField)), false)
	assert.Equal(t, "CONTENT_AGENT_SCOPE_UNSET", codeOf(t, err),
		"an artifact full of allowed-looking types must not talk a whitelist-less credential through the gate")
}

// The half the ruling did not cover, and the reason visibleToAgent exists.
//
// The gate above only reads the SUBMITTED document. The plan is a diff against
// the live schema, and DiffSchemas emits a delete step for every live type the
// document omits — so a perfectly in-scope artifact used to come back naming
// every other content type in the tenant. That is the list GET /types is closed
// to an agent for (§A), arriving through the door §4 had just been extended to
// cover.
func TestAgentPlanDoesNotNameTypesOutsideItsWhitelist(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post", "invoice")

	inScope := art(postType(titleField))

	agentPlan, err := svc.PlanSchema(ctxAgent("t1", "editor", uuid.New(), []string{"post"}), inScope, true)
	require.NoError(t, err)
	for _, step := range agentPlan.Steps {
		assert.NotEqual(t, "invoice", step.Type,
			"the plan named a type the credential may not touch: %+v", step)
	}

	// THE CONTROL, and it is what makes the loop above mean anything: the same
	// artifact planned by a human DOES produce that step, so the channel is real
	// and the assertion is not passing because plans are empty here.
	ownerPlan, err := svc.PlanSchema(owner, inScope, true)
	require.NoError(t, err)
	var sawInvoice bool
	for _, step := range ownerPlan.Steps {
		if step.Type == "invoice" {
			sawInvoice = true
		}
	}
	require.True(t, sawInvoice,
		"the owner's plan of the same artifact must mention invoice, or this test proves nothing about the agent's")
}

// STRUCTURAL. The cost 補裁 E accepted is a second enforcement point, and the
// failure mode that comes with it is the one §A named: the next endpoint that
// takes an artifact quietly not having the check.
//
// So the method list is DERIVED from the interface rather than written down.
// Add a method that takes a domain.Artifact and this test calls it with an
// out-of-scope document; it fails until that method refuses.
//
// What it does NOT prove: which gate did the refusing. ApplySchema satisfies it
// via §4's untyped rule (it still passes ""), and a new method that did the same
// would pass here while being dead to agents rather than scoped for them. That
// collision is a usability bug, not a reach bug, and it is loud — the endpoint
// simply does not work. This test is aimed at the silent failure: an artifact
// method that answers.
func TestEveryArtifactTakingMethodRefusesAnOutOfScopeAgent(t *testing.T) {
	svc, _ := newSvc()
	seedPostType(t, svc, ctxRole("t1", "owner"), "post", "invoice")

	agent := ctxAgent("t1", "editor", uuid.New(), []string{"post"})
	outOfScope := art(invoiceType())

	var (
		ctxType  = reflect.TypeOf((*context.Context)(nil)).Elem()
		artType  = reflect.TypeOf(domain.Artifact{})
		boolType = reflect.TypeOf(false)
		errType  = reflect.TypeOf((*error)(nil)).Elem()
	)

	iface := reflect.TypeOf((*ContentService)(nil)).Elem()
	var checked []string
	for i := range iface.NumMethod() {
		m := iface.Method(i)
		// Matched on the type NAME, not on equality with artType: a method
		// taking *domain.Artifact or []domain.Artifact is just as much an
		// artifact endpoint, and must land in the arg-building switch below and
		// fail loudly rather than be skipped as "not an artifact method".
		takesArtifact := false
		for j := range m.Type.NumIn() {
			if strings.Contains(m.Type.In(j).String(), "domain.Artifact") {
				takesArtifact = true
			}
		}
		if !takesArtifact {
			continue
		}

		args := make([]reflect.Value, m.Type.NumIn())
		for j := range m.Type.NumIn() {
			switch in := m.Type.In(j); in {
			case ctxType:
				args[j] = reflect.ValueOf(agent)
			case artType:
				args[j] = reflect.ValueOf(outOfScope)
			case boolType:
				args[j] = reflect.ValueOf(false)
			default:
				t.Fatalf("%s takes an artifact and a %s this test cannot build — extend the switch, do not exempt the method", m.Name, in)
			}
		}

		out := reflect.ValueOf(svc).MethodByName(m.Name).Call(args)
		last := out[len(out)-1]
		require.True(t, last.Type().Implements(errType), "%s does not return an error last", m.Name)
		require.False(t, last.IsNil(), "%s let an agent submit an artifact naming a type outside its whitelist", m.Name)
		assert.Equal(t, 403, statusOf(t, last.Interface().(error)), "%s refused, but not as a permission decision", m.Name)
		checked = append(checked, m.Name)
	}

	// The list is derived, so this is the only place the expectation is pinned:
	// both known artifact verbs were actually exercised. Without it, a matcher
	// that silently matched nothing would report success.
	assert.Contains(t, checked, "PlanSchema")
	assert.Contains(t, checked, "ApplySchema")
}
