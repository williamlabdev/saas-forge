package service

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// storedEntry reaches past the projection to the row the fake repository holds,
// because the property under test is about the SNAPSHOT — and the admin DTO
// deliberately does not carry it in a form these assertions could trust.
func storedEntry(t *testing.T, repo *memRepo, id uuid.UUID) *domain.Entry {
	t.Helper()
	for _, e := range repo.entries {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("entry %s is not in the repository", id)
	return nil
}

// ADR-014 §1: a person is the publish gate. An agent writes the working copy
// unattended and cannot put it live.
//
// UNLIKE the rest of this package's agent tests, these run under the REAL RBAC
// authorizer rather than NewAllowAllAuthorizer. That is the whole point: the
// gate IS the verb decision, so a fake that answers yes to everything would
// leave nothing under test. The cost is that a failure here could in principle
// come from the RBAC matrix rather than the agent gate, which is why the human
// control below runs the same call through the same authorizer — if the matrix
// were the cause, the control would fail too.
func newGatedSvc() (ContentService, *memRepo) {
	repo := &memRepo{}
	return NewContentService(repo, authz.NewRBACAuthorizer(), staticPlan(Quota{})), repo
}

// Verification plan item 7, at the service chokepoint. The HTTP half — an agent
// token driven at the endpoint with no tool surface in the path — is
// test/e2e/agent_publish_gate_test.go; that a route reaches this refusal is a
// different claim from this function producing it.
func TestAgentCannotPublish(t *testing.T) {
	svc, _ := newGatedSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")

	principal := uuid.New()
	agent := ctxAgent("t1", "editor", principal, []string{"post"})

	created, err := svc.CreateEntry(agent, "post", mustJSON(t, map[string]any{"title": "written by the agent"}))
	require.NoError(t, err, "the agent must still be able to write the working copy — if it cannot, the refusal below proves nothing about publishing")

	_, err = svc.SetEntryStatus(agent, "post", created.ID, domain.StatusPublished, 0)
	assert.ErrorIs(t, err, apperrors.ErrForbidden, "an agent credential put an entry live")
}

// Verification plan item 8, third bullet: the CONTROL. Without it, a gate that
// refused publish to everybody would pass the test above.
func TestHumanCanPublishWhatTheAgentWrote(t *testing.T) {
	svc, repo := newGatedSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")

	principal := uuid.New()
	agent := ctxAgent("t1", "editor", principal, []string{"post"})
	created, err := svc.CreateEntry(agent, "post", mustJSON(t, map[string]any{"title": "written by the agent"}))
	require.NoError(t, err)

	_, err = svc.SetEntryStatus(owner, "post", created.ID, domain.StatusPublished, 0)
	require.NoError(t, err, "the person the gate exists FOR was refused")

	// §3: the release has to be attributable to the person who pressed it, not
	// to the agent that wrote the content. The two actors are different rows.
	assert.Contains(t, actionsOf(rowsFor(repo, domain.ActorKindHuman)), domain.ActivityEntryPublish,
		"the stream cannot say who released it")
}

// Verification plan item 8, first bullet. Retracting is stopping the bleeding;
// gating it would require the harm to continue until a person arrives
// (ADR-014 §1). An editor role is used for the agent so that a refusal here
// could only come from the gate — the same role publishes fine as a person.
func TestAgentMayStillUnpublish(t *testing.T) {
	svc, _ := newGatedSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")

	principal := uuid.New()
	agent := ctxAgent("t1", "editor", principal, []string{"post"})
	created, err := svc.CreateEntry(agent, "post", mustJSON(t, map[string]any{"title": "live"}))
	require.NoError(t, err)
	// Published by the person, so the agent's retract is a real state change
	// rather than the no-op SetEntryStatus short-circuits on.
	_, err = svc.SetEntryStatus(owner, "post", created.ID, domain.StatusPublished, 0)
	require.NoError(t, err)

	got, err := svc.SetEntryStatus(agent, "post", created.ID, domain.StatusDraft, 0)
	require.NoError(t, err, "the agent could not stop the bleeding")
	assert.Equal(t, domain.StatusDraft, got.Status, "the call succeeded without moving the status")
}

// Verification plan item 8, second bullet — the one the gate is actually FOR.
//
// An agent that may write the working copy of a LIVE entry must not thereby
// change what the public sees. Both halves are asserted together: the snapshot
// alone would not catch a gate that also blocked the working copy, which is the
// failure mode ADR-014 §1 explicitly refuses ("擋在這裡才會讓 agent 退化成填表
// 的"); the working copy alone says nothing about the public.
//
// THE PUBLISH ATTEMPT IN THE MIDDLE IS LOAD-BEARING, and the ADR's wording of
// this bullet leaves it out. Written literally — agent edits a live entry, then
// assert payload moved and published_payload did not — the test passes with the
// gate REMOVED, because an agent's UpdateEntry never touched the snapshot in
// the first place: that is ADR-006's working-copy/snapshot separation, which
// held throughout steps 1-5 and would survive step 6 being reverted. So the
// mutation verification the ADR asks for ("拿掉閘門後指定這一支轉紅") is only
// true of a test that also tries to release. What makes an agent's write reach
// the public is not the write; it is the publish that would follow it.
func TestAgentWriteToALiveEntryNeverReachesThePublic(t *testing.T) {
	svc, repo := newGatedSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")

	principal := uuid.New()
	agent := ctxAgent("t1", "editor", principal, []string{"post"})
	created, err := svc.CreateEntry(agent, "post", mustJSON(t, map[string]any{"title": "as published"}))
	require.NoError(t, err)
	_, err = svc.SetEntryStatus(owner, "post", created.ID, domain.StatusPublished, 0)
	require.NoError(t, err)

	_, err = svc.UpdateEntry(agent, "post", created.ID, mustJSON(t, map[string]any{"title": "agent's later edit"}), 0)
	require.NoError(t, err, "the agent must be able to edit a live entry's working copy")

	// The agent doing everything it can to get that edit out: the entry is
	// already live, so this is the "publish changes" path rather than a first
	// release — and it is the one that would move the snapshot.
	_, err = svc.SetEntryStatus(agent, "post", created.ID, domain.StatusPublished, 0)
	require.ErrorIs(t, err, apperrors.ErrForbidden, "the agent released its own edit")

	stored := storedEntry(t, repo, created.ID)
	assert.JSONEq(t, `{"title":"agent's later edit"}`, string(stored.Payload),
		"the working copy did not take the agent's write")
	// Hard-coded rather than compared against whatever the entry held before:
	// the claim is that the PUBLIC still sees the released text, and reading the
	// expectation back from the row would make a snapshot that moved twice look
	// stationary.
	assert.JSONEq(t, `{"title":"as published"}`, string(stored.PublishedPayload),
		"the agent's write reached the published snapshot — the gate is open")
}
