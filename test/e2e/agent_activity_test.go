package e2e_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ADR-014 §驗證計畫第 3 條, over real HTTP: an agent is refused, and the person
// answerable for it can see that the refusal happened.
//
// WHY HERE AND NOT AT THE SERVICE LAYER. The service half is pinned in
// service/activity_test.go and it is not the same claim. §3 exists because the
// 403 went to the agent and nowhere else; "nowhere else" is a statement about
// what a HUMAN can retrieve by asking the API, so the retrieval has to be a real
// request against the real router, the real authorizer and a real database. A
// service-level assertion reads the fake's slice, which proves the row was built
// and nothing about whether anyone can get at it.
//
// The row also has to survive the round trip through Postgres: every CHECK in
// migration 000032 applies to it, and a row the service builds happily but the
// database refuses would look identical at the service layer and be absent here.
func TestE2E_RefusedAgentActionIsVisibleInTheActivityStream(t *testing.T) {
	requireE2E(t)

	_, _, login := registerAndLogin(t, "agtactivity")
	token := login["access_token"].(string)
	human := "Bearer " + token

	for _, name := range []string{"memo", "vault"} {
		rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
			`{"name":"`+name+`","label":"`+name+`","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
			human, "", e2eClientIP(t))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}

	agent := "Bearer " + mintAgent(t, token, "content-bot", []string{"memo"})

	// The refusal. A type the credential was never scoped to.
	rec := doJSON(t, http.MethodPost, "/api/v1/content/entries?type=vault",
		`{"title":"not mine to write"}`, agent, "", e2eClientIP(t))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Equal(t, "CONTENT_AGENT_TYPE_NOT_ALLOWED", decodeErrorCode(t, rec.Body.String()))

	// The control group, and it is not decoration: without a successful action
	// in the stream, a platform that recorded NOTHING would satisfy every
	// assertion about the refusal below by making the whole list empty and
	// letting the search for it fail — which is a red test only if something
	// else is there to be found.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=vault",
		`{"title":"the owner may"}`, human, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodGet, "/api/v1/content/activity", "", human, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	items, ok := decodeDataMap(t, rec)["items"].([]any)
	require.True(t, ok, "the stream answers {items: [...]}")

	var refusal, success map[string]any
	for _, raw := range items {
		row := raw.(map[string]any)
		if row["action"] != "entry.create" || row["target_type"] != "vault" {
			continue
		}
		switch row["outcome"] {
		case "denied":
			refusal = row
		case "success":
			success = row
		}
	}

	require.NotNil(t, refusal, "the agent was refused and the stream cannot say it happened — §3's whole point")
	// Hard-written expectations, not read back from the row: the claim is that
	// these particular facts reach a reader, so asking the response what it
	// contains would assert nothing.
	assert.Equal(t, "denied", refusal["outcome"])
	assert.Equal(t, "CONTENT_AGENT_TYPE_NOT_ALLOWED", refusal["error_code"],
		"a refusal that does not say what refused it is the one thing its reader needs")
	assert.Equal(t, "agent", refusal["actor_kind"])
	assert.Equal(t, "content-bot", refusal["actor_agent_id"], "which bot")
	assert.NotEmpty(t, refusal["actor_user_id"], "and who answers for it — §4 wants both")
	assert.Empty(t, refusal["changed_keys"], "a refusal changed nothing")

	require.NotNil(t, success, "the control: the same action, permitted, is also a row")
	assert.Equal(t, "success", success["outcome"])
	assert.Equal(t, "human", success["actor_kind"])
	assert.Empty(t, success["error_code"])
	assert.Equal(t, "the owner may", success["target_title"],
		"the label the stream is readable by, denormalised at write time")
}

// The stream is for the person answerable for the agent, not for the agent.
// It spans every content type, so it names none, so ADR-013 §4's untyped rule
// shuts an agent out by construction — the same door that keeps it out of media,
// webhooks and usage. Over HTTP because ADR-013's dominating rule is that the
// tool list is UX: what matters is what a caller reaches by asking directly.
func TestE2E_AgentCannotReadTheActivityStream(t *testing.T) {
	requireE2E(t)

	_, _, login := registerAndLogin(t, "agtactread")
	token := login["access_token"].(string)
	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"memo","label":"memo","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		"Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	agent := "Bearer " + mintAgent(t, token, "content-bot", []string{"memo"})
	rec = doJSON(t, http.MethodGet, "/api/v1/content/activity", "", agent, "", e2eClientIP(t))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "CONTENT_AGENT_SCOPE_UNTYPED", decodeErrorCode(t, rec.Body.String()))

	// Control: the minter reaches it, or the refusal above would also hold for
	// an endpoint that is simply broken.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/activity", "", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// One tenant's stream never carries another's rows. RLS and the tenant-scoped
// query are two layers saying the same thing, and this asks the question through
// the front door where both are live.
func TestE2E_ActivityStreamIsTenantScoped(t *testing.T) {
	requireE2E(t)

	_, _, loginA := registerAndLogin(t, "actscopea")
	tokenA := loginA["access_token"].(string)
	_, _, loginB := registerAndLogin(t, "actscopeb")
	tokenB := loginB["access_token"].(string)

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"onlymine","label":"onlymine","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		"Bearer "+tokenA, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodGet, "/api/v1/content/activity", "", "Bearer "+tokenA, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	mine, _ := decodeDataMap(t, rec)["items"].([]any)
	require.NotEmpty(t, mine, "tenant A did something, so its own stream must show it")

	rec = doJSON(t, http.MethodGet, "/api/v1/content/activity", "", "Bearer "+tokenB, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	theirs, _ := decodeDataMap(t, rec)["items"].([]any)
	for _, raw := range theirs {
		assert.NotEqual(t, "onlymine", raw.(map[string]any)["target_type"],
			"tenant B is reading tenant A's activity")
	}
}

// ADR-014 §驗證計畫第 14 條 over real HTTP: the diff's per-field authorship.
//
// The service half is pinned in service/attribution_test.go against the fake,
// and the SQL half in repository/activity_integration_test.go. Neither is this
// claim. The fold that answers "who last changed this field" is a DISTINCT ON
// over unnest(changed_keys) — a shape the Go fake reimplements rather than
// exercises — and the answer only matters if a caller holding an ordinary
// console credential can actually retrieve it from the mounted route.
func TestE2E_DiffAttributesEachFieldToItsLastWriter(t *testing.T) {
	requireE2E(t)

	_, _, login := registerAndLogin(t, "fieldattr")
	token := login["access_token"].(string)
	human := "Bearer " + token

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"brief","label":"brief","fields":[`+
			`{"key":"title","type":"text","label":"T","required":true},`+
			`{"key":"body","type":"text","label":"B"},`+
			`{"key":"summary","type":"text","label":"S"}]}`,
		human, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=brief",
		`{"title":"draft","body":"draft","summary":"settled before the release"}`,
		human, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id, _ := decodeDataMap(t, rec)["id"].(string)
	require.NotEmpty(t, id)

	// The release the diff is measured against.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries/"+id+"/publish?type=brief",
		"", human, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// One field by hand, one by a bot — the two bylines §6 has to keep apart.
	rec = doJSON(t, http.MethodPatch, "/api/v1/content/entries/"+id+"?type=brief",
		`{"title":"edited by hand"}`, human, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	agent := "Bearer " + mintAgent(t, token, "content-bot", []string{"brief"})
	rec = doJSON(t, http.MethodPatch, "/api/v1/content/entries/"+id+"?type=brief",
		`{"body":"edited by bot"}`, agent, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries/"+id+"/attribution?type=brief",
		"", human, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	fields, ok := decodeDataMap(t, rec)["fields"].(map[string]any)
	require.True(t, ok, "the response answers {fields: {...}}")

	title, ok := fields["title"].(map[string]any)
	require.True(t, ok, "the field a person changed has an author")
	assert.Equal(t, "human", title["actor_kind"])
	assert.NotEmpty(t, title["actor_user_id"])
	assert.Nil(t, title["actor_agent_id"], "a human line names no agent")

	body, ok := fields["body"].(map[string]any)
	require.True(t, ok, "the field the agent changed has an author")
	assert.Equal(t, "agent", body["actor_kind"])
	assert.Equal(t, "content-bot", body["actor_agent_id"], "which bot")
	assert.NotEmpty(t, body["actor_user_id"],
		"and who answers for it — a byline with nobody accountable is half a fact")

	// The window. `summary` was written, and recorded, on the far side of the
	// release; crediting it here would sign this release's diff with work that
	// went live in the last one.
	assert.NotContains(t, fields, "summary",
		"attribution covers changes since the live snapshot, not the entry's whole history")
}
