package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/pkg/response"
)

// ADR-013's agent credential, against the real router, the real authorizer, the
// real signing key and a real database.
//
// WHY THESE PARTICULAR PROPERTIES LIVE HERE. The ADR's verification plan says
// items 3 and 4 must be proved over direct HTTP, and it says so because the
// first version of the ADR failed exactly there: it proposed to keep agents out
// of schema apply by not exposing a tool for it. §5 records the correction —
// "the tool list is UX, not a security mechanism" — and a caller that skips MCP
// and posts to the endpoint is the thing that has to be refused. A test at any
// layer above HTTP cannot see that difference.
//
// mintAgent posts to the real minting ENDPOINT rather than calling the signer.
//
// It used to call e2eSigner.IssueAgentToken directly, which was the best
// available answer while no endpoint existed. It stopped being an option on
// 2026-08-06 for a reason worth recording: a hand-signed token now carries a
// `jti` naming an agent_credentials row that no test ever inserted, and the
// middleware refuses it. That is the check working — but it also means the
// hand-built path could only have been made to pass by inserting the row
// itself, i.e. by growing a second copy of the minting logic that drifts from
// the real one (the failure 0805 already paid for in the outbox worker).
// mintAgent mints with the ADMIN role, which is what these credentials carried
// before 補裁 S-1 made the role a choice (they COPIED an owner/admin minter),
// so every test that does not care about the role keeps the agent it had.
//
// NOT "editor", and the reason has changed since this comment was written.
//
// It used to be forced: schema:plan and schema:propose were owner/admin only,
// so an editor-flavoured agent could not file a schema proposal at all, and the
// minter had to choose between an agent that can propose and one
// own_only_roles can confine. 補裁 T (2026-08-30) opened both verbs to editor
// and dissolved that choice — an editor-flavoured agent can now do both.
//
// "admin" stays the default here anyway, and now it is a TEST decision rather
// than a constraint: these credentials carried admin before 補裁 S-1 made the
// role a choice, so keeping it is what stops this helper from silently
// rewriting what half the agents in this package are. Tests that care name the
// role at mintAgentCredential — and after 補裁 T, "editor" is a role a proposal
// test may legitimately ask for.
func mintAgent(t *testing.T, humanToken, agentID string, allowedTypes []string) string {
	t.Helper()
	id, token := mintAgentCredential(t, humanToken, agentID, "admin", allowedTypes)
	require.NotEmpty(t, id)
	return token
}

// mintAgentCredential returns the credential id alongside the token, for the
// tests that go on to revoke it.
func mintAgentCredential(t *testing.T, humanToken, agentID, tenantRole string, allowedTypes []string) (id, token string) {
	t.Helper()
	types, err := json.Marshal(allowedTypes)
	require.NoError(t, err)
	rec := doJSON(t, http.MethodPost, "/api/v1/auth/agent-tokens",
		fmt.Sprintf(`{"agent_id":%q,"tenant_role":%q,"allowed_types":%s}`, agentID, tenantRole, types),
		"Bearer "+humanToken, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	data := decodeDataMap(t, rec)
	return data["id"].(string), data["token"].(string)
}

// Verification plan items 3 and 4, plus §B's amend split, over real HTTP.
func TestE2E_AgentCredentialIsConfinedToItsScope(t *testing.T) {
	requireE2E(t)

	_, _, login := registerAndLogin(t, "agtscope")
	token := login["access_token"].(string)

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"note","label":"N","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		"Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"ledger","label":"L","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		"Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	agent := mintAgent(t, token, "content-bot", []string{"note"})
	bearer := "Bearer " + agent

	// The credential works where it is scoped. Asserted FIRST: every refusal
	// below is worthless if the credential simply does not function.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=note",
		`{"title":"written by a bot"}`, bearer, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries?type=note", "", bearer, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// ADR-013 §A: the path cms_describe is pointed at stays open...
	rec = doJSON(t, http.MethodGet, "/api/v1/content/types/note", "", bearer, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// ...and it now answers with the filter grammar (§6), which is the half of
	// cms_describe that was missing: before this, the operator list existed only
	// inside the 400 a wrong filter got back, so an agent had to guess a query,
	// be refused, and read the refusal to learn what it could have asked. Over
	// real HTTP because the point is what a caller RECEIVES.
	noteField := decodeDataMap(t, rec)["fields"].([]any)[0].(map[string]any)
	require.Equal(t, "title", noteField["key"])
	assert.Equal(t, []any{"eq", "neq", "gt", "gte", "lt", "lte", "in", "contains"}, noteField["supported"],
		"a scalar field publishes the eight scalar operators")

	// ...while the type LIST does not, because it names no single type. This is
	// the case §A deliberately left closed; the fix for cms_describe was to
	// iterate AllowedTypes, not to relax the rule.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/types", "", bearer, "", e2eClientIP(t))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "CONTENT_AGENT_SCOPE_UNTYPED", decodeErrorCode(t, rec.Body.String()))

	// Verification plan item 3: the untyped paths, hit directly over HTTP.
	// Media, webhooks and usage are closed to an agent BY CONSTRUCTION — they
	// pass no content type because they concern none.
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/content/media", `{"content_type":"image/png","filename":"x.png"}`},
		{http.MethodGet, "/api/v1/content/media/" + uuid.NewString(), ""},
		{http.MethodGet, "/api/v1/content/webhooks", ""},
		{http.MethodPost, "/api/v1/content/webhooks", `{"url":"https://example.com/hook","events":["content.entry.published"]}`},
		{http.MethodGet, "/api/v1/content/usage", ""},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := doJSON(t, tc.method, tc.path, tc.body, bearer, "", e2eClientIP(t))
			require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.Equal(t, "CONTENT_AGENT_SCOPE_UNTYPED", decodeErrorCode(t, rec.Body.String()))
		})
	}

	// A type the credential was not scoped to.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=ledger",
		`{"title":"not mine to write"}`, bearer, "", e2eClientIP(t))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "CONTENT_AGENT_TYPE_NOT_ALLOWED", decodeErrorCode(t, rec.Body.String()))

	// Verification plan item 4: apply is refused over direct HTTP. The minter is
	// an OWNER — the role that may apply — so this refusal is the credential's,
	// not the role's. This is the one the first version of the ADR got wrong.
	//
	// The artifact is WELL FORMED on purpose. The handler validates the envelope
	// before the service authorizes, so a malformed body answers 422 and would
	// have made this assertion pass without the authorizer ever running.
	validArtifact := `{"kind":"content-schema","artifact_version":"1","types":[]}`
	rec = doJSON(t, http.MethodPost, "/api/v1/content/schema/apply", validArtifact, bearer, "", e2eClientIP(t))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// PLAN, ON THE OTHER SIDE OF THE SAME LINE (ADR-013 補裁 E).
	//
	// §3 created content:schema:plan specifically so an agent could hold plan
	// without apply, and §5 exposes it as cms_plan_schema. The plan endpoint
	// takes a WHOLE-SCHEMA artifact, so it names no single content type in any
	// parameter — which used to mean it passed "" to authorize() and §4's
	// untyped rule closed it. The ruling did not relax that rule (doing so puts
	// media, webhooks and usage back within reach); it enforces the whitelist
	// against the types the DOCUMENT lists.
	//
	// In scope: the document names only `note`, which this credential holds.
	inScopeArtifact := `{"kind":"content-schema","artifact_version":"1","types":[` +
		`{"name":"note","label":"N","fields":[{"key":"title","type":"text","label":"T","required":true,"multiple":false}]}]}`
	rec = doJSON(t, http.MethodPost, "/api/v1/content/schema/plan", inScopeArtifact, bearer, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// And the plan it gets back is a plan of ITS OWN SCOPE. The tenant also has
	// `ledger`, which the document omits, so an unnarrowed diff would return a
	// delete step naming it — handing an agent the type list that GET /types was
	// refused for two blocks up. Asserted on the raw body: the leak would be a
	// type NAME appearing anywhere in the response, whatever field carries it.
	assert.NotContains(t, rec.Body.String(), "ledger",
		"the plan named a type this credential may not touch")

	// THE CONTROL for that assertion. The same artifact planned by the human who
	// minted the agent DOES name ledger — so the channel exists, the narrowing is
	// what closes it, and the assertion above is not passing on an empty body.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/schema/plan", inScopeArtifact, "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "ledger",
		"the owner's plan of the same document must mention ledger, or the agent assertion proves nothing")

	// Out of scope: one type outside the whitelist refuses the whole document.
	outOfScopeArtifact := `{"kind":"content-schema","artifact_version":"1","types":[` +
		`{"name":"ledger","label":"L","fields":[{"key":"title","type":"text","label":"T","required":true,"multiple":false}]}]}`
	rec = doJSON(t, http.MethodPost, "/api/v1/content/schema/plan", outOfScopeArtifact, bearer, "", e2eClientIP(t))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "CONTENT_AGENT_TYPE_NOT_ALLOWED", decodeErrorCode(t, rec.Body.String()),
		"refused by the artifact gate, NOT by the verb enumeration — the verb is allowed, the document is what shuts it out")

	// The same for the verbs §5 declines and §B split off: deleting an entry,
	// reshaping a type, adding a field. All of them on a type the credential IS
	// scoped to, so only the verb enumeration can be doing the refusing.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries?type=note", "", bearer, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code)
	items := decodeDataMap(t, rec)["items"].([]any)
	require.NotEmpty(t, items)
	entryID := items[0].(map[string]any)["id"].(string)

	rec = doJSON(t, http.MethodDelete, "/api/v1/content/entries/"+entryID+"?type=note", "", bearer, "", e2eClientIP(t))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodPost, "/api/v1/content/types/note/fields",
		`{"key":"body","type":"text","label":"B"}`, bearer, "", e2eClientIP(t))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodPatch, "/api/v1/content/types/note", `{"label":"renamed by a bot"}`, bearer, "", e2eClientIP(t))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// The control for every refusal above: the PERSON who minted the credential
	// may still do all of it. Without this the test would pass against a
	// deployment that had simply broken these endpoints.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/types/note/fields",
		`{"key":"body","type":"text","label":"B"}`, "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	// ADR-013 §7, over real HTTP and on the credential it exists for. `note` now
	// has two payload keys, so a projection of one is distinguishable from the
	// whole record — which is the only way this assertion can mean anything.
	rec = doJSON(t, http.MethodPatch, "/api/v1/content/entries/"+entryID+"?type=note",
		`{"body":"a long body the agent does not want to pay for on every row"}`, bearer, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries?type=note&fields=title", "", bearer, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	projected := decodeDataMap(t, rec)["items"].([]any)[0].(map[string]any)["data"].(map[string]any)
	assert.Equal(t, map[string]any{"title": "written by a bot"}, projected,
		"?fields= must narrow the payload to exactly what was asked for")

	// The control: the same request without the parameter still carries both
	// keys, so the assertion above is the projection and not a lossy write.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries?type=note", "", bearer, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	whole := decodeDataMap(t, rec)["items"].([]any)[0].(map[string]any)["data"].(map[string]any)
	assert.Len(t, whole, 2, "the unprojected payload must carry both keys: %v", whole)

	// The repeated spelling, which is what ?filter= already uses. Both forms are
	// accepted, so a client that reached for the wrong one is not silently
	// handed the whole payload it was trying to avoid.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries?type=note&fields=title&fields=body", "", bearer, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	repeated := decodeDataMap(t, rec)["items"].([]any)[0].(map[string]any)["data"].(map[string]any)
	assert.Len(t, repeated, 2, "?fields=a&fields=b must select both: %v", repeated)

	// A key that is not a field is refused rather than silently ignored.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries?type=note&fields=title,nope", "", bearer, "", e2eClientIP(t))
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, "CONTENT_FIELDS_FIELD_UNKNOWN", decodeErrorCode(t, rec.Body.String()))

	rec = doJSON(t, http.MethodDelete, "/api/v1/content/entries/"+entryID+"?type=note", "", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	rec = doJSON(t, http.MethodGet, "/api/v1/content/webhooks", "", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// Verification plan item 6, against the real columns: BOTH directions in one
// test, because each has a blind spot the other covers — an assertion that only
// checks the agent row passes when every write is marked as an agent's.
func TestE2E_AgentWritesRecordProvenance(t *testing.T) {
	requireE2E(t)
	ctx := context.Background()

	userID, _, login := registerAndLogin(t, "agtprov")
	token := login["access_token"].(string)

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"post","label":"P","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		"Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	agent := mintAgent(t, token, "content-bot", []string{"post"})

	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=post",
		`{"title":"by the bot"}`, "Bearer "+agent, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	byAgent := decodeDataMap(t, rec)["id"].(string)

	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=post",
		`{"title":"by the person"}`, "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	byHuman := decodeDataMap(t, rec)["id"].(string)

	read := func(id string) (createdBy *uuid.UUID, kind string, agentID *string) {
		require.NoError(t, e2ePool.QueryRow(ctx,
			`SELECT created_by, created_by_kind, created_by_agent FROM entries WHERE id = $1::uuid`,
			id).Scan(&createdBy, &kind, &agentID))
		return
	}

	createdBy, kind, agentID := read(byAgent)
	require.NotNil(t, createdBy, "an agent-written row must have an owner — a NULL author is the unownable row ADR-013 §2 prevents")
	assert.Equal(t, userID, createdBy.String(), "created_by is the PRINCIPAL: who answers for the write")
	assert.Equal(t, "agent", kind)
	require.NotNil(t, agentID)
	assert.Equal(t, "content-bot", *agentID, "and the row still says which software typed it")

	createdBy, kind, agentID = read(byHuman)
	require.NotNil(t, createdBy)
	assert.Equal(t, userID, createdBy.String())
	assert.Equal(t, "human", kind, "the same person writing directly is not recorded as an agent")
	assert.Nil(t, agentID)
}

// 補裁 S-1 over real HTTP: the role is asked for, and it is bounded.
//
// The unit layers (domain.CanMintAgentRole and the signer) own the full matrix;
// what only this layer can show is that the refusal arrives as a 403 carrying
// the domain's own sentence rather than as a 500-shaped internal error, and
// that the credential the registry records carries the role that was ASKED FOR
// rather than the minter's.
func TestE2E_AgentCredentialRoleIsChosenAndBounded(t *testing.T) {
	requireE2E(t)

	_, _, login := registerAndLogin(t, "agtrole")
	token := login["access_token"].(string)

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"memo","label":"M","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		"Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// An OWNER may not mint an owner. This is the same cell as "admin may not
	// mint admin" — a minter's own role is in nobody's grant set — and it is
	// the one an implementation that simply allowed anything would fail.
	rec = doJSON(t, http.MethodPost, "/api/v1/auth/agent-tokens",
		`{"agent_id":"peer-bot","tenant_role":"owner","allowed_types":["memo"]}`,
		"Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "owner",
		"the refusal names the roles; a bare 403 cannot be told from a missing verb")

	// Absent is refused, not defaulted. A fallback to the minter's own role is
	// exactly the behaviour this ruling removed, and it would arrive on the
	// path where the caller forgot to decide.
	rec = doJSON(t, http.MethodPost, "/api/v1/auth/agent-tokens",
		`{"agent_id":"peer-bot","allowed_types":["memo"]}`,
		"Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "tenant_role")

	// And the granted case records what was asked for. Asserted through the
	// REGISTRY rather than the mint response, because the row is what an
	// operator reads during an incident and it is written from a separate
	// argument to the one the token carries.
	id, _ := mintAgentCredential(t, token, "narrow-bot", "viewer", []string{"memo"})
	rec = doJSON(t, http.MethodGet, "/api/v1/auth/agent-tokens", "", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var found bool
	for _, raw := range decodeDataSlice(t, rec) {
		row := raw.(map[string]any)
		if row["id"] == id {
			found = true
			assert.Equal(t, "viewer", row["tenant_role"],
				"an owner minted a viewer — a copied role could not produce this")
		}
	}
	assert.True(t, found, "the minted credential must appear in the tenant's registry")
}

// Verification plan item 7 — the gate on ADR-009:238's re-ruling, against the
// real SQL predicate.
//
// It has to be here rather than only at the service layer: own_only is a
// column predicate bound inside the query (postgres_repository buildWhere),
// alongside status and locale and before any caller filter, and it must be in
// the COUNT as well as the page. A fake repository implements that in Go and
// cannot show that the real one binds the right id.
func TestE2E_OwnOnlyHoldsForAgentWrittenRows(t *testing.T) {
	requireE2E(t)

	// Owner A, whose tenant hosts the confined type.
	_, _, loginA := registerAndLogin(t, "agtown")
	tokenA := loginA["access_token"].(string)
	slugA := loginA["tenant_id"].(string)

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"column","label":"C","own_only_roles":["editor","admin"],"fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		"Bearer "+tokenA, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// Two ADMINS in A's tenant: B, whose agent writes, and C, who must not see
	// what B's agent wrote. Each mints an agent carrying the EDITOR role.
	//
	// THE own_only_roles VALUE IS THE POINT OF THIS TEST, and its history is
	// worth keeping rather than editing away. It read ["editor"] until
	// 2026-08-06, when minting was narrowed to owner/admin and the credential
	// COPIED its minter's role: no agent could hold editor any more, so
	// own_only_roles naming only "editor" confined nothing, and the test moved
	// to ["admin"] to keep proving the SQL predicate with a role an agent could
	// actually carry. 補裁 S-1 (2026-08-20) gave the minter a choice bounded by
	// its own role, and "editor" comes back here — that entry is the ruling's
	// verification, and it is deliberately not enough to see the mint endpoint
	// answer 201.
	//
	// BOTH ENTRIES ARE LOAD-BEARING, AND THEY CONFINE DIFFERENT READERS. The
	// predicate binds the ROLE OF WHOEVER IS READING ($2 = ANY(own_only_roles)
	// against the caller's role), so:
	//   - "editor" confines the two AGENTS, which is what 補裁 S-1 made
	//     possible and what a list of ["admin"] alone could not do;
	//   - "admin" confines the two PEOPLE, B and C, who must be admins because
	//     minting is owner/admin only — an editor cannot mint at all.
	// Drop "editor" and agentC sees a row it did not write; drop "admin" and C
	// does. A first draft of this landing had ["editor"] alone and CI caught
	// exactly that: the humans went unconfined and read everything.
	//
	// The property under test is unchanged throughout: own_only binds the
	// agent's PRINCIPAL, in the page and in the COUNT, in the real SQL.
	// Each editor is enrolled inside its own subtest: e2eClientIP derives the
	// forwarded-for address from t.Name(), and the login rate limit is per IP —
	// three registrations plus two tenant switches on one address trips it, and
	// a 429 here would look exactly like a permission failure.
	adminIn := func(t *testing.T, prefix string) string {
		_, email, login := registerAndLogin(t, prefix)
		rec := doJSON(t, http.MethodPost, "/api/v1/tenants/invites",
			fmt.Sprintf(`{"email":%q,"role":"admin"}`, email), "Bearer "+tokenA, "", e2eClientIP(t))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		raw := decodeDataMap(t, rec)["token"].(string)

		own := login["access_token"].(string)
		rec = doJSON(t, http.MethodPost, "/api/v1/tenants/invites/accept",
			fmt.Sprintf(`{"token":%q}`, raw), "Bearer "+own, "", e2eClientIP(t))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		rec = doJSON(t, http.MethodPost, "/api/v1/auth/switch-tenant",
			fmt.Sprintf(`{"tenant":%q}`, slugA), "Bearer "+own, "", e2eClientIP(t))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return decodeDataMap(t, rec)["access_token"].(string)
	}
	var tokenB, tokenC string
	t.Run("enrol admin B", func(t *testing.T) { tokenB = adminIn(t, "agtedb") })
	t.Run("enrol admin C", func(t *testing.T) { tokenC = adminIn(t, "agtedc") })
	require.NotEmpty(t, tokenB)
	require.NotEmpty(t, tokenC)

	// Named rather than left to mintAgent's default: the type above confines
	// exactly this role, and a default that drifted would turn this test into a
	// check that agents see nothing at all.
	_, agentB := mintAgentCredential(t, tokenB, "content-bot", "editor", []string{"column"})
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=column",
		`{"title":"drafted by B's agent"}`, "Bearer "+agentB, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	written := decodeDataMap(t, rec)["id"].(string)

	listAs := func(bearer string) (ids []string, total float64) {
		rec := doJSON(t, http.MethodGet, "/api/v1/content/entries?type=column", "", "Bearer "+bearer, "", e2eClientIP(t))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		data := decodeDataMap(t, rec)
		for _, it := range data["items"].([]any) {
			ids = append(ids, it.(map[string]any)["id"].(string))
		}
		return ids, data["total"].(float64)
	}

	// B, the person, sees what B's agent wrote: the row is recorded against B.
	ids, total := listAs(tokenB)
	require.Equal(t, []string{written}, ids)
	assert.EqualValues(t, 1, total, "the count must agree with the page, or the confinement leaks through pagination")

	// C does not. Authorship by software did not make the row everyone's.
	ids, total = listAs(tokenC)
	assert.Empty(t, ids)
	assert.EqualValues(t, 0, total)

	// B's agent reads it back — the write and the read bind the same id, which
	// is the pair ADR-013 §2 had to settle together.
	ids, _ = listAs(agentB)
	assert.Equal(t, []string{written}, ids)

	// And C's agent inherits C's confinement, not B's.
	_, agentC := mintAgentCredential(t, tokenC, "content-bot", "editor", []string{"column"})
	ids, total = listAs(agentC)
	assert.Empty(t, ids, "the widening is to the MINTER's rows, not to everyone's")
	assert.EqualValues(t, 0, total)
}

// The credential LIFECYCLE over real HTTP (ADR-013, ruled 2026-08-06): mint,
// use, revoke, and the same token stops working on the very next request.
//
// This is the assertion that makes the long TTL defensible. Every other test in
// this file proves what an agent credential may reach; this one proves the
// tenant can take it away — and it has to run against the real middleware,
// because revocation is enforced there and nowhere else. A service-level test
// would pass with the check deleted.
func TestE2E_AgentCredentialCanBeRevoked(t *testing.T) {
	requireE2E(t)

	_, _, login := registerAndLogin(t, "agtrevoke")
	token := login["access_token"].(string)

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"memo","label":"M","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		"Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	credID, agent := mintAgentCredential(t, token, "revocable-bot", "admin", []string{"memo"})
	bearer := "Bearer " + agent

	// Live first. A revocation test whose credential never worked proves that
	// the credential never worked.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=memo",
		`{"title":"before"}`, bearer, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// The list shows it, and shows it as active. `active` is computed rather
	// than inferred from revoked_at, so this also pins the difference between
	// "not revoked" and "usable".
	rec = doJSON(t, http.MethodGet, "/api/v1/auth/agent-tokens", "", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	listed := decodeDataSlice(t, rec)
	require.Len(t, listed, 1)
	assert.Equal(t, credID, listed[0].(map[string]any)["id"])
	assert.Equal(t, "revocable-bot", listed[0].(map[string]any)["agent_id"])
	assert.Equal(t, true, listed[0].(map[string]any)["active"])

	// An agent may not mint, list or revoke credentials — including its own.
	// Through HTTP rather than at the authorizer, because "the tool list is UX,
	// not authorization" applies to this endpoint exactly as it does to schema
	// apply: there is no MCP tool for minting, and that is not what refuses it.
	//
	// WHICH LAYER ANSWERS IS NOT THE SAME FOR THE THREE, and a black-box test
	// cannot tell — mutation testing can, so it is written down here. Removing
	// the authorizer call from list or from revoke makes those two lines go
	// green (verified 2026-08-06); removing it from MINT does not, because
	// IssueAgentToken independently refuses a minter whose Kind is set, so the
	// 403 arrives from the signer instead. The authorizer layer for mint is
	// therefore proved WHITE-BOX, in authz's agentActionExpectations, not here.
	// Both layers are wanted: the signer's covers every future call site, and
	// the authorizer's is what answers before any signing is attempted.
	rec = doJSON(t, http.MethodPost, "/api/v1/auth/agent-tokens",
		`{"agent_id":"second-bot","tenant_role":"editor","allowed_types":["memo"]}`, bearer, "", e2eClientIP(t))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doJSON(t, http.MethodGet, "/api/v1/auth/agent-tokens", "", bearer, "", e2eClientIP(t))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = doJSON(t, http.MethodDelete, "/api/v1/auth/agent-tokens/"+credID, "", bearer, "", e2eClientIP(t))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// Revoke.
	rec = doJSON(t, http.MethodDelete, "/api/v1/auth/agent-tokens/"+credID, "", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	// The SAME token, one request later. Not a new token, not after a restart:
	// the credential the agent is already holding stops working.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=memo",
		`{"title":"after"}`, bearer, "", e2eClientIP(t))
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())

	// Reads too — revocation is not a write-only gate.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries?type=memo", "", bearer, "", e2eClientIP(t))
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())

	// The human who revoked it is unaffected: this is a credential being turned
	// off, not an account.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries?type=memo", "", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Pressing the kill switch twice is not an error, and does not move the
	// timestamp — the second press answers "it is off".
	rec = doJSON(t, http.MethodDelete, "/api/v1/auth/agent-tokens/"+credID, "", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodGet, "/api/v1/auth/agent-tokens", "", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after := decodeDataSlice(t, rec)[0].(map[string]any)
	assert.Equal(t, false, after["active"])
	assert.NotEmpty(t, after["revoked_at"])

	// Another tenant's owner cannot revoke this credential, and learns nothing
	// about whether it exists: the repository scopes by tenant, so the id is a
	// 404 rather than a 403.
	_, _, other := registerAndLogin(t, "agtother")
	rec = doJSON(t, http.MethodDelete, "/api/v1/auth/agent-tokens/"+credID, "",
		"Bearer "+other["access_token"].(string), "", e2eClientIP(t))
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// And that tenant's own list is empty — no cross-tenant leak through the
	// read side either.
	rec = doJSON(t, http.MethodGet, "/api/v1/auth/agent-tokens", "",
		"Bearer "+other["access_token"].(string), "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, decodeDataSlice(t, rec))
}

// Minting refuses what the signer refuses, with a message that names the field.
func TestE2E_AgentCredentialMintingRequiresAScope(t *testing.T) {
	requireE2E(t)

	_, _, login := registerAndLogin(t, "agtscopereq")
	token := login["access_token"].(string)

	// Each case asserts WHICH field was named, not merely that something was
	// refused. Without that, adding a new required field (補裁 S-1 added
	// tenant_role) makes every case here pass for its refusal instead — the
	// bodies below were missing it and stayed green while testing nothing they
	// claim to.
	for name, tc := range map[string]struct{ body, names string }{
		"no allowed_types":    {`{"agent_id":"bot","tenant_role":"editor"}`, "allowed_types"},
		"empty allowed_types": {`{"agent_id":"bot","tenant_role":"editor","allowed_types":[]}`, "allowed_types"},
		// "AgentID", not "agent_id": this one is refused by the struct-tag
		// validator, which names the GO FIELD. The handler's own comment on
		// AllowedTypes predicted exactly this ("a validator tag here would
		// answer the absent case in a different voice") — the voice is what
		// this expectation records.
		"no agent id":    {`{"tenant_role":"editor","allowed_types":["memo"]}`, "AgentID"},
		"no tenant_role": {`{"agent_id":"bot","allowed_types":["memo"]}`, "tenant_role"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doJSON(t, http.MethodPost, "/api/v1/auth/agent-tokens", tc.body,
				"Bearer "+token, "", e2eClientIP(t))
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.names)
		})
	}
}

// decodeDataSlice is decodeDataMap's list twin — the credential list is a JSON
// array, and asserting on it through the map helper would panic rather than
// fail.
func decodeDataSlice(t *testing.T, rec *httptest.ResponseRecorder) []any {
	t.Helper()
	var env response.Envelope
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&env))
	require.Nil(t, env.Error)
	s, ok := env.Data.([]any)
	require.True(t, ok, "expected a JSON array, got %T", env.Data)
	return s
}
