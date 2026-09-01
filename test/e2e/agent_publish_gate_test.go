package e2e_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ADR-014 §驗證計畫第 7 條: the gate is a server-side authorization rule, not
// the absence of a tool.
//
// WHY OVER HTTP. ADR-013's dominating rule is that the tool list is UX and not
// an authorization boundary, and §1 of ADR-014 leans on it directly. A test
// that only showed `cms_set_status` no longer offering publish would prove the
// UX and nothing else — so this drives the agent credential at the endpoint,
// with no MCP surface anywhere in the path. The one that would be exploited is
// exactly this one: a caller holding an agent token and a URL.
//
// It also crosses layers the service test cannot see: the route, the token's
// downgrade to an agent subject, the real authorizer, and Postgres. An entry
// this refusal failed to protect would look identical at the service layer.
func TestE2E_AgentIsRefusedPublishOverHTTP(t *testing.T) {
	requireE2E(t)

	_, _, login := registerAndLogin(t, "agtpublish")
	token := login["access_token"].(string)
	human := "Bearer " + token

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"memo","label":"memo","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		human, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	agent := "Bearer " + mintAgent(t, token, "content-bot", []string{"memo"})

	// The agent does its work unattended, and this half must keep working: a
	// gate that also stopped the write would be the shape ADR-014 §1 explicitly
	// refuses, and every refusal below would then prove nothing about publishing.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=memo",
		`{"title":"drafted by the agent"}`, agent, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := decodeDataMap(t, rec)["id"].(string)

	// The gate.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries/"+id+"/publish?type=memo",
		"", agent, "", e2eClientIP(t))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// And the entry is still not live. Asserting the status code alone would
	// miss a handler that answered 403 after the write — the refusal has to be
	// about the entry, not about the response.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries/"+id+"?type=memo", "", human, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "draft", decodeDataMap(t, rec)["status"],
		"the agent's entry went live despite the refusal")

	// CONTROL. Without it a route that 403s everybody — or one that does not
	// exist — passes every assertion above. This is also §驗證計畫 8's third
	// bullet over HTTP: the person the gate exists for must get through.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries/"+id+"/publish?type=memo",
		"", human, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "published", decodeDataMap(t, rec)["status"])
}

// ADR-014 §驗證計畫 8, first bullet, over HTTP: retracting is stopping the
// bleeding and stays available to the agent.
//
// This is the assertion that keeps the gate from being implemented as "agents
// may not touch editorial state", which would pass the refusal test above while
// requiring bad content to stay public until a person logs in. The exemption is
// only sound because §5.1 made retract keep the snapshot — so the second half
// checks that the released copy survived the agent's retract, which is what
// makes the action reversible and therefore safe to leave ungated.
func TestE2E_AgentMayUnpublishOverHTTP(t *testing.T) {
	requireE2E(t)

	_, _, login := registerAndLogin(t, "agtretract")
	token := login["access_token"].(string)
	human := "Bearer " + token

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"memo","label":"memo","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		human, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=memo",
		`{"title":"released text"}`, human, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := decodeDataMap(t, rec)["id"].(string)

	// Live, by the hand the ADR says has to do it.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries/"+id+"/publish?type=memo",
		"", human, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	agent := "Bearer " + mintAgent(t, token, "content-bot", []string{"memo"})
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries/"+id+"/unpublish?type=memo",
		"", agent, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String(),
		"the agent could not take down live content — the gate caught the stop-the-bleeding action too")

	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries/"+id+"?type=memo", "", human, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := decodeDataMap(t, rec)
	assert.Equal(t, "draft", body["status"], "the retract did not take")
	// §5.1: the snapshot survives the retract. Hard-written rather than compared
	// against the working copy — the two are equal here, so reading one from the
	// other would pass on a row where BOTH had been wiped.
	assert.Equal(t, map[string]any{"title": "released text"}, body["published_data"],
		"the agent's retract destroyed the published snapshot — the exemption's premise")
}
