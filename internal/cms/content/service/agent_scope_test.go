package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
)

// ADR-013 §4 (AllowedTypes) and §2 (provenance), at the service chokepoint.
//
// These run under NewAllowAllAuthorizer (newSvc), which is deliberate: it takes
// the VERB decision out of the picture, so every refusal below is provably the
// credential-scope gate and not the RBAC matrix. The verb enumeration is the
// authorizer's half and is tested there (authz/agent_gate_test.go).

// ctxAgent builds the subject a minted agent token parses to: the principal's
// id in both places a person would occupy, plus the four agent fields.
func ctxAgent(tenant, role string, principal uuid.UUID, allowed []string) context.Context {
	agentID := "content-bot"
	return authn.WithSubject(context.Background(), authn.Subject{
		UserID:       principal,
		TenantID:     tenant,
		TenantRole:   role,
		Kind:         authn.ActorKindAgent,
		AgentID:      &agentID,
		PrincipalID:  &principal,
		AllowedTypes: allowed,
	})
}

// seedPostType creates the type as owner so the agent tests can act on
// something that exists.
func seedPostType(t *testing.T, svc ContentService, owner context.Context, names ...string) {
	t.Helper()
	for _, n := range names {
		_, err := svc.CreateContentType(owner, CreateTypeInput{
			Name:   n,
			Label:  n,
			Fields: []FieldInput{{Key: "title", Type: domain.FieldTypeString, Required: true}},
		})
		require.NoError(t, err)
	}
}

// Verification plan item 2. "Not configured" must never read as "everything",
// and the expected code is written here literally rather than read back from
// the error the service produced.
func TestAgentWithoutAWhitelistMayTouchNothing(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")

	principal := uuid.New()
	unset := ctxAgent("t1", "editor", principal, nil)

	_, err := svc.CreateEntry(unset, "post", mustJSON(t, map[string]any{"title": "x"}))
	assert.Equal(t, "CONTENT_AGENT_SCOPE_UNSET", codeOf(t, err))

	_, err = svc.ListEntries(unset, "post", ListEntriesInput{})
	assert.Equal(t, "CONTENT_AGENT_SCOPE_UNSET", codeOf(t, err), "reads are refused too — unset is not read-only, it is nothing")

	// The control: the same credential WITH a whitelist covering the type
	// works. Without this, a service that refused every agent would pass.
	scoped := ctxAgent("t1", "editor", principal, []string{"post"})
	_, err = svc.CreateEntry(scoped, "post", mustJSON(t, map[string]any{"title": "x"}))
	require.NoError(t, err)
}

func TestAgentIsRefusedTypesOutsideItsWhitelist(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post", "invoice")

	agent := ctxAgent("t1", "editor", uuid.New(), []string{"post"})

	_, err := svc.CreateEntry(agent, "invoice", mustJSON(t, map[string]any{"title": "x"}))
	assert.Equal(t, "CONTENT_AGENT_TYPE_NOT_ALLOWED", codeOf(t, err))

	_, err = svc.ListEntries(agent, "invoice", ListEntriesInput{})
	assert.Equal(t, "CONTENT_AGENT_TYPE_NOT_ALLOWED", codeOf(t, err))

	_, err = svc.CreateEntry(agent, "post", mustJSON(t, map[string]any{"title": "x"}))
	require.NoError(t, err, "the whitelisted type must still work, or the refusals above prove nothing")
}

// Verification plan item 3, at the service layer; the HTTP half is in
// test/e2e/agent_credential_test.go, because §4's guarantee is about what a
// caller can reach by talking to the API directly rather than through MCP.
//
// These paths concern no single content type, so they pass "" and an agent is
// refused BY CONSTRUCTION. ADR-013 §A ruled that this must not be relaxed to
// make cms_describe work — GET /types is on this list on purpose.
func TestAgentIsRefusedPathsThatNameNoType(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")
	agent := ctxAgent("t1", "editor", uuid.New(), []string{"post"})

	_, err := svc.ListContentTypes(agent)
	assert.Equal(t, "CONTENT_AGENT_SCOPE_UNTYPED", codeOf(t, err),
		"GET /types names no single type; ADR-013 §A chose to leave this closed and iterate AllowedTypes instead")

	// Media: the ADR-005 upload flow is one of the paths §5 declines to expose
	// as a tool, and §4 is what makes declining it more than a UX choice.
	// "content_type" here is the MIME type of the file, which is not a CMS
	// content type and cannot stand in for one — precisely why these paths pass
	// "" to authorize().
	_, err = svc.CreateMediaUpload(agent, CreateMediaUploadInput{ContentType: "image/png"})
	assert.Equal(t, "CONTENT_AGENT_SCOPE_UNTYPED", codeOf(t, err))

	_, err = svc.GetMediaAsset(agent, uuid.New())
	assert.Equal(t, "CONTENT_AGENT_SCOPE_UNTYPED", codeOf(t, err),
		"refused before the asset is looked up — a 404 here would mean the gate ran too late")

	_, err = svc.ListWebhooks(agent)
	assert.Equal(t, "CONTENT_AGENT_SCOPE_UNTYPED", codeOf(t, err))

	// The control: a human on the same paths is unaffected.
	_, err = svc.ListContentTypes(owner)
	require.NoError(t, err)
	_, err = svc.ListWebhooks(owner)
	require.NoError(t, err)

	// And the single-type read the ruling points cms_describe at still works.
	_, err = svc.GetContentType(agent, "post")
	require.NoError(t, err, "GET /types/{name} is the path ADR-013 §A sends cms_describe to — it must stay open")
}

func TestAgentWithoutAPrincipalIsRefused(t *testing.T) {
	svc, _ := newSvc()
	seedPostType(t, svc, ctxRole("t1", "owner"), "post")

	agentID := "content-bot"
	orphan := authn.WithSubject(context.Background(), authn.Subject{
		UserID:       uuid.New(),
		TenantID:     "t1",
		TenantRole:   "editor",
		Kind:         authn.ActorKindAgent,
		AgentID:      &agentID,
		AllowedTypes: []string{"post"},
	})

	_, err := svc.CreateEntry(orphan, "post", mustJSON(t, map[string]any{"title": "x"}))
	assert.Equal(t, "CONTENT_AGENT_PRINCIPAL_MISSING", codeOf(t, err),
		"no principal means nobody answers for the write and own_only would confine it to an author no row can match")
}

// Verification plan item 1. An agent credential carries its minter's tenant
// role, so the field gate must apply to it exactly as it applies to that
// person — an agent is not a way to write a field its principal may not.
func TestAgentDoesNotBypassFieldPermission(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, salaryTypeInput())
	require.NoError(t, err)
	seeded, err := svc.CreateEntry(owner, "employee", mustJSON(t, map[string]any{"name": "Ada", "salary": 100000}))
	require.NoError(t, err)

	// salary is writable by owner only; this agent was minted by an editor.
	agent := ctxAgent("t1", "editor", uuid.New(), []string{"employee"})
	_, err = svc.UpdateEntry(agent, "employee", seeded.ID, mustJSON(t, map[string]any{"salary": 1}), 0)
	assert.Equal(t, "CONTENT_FIELD_WRITE_FORBIDDEN", codeOf(t, err))
	d := detailsOf(t, err)
	assert.Equal(t, "salary", d["field"])

	// The open field on the same type still writes, so the refusal above is the
	// field gate rather than the agent gate refusing the whole request.
	_, err = svc.UpdateEntry(agent, "employee", seeded.ID, mustJSON(t, map[string]any{"name": "Grace"}), 0)
	require.NoError(t, err)

	// And the restricted field is not readable back through the agent either.
	// Marshalled through mustMarshalData rather than read off DTO.Data: the
	// stripping happens in the projector at marshal time, so an assertion on
	// the struct field would pass whatever the permission layer did.
	got, err := svc.GetEntry(agent, "employee", seeded.ID)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(mustMarshalData(t, got), &payload))
	assert.Contains(t, payload, "name", "the open field must still come back, or the assertion below is vacuous")
	assert.NotContains(t, payload, "salary", "an agent minted by an editor reads exactly what that editor reads")
}

// Verification plan item 6, at the service layer: BOTH directions, because
// each has its own blind spot — a positive-only assertion passes when every
// write is marked as an agent's, and a negative-only one passes when nothing
// is recorded at all.
func TestAgentWritesRecordPrincipalAndAgentProvenance(t *testing.T) {
	svc, repo := newSvc()
	owner := ctxRole("t1", "owner")
	seedPostType(t, svc, owner, "post")

	principal := uuid.New()
	agent := ctxAgent("t1", "editor", principal, []string{"post"})
	created, err := svc.CreateEntry(agent, "post", mustJSON(t, map[string]any{"title": "written by a bot"}))
	require.NoError(t, err)

	var stored *domain.Entry
	for _, e := range repo.entries {
		if e.ID == created.ID {
			stored = e
		}
	}
	require.NotNil(t, stored)

	require.NotNil(t, stored.CreatedBy, "an agent-written row must have an owner, or it becomes the unownable row ADR-013 §2 exists to prevent")
	assert.Equal(t, principal, *stored.CreatedBy, "created_by is the principal — who answers for it, not who typed")
	assert.Equal(t, domain.ActorKindAgent, stored.CreatedByKind)
	require.NotNil(t, stored.CreatedByAgent)
	assert.Equal(t, "content-bot", *stored.CreatedByAgent, "and the row still says which software did the typing")

	// The negative half, on a human write through the same path.
	humanPrincipal := uuid.New()
	human, err := svc.CreateEntry(ctxRoleUser("t1", "editor", humanPrincipal), "post", mustJSON(t, map[string]any{"title": "typed by a person"}))
	require.NoError(t, err)
	for _, e := range repo.entries {
		if e.ID == human.ID {
			stored = e
		}
	}
	assert.Equal(t, domain.ActorKindHuman, stored.CreatedByKind)
	assert.Nil(t, stored.CreatedByAgent, "a human row carrying an agent id is the same bug seen from the other side")
	require.NotNil(t, stored.CreatedBy)
	assert.Equal(t, humanPrincipal, *stored.CreatedBy)
}

// ADR-014 §4. created_by_* answers who AUTHORED a row; this is the half that
// answers who touched it LAST, which is the question the console's every-row
// column actually asks. The scenario is the one the ADR opens with: a person's
// entry that an agent edited afterwards.
//
// At the service layer rather than the repository's, because what is under test
// is that both write paths stamp the trio at all — the repository test sets
// those fields by hand and so cannot notice a service that forgot to.
func TestLastWriteProvenanceFollowsTheWriter(t *testing.T) {
	svc, repo := newSvc()
	person := uuid.New()
	owner := ctxRoleUser("t1", "owner", person)
	seedPostType(t, svc, owner, "post")

	created, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "typed by a person"}))
	require.NoError(t, err)

	stored := func() *domain.Entry {
		t.Helper()
		for _, e := range repo.entries {
			if e.ID == created.ID {
				return e
			}
		}
		t.Fatal("entry vanished from the fake")
		return nil
	}

	// The control: a fresh row's last writer IS its author, so an assertion that
	// only ever saw "agent" below would have nothing to distinguish it from a
	// column stuck on one value.
	require.NotNil(t, stored().UpdatedByKind)
	assert.Equal(t, domain.ActorKindHuman, *stored().UpdatedByKind)
	assert.Nil(t, stored().UpdatedByAgent)

	principal := uuid.New()
	agent := ctxAgent("t1", "editor", principal, []string{"post"})
	_, err = svc.UpdateEntry(agent, "post", created.ID, mustJSON(t, map[string]any{"title": "edited at 10:03"}), 0)
	require.NoError(t, err)

	after := stored()
	require.NotNil(t, after.UpdatedByKind)
	assert.Equal(t, domain.ActorKindAgent, *after.UpdatedByKind, "the last write was a bot's and the row must say so")
	require.NotNil(t, after.UpdatedByAgent)
	assert.Equal(t, "content-bot", *after.UpdatedByAgent)
	require.NotNil(t, after.UpdatedBy)
	assert.Equal(t, principal, *after.UpdatedBy, "and the principal is who answers for it")

	// The half that makes the column worth having: authorship does not follow.
	// Without this, a bug that restamped BOTH pairs on every edit passes.
	assert.Equal(t, domain.ActorKindHuman, after.CreatedByKind, "a later edit does not rewrite who authored the row")
	assert.Nil(t, after.CreatedByAgent)
	require.NotNil(t, after.CreatedBy)
	assert.Equal(t, person, *after.CreatedBy)

	// Publishing is a write too. A person pressing publish over the bot's draft
	// must take the last-write column back, or the console keeps reporting the
	// bot as the most recent actor on a row a human just released.
	_, err = svc.SetEntryStatus(owner, "post", created.ID, domain.StatusPublished, 0)
	require.NoError(t, err)
	published := stored()
	require.NotNil(t, published.UpdatedByKind)
	assert.Equal(t, domain.ActorKindHuman, *published.UpdatedByKind)
	assert.Nil(t, published.UpdatedByAgent, "the agent id must not survive a human's write")
}

// WHITEBOX, and deliberately so. A minted agent token carries the principal as
// its subject, so on every path that exists today Subject.UserID already EQUALS
// PrincipalID — which means actor()'s agent branch cannot be shown to matter by
// any test that goes through minting. Removing it leaves every other test in
// this file green (verified by mutation, 2026-08-05).
//
// The credential below is one minting cannot produce: the two ids differ. It
// pins the RULE ADR-013 §2 settled — created_by is whoever ANSWERS for the
// write, whatever the credential's own subject happens to be — so that the day
// an agent gains an identity of its own, the column does not silently start
// recording an id no admin UI can resolve. That was the exact failure actor()'s
// original comment refused to allow.
func TestActorRecordsThePrincipalEvenIfTheSubjectDiffers(t *testing.T) {
	svc, repo := newSvc()
	seedPostType(t, svc, ctxRole("t1", "owner"), "post")

	principal := uuid.New()
	credentialSubject := uuid.New()
	agentID := "content-bot"
	ctx := authn.WithSubject(context.Background(), authn.Subject{
		UserID:       credentialSubject, // NOT the principal
		TenantID:     "t1",
		TenantRole:   "editor",
		Kind:         authn.ActorKindAgent,
		AgentID:      &agentID,
		PrincipalID:  &principal,
		AllowedTypes: []string{"post"},
	})

	created, err := svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{"title": "x"}))
	require.NoError(t, err)

	var stored *domain.Entry
	for _, e := range repo.entries {
		if e.ID == created.ID {
			stored = e
		}
	}
	require.NotNil(t, stored)
	require.NotNil(t, stored.CreatedBy)
	assert.Equal(t, principal, *stored.CreatedBy, "created_by names who answers for the write")
	assert.NotEqual(t, credentialSubject, *stored.CreatedBy, "recording the credential's own subject is the unresolvable id actor() has always refused")
}

// The same whitebox shape for the read half (ADR-013 §2 amendment): the
// own_only predicate binds the principal, not the credential's subject. Without
// this an agent could not read back what it just wrote, since the write records
// the principal — the two decisions only work as a pair.
func TestOwnOnlyBindsThePrincipalEvenIfTheSubjectDiffers(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRoleUser("t1", "owner", uuid.New())
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name:         "article",
		OwnOnlyRoles: []string{"editor"},
		Fields:       []FieldInput{{Key: "title", Type: domain.FieldTypeString, Required: true}},
	})
	require.NoError(t, err)

	principal := uuid.New()
	agentID := "content-bot"
	agent := authn.WithSubject(context.Background(), authn.Subject{
		UserID:       uuid.New(), // NOT the principal
		TenantID:     "t1",
		TenantRole:   "editor",
		Kind:         authn.ActorKindAgent,
		AgentID:      &agentID,
		PrincipalID:  &principal,
		AllowedTypes: []string{"article"},
	})

	written, err := svc.CreateEntry(agent, "article", mustJSON(t, map[string]any{"title": "by the agent"}))
	require.NoError(t, err)

	back, err := svc.ListEntries(agent, "article", ListEntriesInput{})
	require.NoError(t, err)
	require.Len(t, back.Items, 1, "an agent that cannot read back its own write has a confinement bound to the wrong id")
	assert.Equal(t, written.ID, back.Items[0].ID)

	// And the principal sees it as theirs.
	mine, err := svc.ListEntries(ctxRoleUser("t1", "editor", principal), "article", ListEntriesInput{})
	require.NoError(t, err)
	require.Len(t, mine.Items, 1)
}

// Verification plan item 7 — the gate on ADR-009:238's re-ruling. Without it,
// the whole §2 decision (created_by records the principal) is indistinguishable
// from having left agent writes unowned.
func TestOwnOnlyStillWorksForAgentWrittenRows(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRoleUser("t1", "owner", uuid.New())
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name:         "article",
		OwnOnlyRoles: []string{"editor"},
		Fields:       []FieldInput{{Key: "title", Type: domain.FieldTypeString, Required: true}},
	})
	require.NoError(t, err)

	alice, bob := uuid.New(), uuid.New()
	written, err := svc.CreateEntry(ctxAgent("t1", "editor", alice, []string{"article"}), "article",
		mustJSON(t, map[string]any{"title": "by Alice's agent"}))
	require.NoError(t, err)

	// Alice, as a person, sees what her agent wrote: it is recorded against
	// her, which is the whole point of binding created_by to the principal.
	mine, err := svc.ListEntries(ctxRoleUser("t1", "editor", alice), "article", ListEntriesInput{})
	require.NoError(t, err)
	require.Len(t, mine.Items, 1)
	assert.Equal(t, written.ID, mine.Items[0].ID)
	assert.Equal(t, 1, mustTotal(t, mine), "the count must agree with the page, or the confinement leaks through pagination")

	// Bob does not. The row is owned — it did not become visible to everyone by
	// being authored by software.
	theirs, err := svc.ListEntries(ctxRoleUser("t1", "editor", bob), "article", ListEntriesInput{})
	require.NoError(t, err)
	assert.Empty(t, theirs.Items)
	assert.Equal(t, 0, mustTotal(t, theirs))

	// And Alice's agent reads it back. This is the WIDENING ADR-013 §2's
	// amendment accepted knowingly: the agent's own_only predicate binds the
	// principal, so it also sees rows Alice typed herself.
	byAgent, err := svc.ListEntries(ctxAgent("t1", "editor", alice, []string{"article"}), "article", ListEntriesInput{})
	require.NoError(t, err)
	require.Len(t, byAgent.Items, 1)

	typedByAlice, err := svc.CreateEntry(ctxRoleUser("t1", "editor", alice), "article", mustJSON(t, map[string]any{"title": "typed by Alice"}))
	require.NoError(t, err)
	byAgent, err = svc.ListEntries(ctxAgent("t1", "editor", alice, []string{"article"}), "article", ListEntriesInput{})
	require.NoError(t, err)
	require.Len(t, byAgent.Items, 2,
		"stated cost, not a bug: Alice's agent reads everything Alice wrote in this type (ADR-013 §2 amendment)")

	// Bob's agent still sees neither.
	byBobsAgent, err := svc.ListEntries(ctxAgent("t1", "editor", bob, []string{"article"}), "article", ListEntriesInput{})
	require.NoError(t, err)
	assert.Empty(t, byBobsAgent.Items, "the widening is to the MINTER's rows, not to everyone's")
	_ = typedByAlice
}
