package e2e_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The whole chain, end to end: a real JWT is parsed into a Subject, the service
// reads UserID off it, and the value lands in the database. The service tests
// prove the assignment against a fake and the repository tests prove the SQL,
// but neither can show that the id travelling through them is the id of the
// person who actually logged in — that join only exists here.
func TestE2E_EntryAuthorshipRecordsTheLoggedInUser(t *testing.T) {
	requireE2E(t)
	ctx := context.Background()

	userID, _, login := registerAndLogin(t, "auth")
	token := login["access_token"].(string)
	slug := login["tenant_id"].(string)

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"memo","label":"M","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		"Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=memo",
		`{"title":"v1"}`, "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	entryID := decodeDataMap(t, rec)["id"].(string)

	var createdBy, updatedBy, publishedBy *uuid.UUID
	readRow := func() {
		require.NoError(t, e2ePool.QueryRow(ctx,
			`SELECT created_by, updated_by, published_by FROM entries WHERE id = $1::uuid`,
			entryID).Scan(&createdBy, &updatedBy, &publishedBy))
	}

	readRow()
	require.NotNil(t, createdBy, "the creating user must be recorded")
	require.Equal(t, userID, createdBy.String())
	require.Nil(t, publishedBy, "a draft has no publisher")

	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries/"+entryID+"/publish?type=memo",
		"", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	readRow()
	// Guard: prove it was written by the publish, so the assertion after the
	// retract is about survival rather than about a column nobody ever wrote.
	require.NotNil(t, publishedBy, "publishing must record who did it")
	require.Equal(t, userID, publishedBy.String())

	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries/"+entryID+"/unpublish?type=memo",
		"", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	readRow()
	// ADR-014 §5.1: the retract moves status and nothing else. Checked here on
	// the real HTTP path, because the whole point of §5.1 is that an actor who
	// only has these two endpoints cannot destroy the released record with them.
	require.NotNil(t, publishedBy, "a retracted entry keeps naming who released it")
	require.Equal(t, userID, publishedBy.String())
	require.NotNil(t, updatedBy, "an unpublish still has an editor")

	var snapshot *string
	require.NoError(t, e2ePool.QueryRow(ctx,
		`SELECT published_payload::text FROM entries WHERE id = $1::uuid`, entryID).Scan(&snapshot))
	require.NotNil(t, snapshot, "a retract must not destroy the published snapshot")

	// The retained snapshot must not reach a public reader — status is what
	// excludes it now, and this is the assertion that says so on the wire rather
	// than by reading the query. Runs BEFORE the re-publish below, or the entry
	// would be live again and the check would pass for the wrong reason.
	dtokRetracted, _, err := e2eSigner.IssueDeliveryToken(uuid.New(), slug)
	require.NoError(t, err)
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries/"+entryID+"?type=memo",
		"", "Bearer "+dtokRetracted, "", e2eClientIP(t))
	require.Equal(t, http.StatusNotFound, rec.Code,
		"a retracted entry keeps its snapshot but must not be served: %s", rec.Body.String())

	// And the public wire, not just the DTO: apps/delivery forwards these bytes
	// verbatim, so what the Domain API emits for a delivery credential is exactly
	// what a reader receives.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries/"+entryID+"/publish?type=memo",
		"", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code)

	dtok, _, err := e2eSigner.IssueDeliveryToken(uuid.New(), slug)
	require.NoError(t, err)
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries?type=memo",
		"", "Bearer "+dtok, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := rec.Body.String()
	for _, field := range []string{"created_by", "updated_by", "published_by"} {
		require.NotContains(t, body, field,
			"%s must never reach the delivery wire — updated_by in particular would announce that unreleased edits exist, and who is making them", field)
	}
}
