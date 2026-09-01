package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ADR-014 §2's release queue over real HTTP.
//
// WHY HERE AND NOT ONLY AT THE SERVICE LAYER. The service half is pinned in
// service/pending_review_test.go against a Go fake that REWRITES the SQL
// criterion, so it proves the service and the fake agree and nothing about
// whether a wired route returns the same set. Two of the three things that can
// go wrong here are invisible from there: the route may not be mounted at all,
// and the real query runs under RLS against a real database where the
// cross-type WHERE has no content_type_id to lean on.
//
// The repository half (repository/pending_review_integration_test.go) runs the
// real predicate but calls it in-process, bypassing the router and the
// authorizer. All three are needed and none subsumes another.

func pendingTitles(t *testing.T, body string) []string {
	t.Helper()
	// The envelope is {"data":{"items":[...]}} — every response on this API is
	// wrapped, and reading `items` off the top level yields an empty slice that
	// looks exactly like an empty queue. That is how the first version of this
	// helper failed: three tests reported "nothing is waiting" against a queue
	// that was returning rows correctly.
	var out struct {
		Data struct {
			Items []struct {
				Title                string `json:"title"`
				ContentType          string `json:"content_type"`
				Status               string `json:"status"`
				HasPublishedSnapshot bool   `json:"has_published_snapshot"`
			} `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	titles := make([]string, 0, len(out.Data.Items))
	for _, it := range out.Data.Items {
		titles = append(titles, it.Title)
	}
	return titles
}

// TestE2E_ReleaseQueueHoldsWhatIsNotLive is 驗證計畫第 9 條 through the front
// door, and it carries the draft half deliberately: a fixture made only of
// published entries satisfies the clause as written while the queue silently
// drops every draft, which is half of what §2 puts in it.
func TestE2E_ReleaseQueueHoldsWhatIsNotLive(t *testing.T) {
	requireE2E(t)

	_, _, login := registerAndLogin(t, "pendingq")
	token := login["access_token"].(string)
	human := "Bearer " + token

	// Two types, because the queue's reason for existing is that it spans them.
	for _, name := range []string{"memo", "note"} {
		rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
			`{"name":"`+name+`","label":"`+name+`","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
			human, "", e2eClientIP(t))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}

	create := func(typeName, title string) string {
		rec := doJSON(t, http.MethodPost, "/api/v1/content/entries?type="+typeName,
			`{"title":"`+title+`"}`, human, "", e2eClientIP(t))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		return decodeDataMap(t, rec)["id"].(string)
	}
	publish := func(typeName, id string) {
		rec := doJSON(t, http.MethodPost, "/api/v1/content/entries/"+id+"/publish?type="+typeName,
			"", human, "", e2eClientIP(t))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}

	// Live and edited: goes live, then the working copy moves away from it.
	liveEdited := create("memo", "live-edited")
	publish("memo", liveEdited)
	rec := doJSON(t, http.MethodPatch, "/api/v1/content/entries/"+liveEdited+"?type=memo",
		`{"title":"live-edited, revised"}`, human, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Live and clean: the row that must NOT appear.
	publish("memo", create("memo", "live-clean"))

	// Never released — the half `status = published` drops. In the OTHER type,
	// so a queue that only ever reached one type fails here too.
	create("note", "fresh-draft")

	rec = doJSON(t, http.MethodGet, "/api/v1/content/pending-review", "", human, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	titles := pendingTitles(t, rec.Body.String())

	// Expected values written out, not filtered from the fixture.
	assert.ElementsMatch(t, []string{"live-edited, revised", "fresh-draft"}, titles)
	assert.NotContains(t, titles, "live-clean",
		"a live entry with no unreleased edits is waiting on nobody")
}

// TestE2E_ReleaseQueueCarriesNoFieldValues. The queue spans every type, so it
// cannot apply the per-type read rule or §6's field mask — the protection is
// that no payload leaves at all. Asserted against the RAW body: a value that
// reaches the client is out regardless of whether the console draws it.
func TestE2E_ReleaseQueueCarriesNoFieldValues(t *testing.T) {
	requireE2E(t)

	_, _, login := registerAndLogin(t, "pendingval")
	token := login["access_token"].(string)
	human := "Bearer " + token

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"memo","label":"memo","fields":[`+
			`{"key":"title","type":"text","label":"T","required":true},`+
			`{"key":"body","type":"text","label":"B"}]}`,
		human, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=memo",
		`{"title":"a memo","body":"SENTINEL-BODY-VALUE"}`, human, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodGet, "/api/v1/content/pending-review", "", human, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "SENTINEL-BODY-VALUE",
		"the queue says WHICH entries are waiting; the per-entry diff says what changed")
	// Control: the row IS there, or the absence above would hold for an empty
	// response just as well.
	assert.Contains(t, pendingTitles(t, rec.Body.String()), "a memo")
}

// TestE2E_AgentCannotReadTheReleaseQueue. Same mechanism as the activity stream
// (ADR-013 §4's untyped rule) and the same reason it is right: the queue is the
// list of what an agent's output is waiting on a person to approve.
//
// Asked over HTTP rather than only at the service, because ADR-013's dominating
// rule is that the tool surface is UX and the server is the authorization
// boundary — a refusal that only holds when the service is called directly is
// not a boundary.
func TestE2E_AgentCannotReadTheReleaseQueue(t *testing.T) {
	requireE2E(t)

	_, _, login := registerAndLogin(t, "pendingagt")
	token := login["access_token"].(string)
	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"memo","label":"memo","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		"Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	agent := "Bearer " + mintAgent(t, token, "content-bot", []string{"memo"})
	rec = doJSON(t, http.MethodGet, "/api/v1/content/pending-review", "", agent, "", e2eClientIP(t))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "CONTENT_AGENT_SCOPE_UNTYPED", decodeErrorCode(t, rec.Body.String()))

	// Control: the minter reaches it, or the refusal above would also hold for
	// an endpoint that is simply not mounted.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/pending-review", "", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestE2E_ReleaseQueueIsTenantScoped. The cross-type WHERE has no
// content_type_id to narrow it, so tenant isolation rests on the tenant clause
// and RLS alone — asked here through the front door where both are live.
func TestE2E_ReleaseQueueIsTenantScoped(t *testing.T) {
	requireE2E(t)

	_, _, loginA := registerAndLogin(t, "pendscopea")
	tokenA := loginA["access_token"].(string)
	_, _, loginB := registerAndLogin(t, "pendscopeb")
	tokenB := loginB["access_token"].(string)

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"memo","label":"memo","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		"Bearer "+tokenA, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=memo",
		`{"title":"tenant A only"}`, "Bearer "+tokenA, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// Control first: tenant A sees its own waiting draft.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/pending-review", "", "Bearer "+tokenA, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, pendingTitles(t, rec.Body.String()), "tenant A only")

	rec = doJSON(t, http.MethodGet, "/api/v1/content/pending-review", "", "Bearer "+tokenB, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, pendingTitles(t, rec.Body.String()), "tenant A only")
}
