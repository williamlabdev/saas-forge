package e2e_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/auth/jwt"
)

// End-to-end proof of the ADR-006 preview link against the real router, real
// authz, a real database and the real delivery signing key.
//
// WHY IT EXISTS. The same properties were proved once by a probe script run
// against the compose stack — and that script lived in a session scratchpad, so
// it went away with the session and nothing has re-run it since. Everything
// below is otherwise covered only by unit tests with a fake repository and a
// fake signer, which cannot show that a token this deployment MINTS is a token
// this deployment ACCEPTS.
//
// The requests are the ones the delivery edge issues: the edge forwards the
// caller's preview token verbatim as the bearer of a single-entry read
// (apps/delivery/internal/upstream.GetEntryPreview), and that is exactly what
// this test sends. The edge's own half — no-store, no ETag, refusal on
// collection routes — is proved over real HTTP in apps/delivery/test/e2e, which
// can import the edge's internal packages while this suite cannot.
func TestE2E_PreviewLink(t *testing.T) {
	requireE2E(t)

	_, _, login := registerAndLogin(t, "pvw")
	token := login["access_token"].(string)
	slug := login["tenant_id"].(string)

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"story","label":"S","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		"Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=story",
		`{"title":"draft under review"}`, "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	draft := decodeDataMap(t, rec)
	draftID := draft["id"].(string)
	require.Equal(t, "draft", draft["status"], "the point of a preview is that it is not published")

	// A second entry in the same tenant, PUBLISHED on purpose. A second draft
	// would make the scope assertion below pass for the wrong reason: with the
	// id comparison removed, an unpublished row still 404s on the published-only
	// gate underneath it, so the test would stay green while the narrowing was
	// gone. Published, only the id comparison can produce the 404.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=story",
		`{"title":"already public, still not this token's"}`, "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	otherID := decodeDataMap(t, rec)["id"].(string)
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries/"+otherID+"/publish?type=story",
		"", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// 1. Minting. 201 because the link is new, and no-store because this body IS
	//    the credential.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries/"+draftID+"/preview-link?type=story",
		"", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"),
		"the mint response carries a live bearer token and must not be stored")
	link := decodeDataMap(t, rec)
	previewToken := link["token"].(string)
	require.NotEmpty(t, previewToken)
	require.Equal(t, draftID, link["entry_id"], "the caller builds a URL from what was SIGNED, not from its own variables")
	require.Equal(t, "story", link["type"])
	require.Equal(t, slug, link["tenant"], "the tenant path segment comes back from the mint, not from the caller's guess")

	// The TTL is the entire security story for a token nothing can revoke, so it
	// is asserted rather than assumed.
	exp, err := time.Parse(time.RFC3339, link["expires_at"].(string))
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().UTC().Add(jwt.PreviewTokenTTL), exp, time.Minute,
		"a preview credential expires in PreviewTokenTTL and cannot be revoked before then")

	// 2. The read the edge forwards: the working copy of the one entry named, in
	//    the delivery shape.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries/"+draftID+"?type=story",
		"", "Bearer "+previewToken, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decodeDataMap(t, rec)
	require.Equal(t, "draft under review", entryField(t, got, "title"),
		"a preview credential serves the unpublished working copy — that is what it is for")
	require.NotContains(t, got, "created_by", "the preview audience is shown no actors")
	require.NotContains(t, got, "updated_by")
	require.NotContains(t, got, "has_unpublished_changes", "editorial state is not the reviewer's business")

	// 3. The same token pointed at the tenant's other entry — published, and so
	//    readable by an ordinary delivery credential. Still 404: this token
	//    addresses one row, and 404 rather than 403 because a preview link is the
	//    delivery credential most likely to be aimed at ids it never named.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries/"+otherID+"?type=story",
		"", "Bearer "+previewToken, "", e2eClientIP(t))
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// 4. Collections. Refused uniformly — a preview token that could list would
	//    burn the tenant's metered read quota from outside the platform.
	for _, path := range []string{
		"/api/v1/content/entries?type=story",
		"/api/v1/content/entries/" + draftID + "/translations?type=story",
	} {
		rec = doJSON(t, http.MethodGet, path, "", "Bearer "+previewToken, "", e2eClientIP(t))
		require.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", path, rec.Body.String())
		require.Contains(t, rec.Body.String(), "CONTENT_PREVIEW_SCOPE_EXCEEDED", path)
	}

	// 5. Re-minting. THE gate: a credential that can issue itself a fresh token
	//    before each expiry has no TTL at all, for any entry in the tenant.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries/"+otherID+"/preview-link?type=story",
		"", "Bearer "+previewToken, "", e2eClientIP(t))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "CONTENT_PREVIEW_MINT_FORBIDDEN")

	// Minting for the entry it ALREADY holds is refused too — the gate is the
	// credential, not the id, so there is no "harmless" self-renewal either.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries/"+draftID+"/preview-link?type=story",
		"", "Bearer "+previewToken, "", e2eClientIP(t))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "CONTENT_PREVIEW_MINT_FORBIDDEN")

	// The successful preview read is metered against the tenant. This is the
	// reason collections are refused rather than merely narrowed: every delivery
	// read costs the tenant, and a preview token is the one that leaves.
	require.NoError(t, e2eApp.DeliveryCounter.Flush(context.Background(), e2eApp.ContentRepo, time.Now().UTC()))
	var reads int64
	require.NoError(t, e2ePool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(reads),0) FROM content_delivery_usage WHERE tenant_id=$1`, slug).Scan(&reads))
	require.EqualValues(t, 1, reads, "exactly the one successful preview read is billed to the tenant")
}

// A published entry with unreleased edits is the case preview exists for, and
// the one where "working copy" and "published snapshot" are actually different
// bytes — a draft-only test would pass even if the audience served the wrong
// copy, because there is only one copy to serve.
func TestE2E_PreviewShowsUnreleasedEdits(t *testing.T) {
	requireE2E(t)

	_, _, login := registerAndLogin(t, "pvwedit")
	token := login["access_token"].(string)
	slug := login["tenant_id"].(string)

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"page","label":"P","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		"Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=page",
		`{"title":"live headline"}`, "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := decodeDataMap(t, rec)["id"].(string)

	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries/"+id+"/publish?type=page",
		"", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Edit without publishing: the working copy and the snapshot now disagree.
	rec = doJSON(t, http.MethodPatch, "/api/v1/content/entries/"+id+"?type=page",
		`{"title":"rewritten, not yet live"}`, "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, decodeDataMap(t, rec)["has_unpublished_changes"].(bool))

	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries/"+id+"/preview-link?type=page",
		"", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	previewToken := decodeDataMap(t, rec)["token"].(string)

	// The preview shows the edit...
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries/"+id+"?type=page",
		"", "Bearer "+previewToken, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "rewritten, not yet live", entryField(t, decodeDataMap(t, rec), "title"))

	// ...while the public still sees what was actually published. Same route,
	// same tenant, same id — only the credential differs.
	dtok, _, err := e2eSigner.IssueDeliveryToken(uuid.New(), slug)
	require.NoError(t, err)
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries/"+id+"?type=page",
		"", "Bearer "+dtok, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "live headline", entryField(t, decodeDataMap(t, rec), "title"),
		"an ordinary delivery credential must never see the unreleased edit")
}

// entryField reads one key out of an entry's payload.
func entryField(t *testing.T, entry map[string]any, key string) any {
	t.Helper()
	data, ok := entry["data"].(map[string]any)
	require.True(t, ok, "entry has no data object: %v", entry)
	return data[key]
}
