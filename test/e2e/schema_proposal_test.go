package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ADR-013 §3 step 8 over HTTP: an agent files a schema change, a person
// approves it, and the applying is the approval.
//
// WHY OVER HTTP, and not only at the service. The dominating rule of ADR-013 is
// that the tool list is UX and not an authorization boundary — so what has to be
// shown is a caller holding an agent token and a URL. Everything the service
// suite cannot see is in the path here: the routes, the token's downgrade to an
// agent subject, the real authorizer with its verb table and its whitelist, and
// Postgres with the constraints and the RLS.
func TestE2E_AgentProposesAndAPersonApproves(t *testing.T) {
	requireE2E(t)

	_, _, login := registerAndLogin(t, "schemaprop")
	token := login["access_token"].(string)
	human := "Bearer " + token

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"memo","label":"memo","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		human, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// A SECOND TYPE, and it is the point of this test rather than scenery. An
	// agent's plan is narrowed to its whitelist, so a stored proposer-view plan
	// would differ from the approver's re-run by exactly the delete step for
	// this type — and the approval below would fail. That is the failure mode
	// the ruling of 2026-08-06 removes, and without `ledger` here the test
	// passes whichever view is stored.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"ledger","label":"ledger","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		human, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	agent := "Bearer " + mintAgent(t, token, "schema-bot", []string{"memo"})

	// The document: memo plus a new field. Only types the credential may touch,
	// or the artifact gate refuses it before any of this is reached.
	document := `{"artifact_version":"1","kind":"content-schema","types":[
		{"name":"memo","label":"memo","fields":[
			{"key":"title","type":"text","label":"T","required":true},
			{"key":"body","type":"richtext","label":"Body"}]}]}`

	// THE AGENT MAY NOT APPLY. Asserted first, because everything after it is
	// only interesting if this holds: the proposal flow is a review trail, and
	// the refusal it rests on is this one.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/schema/apply", document, agent, "", e2eClientIP(t))
	require.Equal(t, http.StatusForbidden, rec.Code,
		"an agent reached /schema/apply — the proposal flow is not what stops it, this verb is: %s", rec.Body)

	// It may file.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/schema/proposals", document, agent, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	filed := decodeDataMap(t, rec)
	id, _ := filed["id"].(string)
	require.NotEmpty(t, id)
	assert.Equal(t, "pending", filed["status"])
	assert.Equal(t, "agent", filed["proposed_by_kind"])
	assert.Equal(t, "schema-bot", filed["proposed_by_agent"])

	// What came back is the PROPOSER's view: no step names a type outside the
	// whitelist. The stored plan is the approver's — that is what makes the
	// approval below possible — and this is what keeps §4's reach guarantee
	// while it does.
	plan, _ := filed["plan"].(map[string]any)
	require.NotNil(t, plan)
	steps, _ := plan["steps"].([]any)
	for _, s := range steps {
		step, _ := s.(map[string]any)
		assert.NotEqual(t, "ledger", step["type"],
			"the proposer's copy named a type its credential may not see: %v", step)
	}

	// The queue and the decisions are closed to it, both of them. Reading is
	// refused for the same reason approving is: the stored plan names every type
	// in the tenant.
	for _, call := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/content/schema/proposals"},
		{http.MethodGet, "/api/v1/content/schema/proposals/" + id},
		{http.MethodPost, "/api/v1/content/schema/proposals/" + id + "/approve"},
		{http.MethodPost, "/api/v1/content/schema/proposals/" + id + "/reject"},
	} {
		rec = doJSON(t, call.method, call.path, "", agent, "", e2eClientIP(t))
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"%s %s answered an agent: %s", call.method, call.path, rec.Body)
	}

	// The person sees it, with the full-scope plan.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/schema/proposals", "", human, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	queue, _ := decodeDataMap(t, rec)["proposals"].([]any)
	require.Len(t, queue, 1)
	stored, _ := queue[0].(map[string]any)
	storedPlan, _ := stored["plan"].(map[string]any)
	storedSteps, _ := storedPlan["steps"].([]any)
	var sawLedger bool
	for _, s := range storedSteps {
		step, _ := s.(map[string]any)
		if step["type"] == "ledger" {
			sawLedger = true
		}
	}
	require.True(t, sawLedger,
		"the stored plan is not the approver's view — without the ledger step this test cannot tell the two apart")

	// And approves it. This is the assertion that fails if the stored view is
	// the proposer's: the re-run would not match and the answer would be 409.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/schema/proposals/"+id+"/approve", "", human, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code,
		"approving an agent's proposal in a tenant with a second type must work: %s", rec.Body)

	// The schema really changed — a green status code is not the claim.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/types/memo", "", human, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	fields, _ := decodeDataMap(t, rec)["fields"].([]any)
	var sawBody bool
	for _, f := range fields {
		field, _ := f.(map[string]any)
		if field["key"] == "body" {
			sawBody = true
		}
	}
	assert.True(t, sawBody, "the approved change did not land on the type")

	// Spent. A second approval must not run the artifact again.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/schema/proposals/"+id+"/approve", "", human, "", e2eClientIP(t))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, "CONTENT_SCHEMA_PROPOSAL_DECIDED", proposalErrorCode(t, rec))
}

// The proposer's own read over real HTTP with a real minted credential
// (ADR-013 未解項, 000038). The service suite proves the narrowing and the
// ownership match; only this layer proves an agent TOKEN reaches the route at
// all — the verb, the artifact gate and the router all sit between them.
func TestE2E_AgentReadsItsOwnProposalButNotASiblingsAndSeesTheDecision(t *testing.T) {
	requireE2E(t)

	_, _, login := registerAndLogin(t, "ownprop")
	token := login["access_token"].(string)
	human := "Bearer " + token

	for _, name := range []string{"memo", "ledger"} {
		rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
			`{"name":"`+name+`","label":"`+name+`","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
			human, "", e2eClientIP(t))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}

	// TWO agents, ONE person. Both credentials carry the same principal id —
	// that is what makes the refusal below a real test rather than a tautology.
	filer := "Bearer " + mintAgent(t, token, "memo-bot", []string{"memo"})
	sibling := "Bearer " + mintAgent(t, token, "ledger-bot", []string{"ledger"})

	document := `{"artifact_version":"1","kind":"content-schema","types":[
		{"name":"memo","label":"memo","fields":[
			{"key":"title","type":"text","label":"T","required":true},
			{"key":"body","type":"richtext","label":"Body"}]}]}`

	rec := doJSON(t, http.MethodPost, "/api/v1/content/schema/proposals", document, filer, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id, _ := decodeDataMap(t, rec)["id"].(string)
	require.NotEmpty(t, id)

	// The filer can look it up, and what it gets back is its own scope.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/schema/proposals/mine/"+id, "", filer, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	own := decodeDataMap(t, rec)
	assert.Equal(t, "pending", own["status"])
	assert.Equal(t, true, own["plan_recorded"], "a proposal filed after 000038 carries the proposer's view")
	ownPlan, _ := own["plan"].(map[string]any)
	require.NotNil(t, ownPlan)
	for _, s := range asSlice(ownPlan["steps"]) {
		step, _ := s.(map[string]any)
		assert.NotEqual(t, "ledger", step["type"],
			"the proposer's own read named a type its credential may not see: %v", step)
	}
	// Who decided is the queue's information, not the proposer's.
	_, hasDecider := own["decided_by"]
	assert.False(t, hasDecider, "the proposer's view named the approver: %v", own)

	// THE CLAIM. Same person, same tenant, same verb — a different credential.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/schema/proposals/mine/"+id, "", sibling, "", e2eClientIP(t))
	assert.Equal(t, http.StatusNotFound, rec.Code,
		"a sibling agent of the same principal read another credential's proposal: %s", rec.Body)

	// The queue stays shut to the filer — the new door did not widen the old one.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/schema/proposals", "", filer, "", e2eClientIP(t))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// THE LIST, which is what makes the read above reachable at all: the id came
	// from the POST, and once that response is gone this route is the only way
	// back to it (ADR-013 補裁 T). Over HTTP because the service suite cannot see
	// the route or the token's downgrade to an agent subject.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/schema/proposals/mine", "", filer, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	listed := asSlice(decodeDataMap(t, rec)["proposals"])
	require.Len(t, listed, 1, "the filer's own list did not return the one row it filed: %s", rec.Body)
	row, _ := listed[0].(map[string]any)
	assert.Equal(t, id, row["id"])
	assert.Equal(t, true, row["plan_recorded"])
	rowPlan, _ := row["plan"].(map[string]any)
	require.NotNil(t, rowPlan, "the listed row carried no plan: %v", row)
	for _, s := range asSlice(rowPlan["steps"]) {
		step, _ := s.(map[string]any)
		assert.NotEqual(t, "ledger", step["type"],
			"the list served the stored approver plan, which names types the credential may not see: %v", step)
	}
	_, listedDecider := row["decided_by"]
	assert.False(t, listedDecider, "the listed row named the approver: %v", row)

	// The sibling's list is EMPTY rather than the filer's. Same person, same
	// tenant, same verb — ownership is the CREDENTIAL.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/schema/proposals/mine", "", sibling, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, asSlice(decodeDataMap(t, rec)["proposals"]),
		"a sibling agent of the same principal listed another credential's proposal: %s", rec.Body)

	// And the whole point: after a person answers, the proposer can find out.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/schema/proposals/"+id+"/approve", "", human, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodGet, "/api/v1/content/schema/proposals/mine/"+id, "", filer, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "approved", decodeDataMap(t, rec)["status"],
		"the proposer still reads pending after the proposal was approved")

	// The list carries the decision too, so a proposer polling one endpoint
	// rather than one per id learns the same thing.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/schema/proposals/mine", "", filer, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	listed = asSlice(decodeDataMap(t, rec)["proposals"])
	require.Len(t, listed, 1)
	row, _ = listed[0].(map[string]any)
	assert.Equal(t, "approved", row["status"])
}

// asSlice keeps the plan-step loops above readable; a missing or wrongly typed
// steps key yields no iterations rather than a panic, and the assertions that
// matter are about what IS there.
func asSlice(v any) []any {
	out, _ := v.([]any)
	return out
}

// 驗證計畫第 5 條 over HTTP: the schema moved, so the approval must fail.
func TestE2E_ApprovingAProposalThePlanNoLongerMatchesIsRefused(t *testing.T) {
	requireE2E(t)

	_, _, login := registerAndLogin(t, "schemastale")
	token := login["access_token"].(string)
	human := "Bearer " + token

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"note","label":"note","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		human, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	agent := "Bearer " + mintAgent(t, token, "schema-bot", []string{"note"})

	document := `{"artifact_version":"1","kind":"content-schema","types":[
		{"name":"note","label":"note","fields":[
			{"key":"title","type":"text","label":"T","required":true},
			{"key":"body","type":"richtext","label":"Body"}]}]}`

	rec = doJSON(t, http.MethodPost, "/api/v1/content/schema/proposals", document, agent, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := decodeDataMap(t, rec)["id"].(string)

	// Somebody changes the schema underneath the proposal.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/types/note/fields",
		`{"key":"summary","type":"text","label":"Summary"}`, human, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodPost, "/api/v1/content/schema/proposals/"+id+"/approve", "", human, "", e2eClientIP(t))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, "CONTENT_SCHEMA_PROPOSAL_STALE", proposalErrorCode(t, rec))

	// AND NOTHING WAS APPLIED. Without this the test passes on a service that
	// applied the artifact and then reported the mismatch.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/types/note", "", human, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	fields, _ := decodeDataMap(t, rec)["fields"].([]any)
	for _, f := range fields {
		field, _ := f.(map[string]any)
		assert.NotEqual(t, "body", field["key"], "a refused approval applied its artifact anyway")
	}

	// The row is still pending: a stale plan is not a decision.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/schema/proposals/"+id, "", human, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "pending", decodeDataMap(t, rec)["status"])
}

// 補裁 T (2026-08-30) over HTTP, and THE PAIRING IS THE CLAIM: one credential
// that both files schema proposals and is confined by own_only_roles.
//
// Neither half is new alone. What was impossible until today is holding them
// together. content:schema:propose was owner/admin, so a proposing agent had to
// carry one of those roles — and own_only confinement is a set-membership test
// ($2 = ANY(ct.own_only_roles) against the READER's role, bound in
// postgres_repository buildWhere), which nobody satisfies by listing "admin" to
// confine their admins. 補裁 S-1 (2026-08-20) made the credential's role the
// minter's choice, and that choice was then between "an agent that can propose"
// and "an agent that can be confined" — never both. Widening plan and propose to
// the content roles is what removes the trade.
//
// So this mints BOTH shapes from the same person and reads the same list with
// each. Drop the admin agent and the editor's short list stops distinguishing
// confinement from an agent that sees nothing at all; drop the editor's own row
// and it stops distinguishing confinement from an empty page.
func TestE2E_EditorRoledAgentProposesAndIsStillConfined(t *testing.T) {
	requireE2E(t)

	// A owns the tenant and writes the row that neither of B's agents authored.
	_, _, loginA := registerAndLogin(t, "editprop")
	tokenA := loginA["access_token"].(string)
	humanA := "Bearer " + tokenA
	slugA := loginA["tenant_id"].(string)

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"memo","label":"memo","own_only_roles":["editor"],`+
			`"fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		humanA, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=memo",
		`{"title":"written by A"}`, humanA, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rowA := decodeDataMap(t, rec)["id"].(string)

	// B is an ADMIN because minting is owner/admin only — an editor cannot mint
	// at all. That is exactly why the confinement below has to bind the
	// CREDENTIAL's role rather than the minter's: B is unconfinable and mints an
	// agent that is not. Two logins plus one switch-tenant land on this IP,
	// against AUTH_LOGIN_RATE_LIMIT's default of 10 per minute.
	_, emailB, loginB := registerAndLogin(t, "editpropb")
	rec = doJSON(t, http.MethodPost, "/api/v1/tenants/invites",
		fmt.Sprintf(`{"email":%q,"role":"admin"}`, emailB), humanA, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	invite := decodeDataMap(t, rec)["token"].(string)

	ownB := loginB["access_token"].(string)
	rec = doJSON(t, http.MethodPost, "/api/v1/tenants/invites/accept",
		fmt.Sprintf(`{"token":%q}`, invite), "Bearer "+ownB, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doJSON(t, http.MethodPost, "/api/v1/auth/switch-tenant",
		fmt.Sprintf(`{"tenant":%q}`, slugA), "Bearer "+ownB, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	tokenB := decodeDataMap(t, rec)["access_token"].(string)

	_, editorTok := mintAgentCredential(t, tokenB, "proposer-bot", "editor", []string{"memo"})
	// The control is minted by A, not by B, because 補裁 S-1 forbids minting your
	// OWN role and B is an admin. That makes the control sharper rather than
	// weaker: A's admin-roled agent sees the row B's agent wrote, across
	// principals, which is precisely what own_only_roles:["editor"] cannot reach.
	_, adminTok := mintAgentCredential(t, tokenA, "legacy-bot", "admin", []string{"memo"})
	editorAgent := "Bearer " + editorTok

	document := `{"artifact_version":"1","kind":"content-schema","types":[
		{"name":"memo","label":"memo","own_only_roles":["editor"],"fields":[
			{"key":"title","type":"text","label":"T","required":true},
			{"key":"body","type":"richtext","label":"Body"}]}]}`

	// Still refused where it was refused before. Asserted first: the widening is
	// only interesting if the boundary it does NOT move is still standing.
	//
	// FOR THIS CALLER THE REFUSAL IS THE AGENT GATE, not the tenant role.
	// refuseUnlistedAgentAction enumerates the verbs an agent may hold at all
	// and content:schema:write is deliberately absent, so an agent is refused
	// before its role is consulted — MEASURED by deleting the write branch from
	// allowContentByTenantRole and watching this test stay green while the authz
	// table test and TestApplySchema_EditorCannotApply go red. Those two are
	// where the tenant-role half of the boundary is pinned; this line is the
	// other half, and both have to hold for an editor-roled agent to be safe.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/schema/apply", document, editorAgent, "", e2eClientIP(t))
	require.Equal(t, http.StatusForbidden, rec.Code,
		"an editor-roled agent reached /schema/apply — 補裁 T widened plan and propose, not write: %s", rec.Body)

	// The two verbs the ruling opened, in the order an agent uses them: read the
	// plan, then file what it read. They moved together because a proposer who
	// cannot see a plan cannot know what its proposal says.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/schema/plan", document, editorAgent, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code,
		"content:schema:plan is closed to an editor-roled agent: %s", rec.Body)

	rec = doJSON(t, http.MethodPost, "/api/v1/content/schema/proposals", document, editorAgent, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code,
		"content:schema:propose is closed to an editor-roled agent — this is the ruling: %s", rec.Body)
	filed := decodeDataMap(t, rec)
	proposalID, _ := filed["id"].(string)
	require.NotEmpty(t, proposalID)
	assert.Equal(t, "agent", filed["proposed_by_kind"])
	assert.Equal(t, "proposer-bot", filed["proposed_by_agent"])

	// The queue did not open with it. What an editor gained is the ability to
	// ASK; reading the queue and deciding on it are content:schema:write
	// (schema_proposal.go ListSchemaProposals/GetSchemaProposal/decide), which
	// stayed owner/admin — and which, for an agent, the gate above refuses
	// first. Kept even though the sibling test asserts the same four doors
	// against an admin-roled agent: the ruling changed which roles reach this
	// route, and "the widening stopped at propose" is a claim about the WIDENED
	// role, which only this credential can make.
	for _, call := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/content/schema/proposals"},
		{http.MethodGet, "/api/v1/content/schema/proposals/" + proposalID},
		{http.MethodPost, "/api/v1/content/schema/proposals/" + proposalID + "/approve"},
		{http.MethodPost, "/api/v1/content/schema/proposals/" + proposalID + "/reject"},
	} {
		rec = doJSON(t, call.method, call.path, "", editorAgent, "", e2eClientIP(t))
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"%s %s answered an editor-roled agent: %s", call.method, call.path, rec.Body)
	}

	// Its own door is open, as it is for every proposer.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/schema/proposals/mine/"+proposalID, "", editorAgent, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "pending", decodeDataMap(t, rec)["status"])

	// AND THE OTHER HALF. The same credential that just filed is confined by
	// own_only_roles — the property the pre-T build could not give a proposer.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=memo",
		`{"title":"written by B's editor agent"}`, editorAgent, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rowEditor := decodeDataMap(t, rec)["id"].(string)

	listAs := func(bearer string) (ids []string, total float64) {
		rec := doJSON(t, http.MethodGet, "/api/v1/content/entries?type=memo", "", "Bearer "+bearer, "", e2eClientIP(t))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		data := decodeDataMap(t, rec)
		for _, it := range data["items"].([]any) {
			ids = append(ids, it.(map[string]any)["id"].(string))
		}
		return ids, data["total"].(float64)
	}

	ids, total := listAs(editorTok)
	assert.Equal(t, []string{rowEditor}, ids,
		"the proposing agent saw a row it did not write — own_only stopped binding once the credential could propose")
	assert.EqualValues(t, 1, total, "the count must agree with the page, or the confinement leaks through pagination")

	// THE CONTROL, and the shape 補裁 S-1 forced on any proposing agent before
	// today: same tenant, same type, admin role — and own_only_roles naming
	// "editor" reaches none of it, across principals. This is the cost the
	// ruling removed.
	ids, total = listAs(adminTok)
	assert.ElementsMatch(t, []string{rowA, rowEditor}, ids,
		"an admin-roled agent must see both rows — including one written under another principal — or the editor's short list proves nothing")
	assert.EqualValues(t, 2, total)
}

// proposalErrorCode reads the API's code out of the envelope. Written out here
// rather than compared as a whole body, because the code is the part the
// console branches on — a test that matched the message would go red on
// rewording and green on a code that changed meaning.
func proposalErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), "body is not JSON: %s", rec.Body)
	require.NotNil(t, env.Error, "no error envelope: %s", rec.Body)
	return env.Error.Code
}
