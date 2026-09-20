package e2e_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The diff material has to survive the whole chain — the SELECT that loads the
// snapshot column, the projector, the marshaller and the handler — before a
// console can render anything (ADR-014 §6). The service tests prove the
// projection against hand-built entries; none of them can show that
// `published_data` is on the bytes an HTTP client receives, because every one of
// them is holding the DTO rather than a response body.
//
// That distinction is not theoretical here: `published_payload` is selected by a
// shared column list that a read path can silently omit, and the delivery half
// of this test is asserting on the exact key set OD2-025 shipped a leak in.
func TestE2E_EntryDiffReachesTheAdminWireOnly(t *testing.T) {
	requireE2E(t)

	_, _, login := registerAndLogin(t, "diff")
	token := login["access_token"].(string)
	slug := login["tenant_id"].(string)

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"note","label":"N","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		"Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=note",
		`{"title":"released"}`, "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	created := decodeDataMap(t, rec)
	id := created["id"].(string)

	// A draft has no snapshot, and the ABSENCE is the signal a console renders
	// as "never published" rather than as "no changes" — so it is asserted, not
	// assumed.
	_, present := created["published_data"]
	require.False(t, present, "a draft has no live version to diff against")

	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries/"+id+"/publish?type=note",
		"", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	published := decodeDataMap(t, rec)
	require.Equal(t, map[string]any{"title": "released"}, published["published_data"],
		"a published entry must carry the snapshot the console diffs against")
	require.Equal(t, published["data"], published["published_data"],
		"nothing was edited after publishing, so the two copies must agree")

	// Edit, and the two must part company — with the SNAPSHOT keeping the
	// released text. Reading it back through GET rather than trusting the PATCH
	// response is the point: the two are different code paths onto the same
	// column list, which is the pairing that has drifted here before.
	rec = doJSON(t, http.MethodPatch, "/api/v1/content/entries/"+id+"?type=note",
		`{"title":"edited"}`, "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries/"+id+"?type=note",
		"", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decodeDataMap(t, rec)
	require.Equal(t, map[string]any{"title": "edited"}, got["data"])
	require.Equal(t, map[string]any{"title": "released"}, got["published_data"],
		"the snapshot must still hold what is LIVE, not the edit that has not been released")
	require.Equal(t, true, got["has_unpublished_changes"])

	// The list path is a separate call site of the same projector and its own
	// SELECT. A field that arrives on GET-one and not on the list is the exact
	// asymmetry entrySelectColumns exists to prevent, and it looks like data.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries?type=note",
		"", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	items := decodeDataMap(t, rec)["items"].([]any)
	require.Len(t, items, 1)
	require.Equal(t, map[string]any{"title": "released"},
		items[0].(map[string]any)["published_data"],
		"the list path must carry the snapshot too, or the console's diff works on one screen and not the other")

	// And none of it reaches a public reader. published_data is the live text,
	// which delivery already receives as `data` — but has_hidden_changes is
	// editing state, and the pair travels together.
	dtok, _, err := e2eSigner.IssueDeliveryToken(uuid.New(), slug)
	require.NoError(t, err)
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries/"+id+"?type=note",
		"", "Bearer "+dtok, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	delivered := decodeDataMap(t, rec)
	require.Equal(t, map[string]any{"title": "released"}, delivered["data"],
		"delivery serves the snapshot as `data` — guard, so the absences below are not vacuous")
	for _, k := range []string{"published_data", "has_hidden_changes"} {
		_, leaked := delivered[k]
		require.False(t, leaked, "%s must not reach a public reader", k)
	}
}
