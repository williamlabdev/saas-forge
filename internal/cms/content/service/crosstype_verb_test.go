package service

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// The 2026-08-06 narrowing, asserted BEHAVIOURALLY: the stream and the queue
// moved off content:list onto verbs of their own, and the whole point was to
// stop viewer reading them.
//
// EVERY ASSERTION HERE CALLS THE SERVICE. Checking that the constants exist, or
// that the authorizer answers no for the string, would pass just as well with
// the service still authorizing on content:list — the two layers agree on
// everything except the one call that matters. The verb only counts if the
// endpoint asks for it, so the endpoint is what gets asked.
//
// The RBAC authorizer is wired for real (newSvc's allow-all default would make
// every line below vacuous).
func TestCrossTypeReadsUseTheirOwnVerbs(t *testing.T) {
	svc := NewContentService(&memRepo{}, authz.NewRBACAuthorizer(), staticPlan(Quota{}))

	t.Run("a viewer is refused both, which is the entire ruling", func(t *testing.T) {
		viewer := ctxRole("t1", "viewer")

		_, err := svc.ListActivity(viewer, ListActivityInput{})
		require.ErrorIs(t, err, apperrors.ErrForbidden,
			"a viewer holds content:list and must no longer reach the stream through it")

		_, err = svc.ListPendingReview(viewer, ListPendingReviewInput{})
		require.ErrorIs(t, err, apperrors.ErrForbidden,
			"same for the release queue")
	})

	// The other half of the ruling, and the half a narrowing done with the wrong
	// instrument would break: NOBODY BUT VIEWER LOSES ANYTHING. An editor is the
	// person reviewing the agent output the stream exists to explain.
	t.Run("owner, admin and editor keep both", func(t *testing.T) {
		for _, role := range []string{"owner", "admin", "editor"} {
			ctx := ctxRole("t1", role)

			_, err := svc.ListActivity(ctx, ListActivityInput{})
			require.NoError(t, err, "%s reads the stream", role)

			_, err = svc.ListPendingReview(ctx, ListPendingReviewInput{})
			require.NoError(t, err, "%s reads the queue", role)
		}
	})

	// An agent is refused both, and WHICH LAYER REFUSES IT is worth stating
	// precisely, because writing this test corrected the answer.
	//
	// There are two layers, and they do not fire in the order the verbs' own
	// comments first claimed. authorizeAgentScope applies the per-credential
	// whitelist BEFORE it calls the authorizer, so ADR-013 §4's untyped rule —
	// these calls name no content type, therefore an agent may not make them —
	// still gets there first and is what produces the error below. The agent
	// gate's omission of the two verbs is the SECOND layer, not the first.
	//
	// That second layer is not decoration, and its reachable case is named in
	// ADR-013 §A: relaxing the untyped rule is a standing temptation, because
	// that rule is what shut schema:plan out of the very agents it was built
	// for. On the day someone relaxes it, this refusal has to survive, and the
	// gate is what makes it survive. It is verified white-box in the authz
	// package's agentActionExpectations — a black-box test here cannot tell the
	// two layers apart, which is exactly why the layer test exists separately.
	t.Run("an agent credential is refused both", func(t *testing.T) {
		agent := ctxAgent("t1", "editor", uuid.New(), []string{"post"})

		_, err := svc.ListActivity(agent, ListActivityInput{})
		requireRefused(t, err, "the record of what an agent did is for the person answerable for it")

		_, err = svc.ListPendingReview(agent, ListPendingReviewInput{})
		requireRefused(t, err, "an agent must not watch how long its own output sits unreviewed")
	})
}

// requireRefused asserts a 403 without pinning WHICH guard produced it.
//
// The two agent layers return different errors — §4's untyped rule returns a
// named AppError, the agent gate returns ErrForbidden — and an assertion on
// either one turns a change in guard ORDER into a red test that reads like a
// permissions regression. What must hold is that the call is refused; which of
// the two says so is the layer tests' business.
func requireRefused(t *testing.T, err error, msg string) {
	t.Helper()
	require.Error(t, err, msg)
	var appErr *apperrors.AppError
	if errors.As(err, &appErr) {
		require.Equal(t, 403, appErr.HTTPStatus, msg)
		return
	}
	require.ErrorIs(t, err, apperrors.ErrForbidden, msg)
}
