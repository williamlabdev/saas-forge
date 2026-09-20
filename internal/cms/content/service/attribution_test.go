package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// ADR-014 §6 step 4 — who last changed each field of the entry being released.
//
// The RENDERED strings 驗證計畫第 14 條 asks for are asserted on the web side
// (src/lib/entry-provenance.test.ts in the saas-platform-console repo, ADR-016),
// where the three-state rule has its
// single implementation. What is asserted here is the half that decides what
// those strings are made of: which actor each key resolves to, which keys are in
// the window at all, and which keys never reach the caller.

func seedAttributionType(t *testing.T, svc ContentService, owner context.Context) {
	t.Helper()
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name:  "post",
		Label: "Post",
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "body", Type: domain.FieldTypeText},
			{Key: "summary", Type: domain.FieldTypeText},
		},
	})
	require.NoError(t, err)
}

// TestEntryFieldAttribution_HumanAndAgentInTheSameRelease is 驗證計畫第 14 條's
// scenario at the service layer: a person changed one field and an agent changed
// another since the live snapshot was taken.
//
// `summary` is the control that makes the other two mean something. It was
// written once, BEFORE the release, so it is recorded and yet must not appear:
// without it, an implementation that ignored the window entirely would pass the
// first two assertions and attribute the previous release's work into this one.
func TestEntryFieldAttribution_HumanAndAgentInTheSameRelease(t *testing.T) {
	svc, _ := newSvc()
	person := uuid.New()
	owner := ctxRoleUser("t1", "owner", person)
	seedAttributionType(t, svc, owner)

	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{
		"title": "draft", "body": "draft", "summary": "written before the release",
	}))
	require.NoError(t, err)
	_, err = svc.SetEntryStatus(owner, "post", e.ID, domain.StatusPublished, 0)
	require.NoError(t, err)

	_, err = svc.UpdateEntry(owner, "post", e.ID, mustJSON(t, map[string]any{"title": "by hand"}), 0)
	require.NoError(t, err)

	principal := uuid.New()
	agent := ctxAgent("t1", "editor", principal, []string{"post"})
	_, err = svc.UpdateEntry(agent, "post", e.ID, mustJSON(t, map[string]any{"body": "by bot"}), 0)
	require.NoError(t, err)

	got, err := svc.EntryFieldAttribution(owner, "post", e.ID)
	require.NoError(t, err)

	require.Contains(t, got.Fields, "title")
	assert.Equal(t, domain.ActorKindHuman, got.Fields["title"].ActorKind)
	require.NotNil(t, got.Fields["title"].ActorUserID)
	assert.Equal(t, person, *got.Fields["title"].ActorUserID)
	assert.Nil(t, got.Fields["title"].ActorAgentID, "a human line names no agent")

	require.Contains(t, got.Fields, "body")
	assert.Equal(t, domain.ActorKindAgent, got.Fields["body"].ActorKind)
	require.NotNil(t, got.Fields["body"].ActorAgentID)
	assert.Equal(t, "content-bot", *got.Fields["body"].ActorAgentID)
	require.NotNil(t, got.Fields["body"].ActorUserID,
		"the byline and the accountability are different parties and both must be here")
	assert.Equal(t, principal, *got.Fields["body"].ActorUserID)

	assert.NotContains(t, got.Fields, "summary",
		"its only write is on the other side of the release — attributing it would "+
			"credit the previous release's work to this one's diff")
}

// TestEntryFieldAttribution_UnrecordedFieldIsAbsent is the reverse assertion of
// 驗證計畫第 14 條, at the layer where the fallback would be built rather than
// where it would be rendered.
//
// The entry's own updated_by names a person here, so an implementation reaching
// for it would produce a plausible answer for a field that person never touched.
// The contract is that the key is simply not in the map.
func TestEntryFieldAttribution_UnrecordedFieldIsAbsent(t *testing.T) {
	svc, repo := newSvc()
	person := uuid.New()
	owner := ctxRoleUser("t1", "owner", person)
	seedAttributionType(t, svc, owner)

	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "a"}))
	require.NoError(t, err)
	_, err = svc.SetEntryStatus(owner, "post", e.ID, domain.StatusPublished, 0)
	require.NoError(t, err)
	_, err = svc.UpdateEntry(owner, "post", e.ID, mustJSON(t, map[string]any{"title": "b"}), 0)
	require.NoError(t, err)

	// The write that this console cannot explain: a payload that reached the row
	// without going through a path that names its keys. A schema apply rewriting
	// payloads is the reachable one; here it is done directly so the test does
	// not depend on which verb happens to have that shape today.
	for _, stored := range repo.entries {
		if stored.ID == e.ID {
			stored.Payload = mustJSON(t, map[string]any{"title": "b", "body": "appeared from nowhere"})
		}
	}

	got, err := svc.EntryFieldAttribution(owner, "post", e.ID)
	require.NoError(t, err)
	assert.Contains(t, got.Fields, "title", "the recorded write is still attributed")
	assert.NotContains(t, got.Fields, "body",
		"nobody's write to this field was recorded — the console must say unknown, "+
			"and the one answer it may not give is the entry's own updated_by")
}

// TestEntryFieldAttribution_MasksFieldsTheCallerMayNotRead applies §6's rule
// that both sides of a diff are narrowed identically: a caller who cannot read a
// field does not get answers ABOUT that field either.
func TestEntryFieldAttribution_MasksFieldsTheCallerMayNotRead(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	id := seedEmployee(t, svc, owner)
	_, err := svc.SetEntryStatus(owner, "employee", id, domain.StatusPublished, 0)
	require.NoError(t, err)
	// Only owner may write salary, so the recorded author of it is the owner —
	// which is precisely the line the editor must not be shown.
	_, err = svc.UpdateEntry(owner, "employee", id, mustJSON(t, map[string]any{
		"name": "Ada Lovelace", "salary": 120000,
	}), 0)
	require.NoError(t, err)

	editor, err := svc.EntryFieldAttribution(ctxRole("t1", "editor"), "employee", id)
	require.NoError(t, err)
	assert.Contains(t, editor.Fields, "name", "an unrestricted field is still attributed")
	assert.NotContains(t, editor.Fields, "salary",
		"the caller cannot read this field, so it has no line in their diff to attribute")

	// Control: without it, an endpoint that returned nothing at all would pass.
	asOwner, err := svc.EntryFieldAttribution(owner, "employee", id)
	require.NoError(t, err)
	assert.Contains(t, asOwner.Fields, "salary")
}

// TestEntryFieldAttribution_RefusesTheDeliveryAudience keeps this on the same
// side of the line as published_data and the provenance columns: internal user
// ids and a tenant's private automation topology are admin-only, and a preview
// link goes to people outside the platform.
func TestEntryFieldAttribution_RefusesTheDeliveryAudience(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedAttributionType(t, svc, owner)
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "a"}))
	require.NoError(t, err)
	_, err = svc.SetEntryStatus(owner, "post", e.ID, domain.StatusPublished, 0)
	require.NoError(t, err)

	_, err = svc.EntryFieldAttribution(ctxDelivery("t1"), "post", e.ID)
	require.Error(t, err)
	assert.Equal(t, "FORBIDDEN", codeOf(t, err))

	// The refusal is decided before the entry is looked up, so it cannot be used
	// to tell an id that exists from one that does not.
	_, missing := svc.EntryFieldAttribution(ctxDelivery("t1"), "post", uuid.New())
	require.Error(t, missing)
	assert.Equal(t, codeOf(t, err), codeOf(t, missing),
		"a delivery credential must not learn which ids exist from the difference")
}

// TestEntryFieldAttribution_IsRecorded closes the dominating rule over this
// endpoint: an agent can call it, so the console must be able to say afterwards
// that it did — under its own verb, not filed as a read of the entry.
func TestEntryFieldAttribution_IsRecorded(t *testing.T) {
	svc, repo := newSvc()
	owner := ctxRole("t1", "owner")
	seedAttributionType(t, svc, owner)
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "a"}))
	require.NoError(t, err)

	principal := uuid.New()
	agent := ctxAgent("t1", "editor", principal, []string{"post"})
	_, err = svc.EntryFieldAttribution(agent, "post", e.ID)
	require.NoError(t, err)

	row := lastRow(t, repo)
	assert.Equal(t, domain.ActivityEntryAttribution, row.Action,
		"filing this under entry.read would tell an operator the agent was refused "+
			"the entry when what it asked for was the answer to who did this")
	assert.Equal(t, domain.ActorKindAgent, row.ActorKind)
	assert.Equal(t, domain.ActivityOutcomeSuccess, row.Outcome)
	require.NotNil(t, row.TargetEntryID)
	assert.Equal(t, e.ID, *row.TargetEntryID)
}
