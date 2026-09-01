package e2e_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// End-to-end proof of the ADR-004 delivery credential against the real router,
// real authz and a real database: published-only reads, refused writes, and
// read volume attributed to the tenant.
func TestE2E_DeliveryCredential(t *testing.T) {
	requireE2E(t)

	_, _, login := registerAndLogin(t, "dlv")
	token := login["access_token"].(string)
	slug := login["tenant_id"].(string)

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"article","label":"A","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		"Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=article",
		`{"title":"live"}`, "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	created := decodeDataMap(t, rec)
	liveID := created["id"].(string)
	require.Equal(t, "draft", created["status"], "new entries must start as drafts")

	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=article",
		`{"title":"secret"}`, "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code)

	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries/"+liveID+"/publish?type=article",
		"", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "published", decodeDataMap(t, rec)["status"])

	// The admin sees both states.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries?type=article",
		"", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code)
	require.EqualValues(t, 2, decodeDataMap(t, rec)["total"])

	// A delivery credential sees ONLY published entries, however it asks. Asking
	// for drafts is REFUSED rather than quietly answered with published ones:
	// silently substituting the answer lets the caller conclude no drafts exist,
	// when the truth is it may not ask.
	dtok, _, err := e2eSigner.IssueDeliveryToken(uuid.New(), slug)
	require.NoError(t, err)

	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries?type=article&status=draft",
		"", "Bearer "+dtok, "", e2eClientIP(t))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "CONTENT_STATUS_FORBIDDEN")

	// Asking for `published` — the one value this audience CAN be given — still
	// works, and yields exactly the published entry.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries?type=article&status=published",
		"", "Bearer "+dtok, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, deliveryItemCount(t, rec),
		"a delivery credential must never see a draft, however it asks")

	// It cannot write, despite the request being otherwise well-formed.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=article",
		`{"title":"x"}`, "Bearer "+dtok, "", e2eClientIP(t))
	require.GreaterOrEqual(t, rec.Code, 400, "delivery credential must not write")

	// Nor publish.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries/"+liveID+"/publish?type=article",
		"", "Bearer "+dtok, "", e2eClientIP(t))
	require.GreaterOrEqual(t, rec.Code, 400, "delivery credential must not publish")

	// The read volume is attributed to the tenant once flushed.
	require.NoError(t, e2eApp.DeliveryCounter.Flush(context.Background(), e2eApp.ContentRepo, time.Now().UTC()))
	var reads int64
	require.NoError(t, e2ePool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(reads),0) FROM content_delivery_usage WHERE tenant_id=$1`, slug).Scan(&reads))
	require.EqualValues(t, 1, reads, "exactly the one successful delivery read must be counted")

	// And it surfaces in the tenant's usage view.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/usage", "", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code)
	require.EqualValues(t, 1, decodeDataMap(t, rec)["delivery_reads_today"])
}

// A delivery token minted for one tenant must not read another's content, even
// though both are published.
func TestE2E_DeliveryCredentialIsTenantScoped(t *testing.T) {
	requireE2E(t)

	_, _, loginA := registerAndLogin(t, "dlva")
	tokenA := loginA["access_token"].(string)
	slugA := loginA["tenant_id"].(string)
	_, _, loginB := registerAndLogin(t, "dlvb")
	slugB := loginB["tenant_id"].(string)
	require.NotEqual(t, slugA, slugB)

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"note","label":"N","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		"Bearer "+tokenA, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// B's delivery credential asking for A's type: A's type does not exist in B.
	dtokB, _, err := e2eSigner.IssueDeliveryToken(uuid.New(), slugB)
	require.NoError(t, err)
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries?type=note",
		"", "Bearer "+dtokB, "", e2eClientIP(t))
	require.Equal(t, http.StatusNotFound, rec.Code,
		"tenant B's credential must not resolve tenant A's content type")
}

// Locale end-to-end: a translation publishes independently, and the delivery
// surface serves exactly one language.
func TestE2E_LocaleTranslations(t *testing.T) {
	requireE2E(t)

	_, _, login := registerAndLogin(t, "loc")
	token := login["access_token"].(string)
	slug := login["tenant_id"].(string)

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"page","label":"P","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		"Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// Source entry in English.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=page&locale=en",
		`{"title":"Hello"}`, "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	en := decodeDataMap(t, rec)
	enID := en["id"].(string)
	require.Equal(t, "en", en["locale"])
	group := en["translation_group_id"].(string)

	// Chinese translation joins the same group.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=page&locale=zh-TW&translation_of="+enID,
		`{"title":"你好"}`, "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	zh := decodeDataMap(t, rec)
	require.Equal(t, group, zh["translation_group_id"], "a translation shares the source's group")

	// A second zh-TW in the same group is a conflict.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=page&locale=zh-TW&translation_of="+enID,
		`{"title":"重複"}`, "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusConflict, rec.Code, "one row per locale per group")

	// The group view lists both languages.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries/"+enID+"/translations?type=page",
		"", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, decodeDataMap(t, rec)["items"], 2)

	// Publish English only — the whole point of one row per locale.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries/"+enID+"/publish?type=page",
		"", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	dtok, _, err := e2eSigner.IssueDeliveryToken(uuid.New(), slug)
	require.NoError(t, err)

	// Delivery sees the published English one...
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries?type=page&locale=en",
		"", "Bearer "+dtok, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, deliveryItemCount(t, rec))

	// ...and nothing in Chinese, because that translation is still a draft.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries?type=page&locale=zh-TW",
		"", "Bearer "+dtok, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 0, deliveryItemCount(t, rec),
		"an unpublished translation must not be delivered")
}

// deliveryItemCount counts a cursor-paged page. The delivery audience reports no
// `total` — keyset paging does not run COUNT(*), and the number it used to
// report was computed over the WORKING copies anyway (OD2-023 F1). Asserting on
// the page itself is what these tests actually meant.
func deliveryItemCount(t *testing.T, rec *httptest.ResponseRecorder) int {
	t.Helper()
	body := decodeDataMap(t, rec)
	require.Nil(t, body["total"], "delivery must not report a total")
	require.Nil(t, body["offset"], "delivery must not report an offset")
	items, ok := body["items"].([]any)
	if !ok && body["items"] != nil {
		t.Fatalf("items is not a list: %T", body["items"])
	}
	return len(items)
}
