package e2e_test

// TKT-R1 PR2 e2e: self-serve tenant provisioning, tenant isolation on the
// content surface, the no-membership boundary, and the legacy-refresh rollout
// path.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/pkg/response"
)

// registerAndLogin creates a fresh self-serve user and returns its login data.
func registerAndLogin(t *testing.T, usernamePrefix string) (userID, email string, loginData map[string]any) {
	t.Helper()
	email = fmt.Sprintf("%s-%s@example.com", usernamePrefix, uuid.NewString()[:8])
	rec := doJSON(t, http.MethodPost, "/api/v1/users",
		fmt.Sprintf(`{"username":%q,"email":%q,"password":"password12"}`, usernamePrefix, email), "", "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	userID = decodeDataMap(t, rec)["id"].(string)

	recLogin := doJSON(t, http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"email":%q,"password":"password12"}`, email), "", "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, recLogin.Code, recLogin.Body.String())
	return userID, email, decodeDataMap(t, recLogin)
}

// jwtClaims decodes the (already signature-verified upstream) payload segment.
func jwtClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var claims map[string]any
	require.NoError(t, json.Unmarshal(raw, &claims))
	return claims
}

func decodeErrorCode(t *testing.T, body string) string {
	t.Helper()
	var env response.Envelope
	require.NoError(t, json.Unmarshal([]byte(body), &env))
	require.NotNil(t, env.Error)
	return env.Error.Code
}

// e2e #1 + registration provisioning + JWT namespace regression (§7).
func TestE2E_SelfServeProvisioningAndTenantIsolation(t *testing.T) {
	requireE2E(t)
	ctx := context.Background()

	userA, _, loginA := registerAndLogin(t, "tenanta")
	tokenA := loginA["access_token"].(string)

	// Registration provisioned exactly one owner membership, atomically.
	var count int
	var role string
	require.NoError(t, e2ePool.QueryRow(ctx, `
		SELECT COUNT(*), MIN(m.role) FROM memberships m WHERE m.user_id = $1::uuid
	`, userA).Scan(&count, &role))
	assert.Equal(t, 1, count, "exactly one membership per self-serve registration")
	assert.Equal(t, "owner", role)

	// Login surfaces the active tenant; the slug is opaque (D10).
	slugA, _ := loginA["tenant_id"].(string)
	require.NotEmpty(t, slugA, "login must carry the active tenant")
	assert.True(t, strings.HasPrefix(slugA, "t_"), "opaque slug, got %q", slugA)

	// JWT regression (§7 last item): tenant_id non-empty, tenant role in its
	// own claim, and the platform roles claim EMPTY — a fresh self-serve user
	// gets no global role (D12), and tenant roles never leak into it (D6).
	claims := jwtClaims(t, tokenA)
	assert.Equal(t, slugA, claims["tenant_id"])
	assert.Equal(t, "owner", claims["tenant_role"])
	if roles, ok := claims["roles"].([]any); ok {
		assert.Empty(t, roles, "self-serve registration must not grant platform roles (D12/D6)")
	}

	// A defines a type and writes an entry in its own tenant.
	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"article","label":"Article","fields":[{"key":"title","type":"text","label":"Title","required":true}]}`,
		"Bearer "+tokenA, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=article",
		`{"title":"hello from tenant A"}`, "Bearer "+tokenA, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	entryID := decodeDataMap(t, rec)["id"].(string)

	// A can read it back.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries/"+entryID+"?type=article", "",
		"Bearer "+tokenA, "", e2eClientIP(t))
	assert.Equal(t, http.StatusOK, rec.Code)

	// B gets its own tenant and creates the SAME type name + an entry of its
	// own, so the cross-tenant GET below exercises entry-level scoping (the
	// tenant_id predicate on entries), not just the type lookup missing.
	_, _, loginB := registerAndLogin(t, "tenantb")
	tokenB := loginB["access_token"].(string)
	require.NotEqual(t, slugA, loginB["tenant_id"], "each registration gets its own tenant")

	rec = doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"article","label":"Article","fields":[{"key":"title","type":"text","label":"Title","required":true}]}`,
		"Bearer "+tokenB, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=article",
		`{"title":"hello from tenant B"}`, "Bearer "+tokenB, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	entryB := decodeDataMap(t, rec)["id"].(string)

	// Cross-tenant reads must 404 in BOTH directions — not 403 — so existence
	// leaks nothing across tenants (isolation guard, doubles as R6a).
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries/"+entryID+"?type=article", "",
		"Bearer "+tokenB, "", e2eClientIP(t))
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries/"+entryB+"?type=article", "",
		"Bearer "+tokenA, "", e2eClientIP(t))
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// Idempotent replay of a registration must not provision a second tenant.
	emailR := fmt.Sprintf("replay-%s@example.com", uuid.NewString()[:8])
	key := "idem-" + uuid.NewString()
	body := fmt.Sprintf(`{"username":"replayuser","email":%q,"password":"password12"}`, emailR)
	rec1 := doJSON(t, http.MethodPost, "/api/v1/users", body, "", key, e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec1.Code)
	replayID := decodeDataMap(t, rec1)["id"].(string)
	rec2 := doJSON(t, http.MethodPost, "/api/v1/users", body, "", key, e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec2.Code)
	var replayCount int
	require.NoError(t, e2ePool.QueryRow(ctx, `
		SELECT COUNT(*) FROM memberships WHERE user_id = $1::uuid
	`, replayID).Scan(&replayCount))
	assert.Equal(t, 1, replayCount, "idempotent replay must not provision twice (D8)")
}

// e2e #3 (§6): a user with no membership is explicitly rejected on content —
// never pooled into an empty-tenant bucket.
func TestE2E_NoMembershipUserContentRejected(t *testing.T) {
	requireE2E(t)
	ctx := context.Background()

	userC, email, _ := registerAndLogin(t, "nomember")
	_, err := e2ePool.Exec(ctx, `DELETE FROM memberships WHERE user_id = $1::uuid`, userC)
	require.NoError(t, err)

	// Fresh login after revocation: succeeds (platform operators have no
	// membership either) but the token must carry no tenant.
	recLogin := doJSON(t, http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"email":%q,"password":"password12"}`, email), "", "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, recLogin.Code, "membershipless users (platform operators) must still log in")
	data := decodeDataMap(t, recLogin)
	token := data["access_token"].(string)
	tenantID, _ := data["tenant_id"].(string)
	assert.Empty(t, tenantID)

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"sneaky","label":"Sneaky","fields":[{"key":"a","type":"text","label":"A"}]}`,
		"Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "CONTENT_NO_TENANT", decodeErrorCode(t, rec.Body.String()))
}

// Rollout regression #6 (§6/§7): a refresh token stored before tenant tracking
// (tenant_id NULL) degrades to the user's default membership on refresh.
func TestE2E_RefreshLegacyNullTenantGainsDefault(t *testing.T) {
	requireE2E(t)
	ctx := context.Background()

	userD, _, loginD := registerAndLogin(t, "legacyrt")
	slug := loginD["tenant_id"].(string)
	require.NotEmpty(t, slug)
	refreshToken := loginD["refresh_token"].(string)

	// Simulate an in-flight pre-PR2 refresh token.
	tag, err := e2ePool.Exec(ctx, `UPDATE refresh_tokens SET tenant_id = NULL WHERE user_id = $1::uuid`, userD)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())

	rec := doJSON(t, http.MethodPost, "/api/v1/auth/refresh",
		fmt.Sprintf(`{"refresh_token":%q}`, refreshToken), "", "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	data := decodeDataMap(t, rec)
	assert.Equal(t, slug, data["tenant_id"], "legacy refresh must resolve the default membership, not the empty bucket")

	claims := jwtClaims(t, data["access_token"].(string))
	assert.Equal(t, slug, claims["tenant_id"])
	assert.Equal(t, "owner", claims["tenant_role"])
}

// e2e #2 (§7): a multi-membership user switches tenant; the new token's
// tenant_id changes and only the switched tenant's data is visible (PR3/D5).
func TestE2E_SwitchTenantChangesVisibility(t *testing.T) {
	requireE2E(t)
	ctx := context.Background()

	// A owns tenant 1, B owns tenant 2. A is also granted editor in tenant 2.
	userA, emailA, loginA := registerAndLogin(t, "switcha")
	tokenA := loginA["access_token"].(string)
	slug1 := loginA["tenant_id"].(string)

	_, _, loginB := registerAndLogin(t, "switchb")
	tokenB := loginB["access_token"].(string)
	slug2 := loginB["tenant_id"].(string)

	tag, err := e2ePool.Exec(ctx, `
		INSERT INTO memberships (user_id, tenant_id, role)
		SELECT $1::uuid, t.id, 'editor' FROM tenants t WHERE t.slug = $2
	`, userA, slug2)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())

	// Each tenant gets an entry of the same type name.
	for _, tc := range []struct{ token, title string }{{tokenA, "in tenant 1"}, {tokenB, "in tenant 2"}} {
		rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
			`{"name":"note","label":"Note","fields":[{"key":"title","type":"text","label":"Title","required":true}]}`,
			"Bearer "+tc.token, "", e2eClientIP(t))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}
	rec := doJSON(t, http.MethodPost, "/api/v1/content/entries?type=note",
		`{"title":"in tenant 1"}`, "Bearer "+tokenA, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code)
	entry1 := decodeDataMap(t, rec)["id"].(string)
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=note",
		`{"title":"in tenant 2"}`, "Bearer "+tokenB, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code)
	entry2 := decodeDataMap(t, rec)["id"].(string)

	// Re-login A: now multi-membership, so available_tenants must be listed
	// and the default stays the earliest membership (tenant 1).
	recLogin := doJSON(t, http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"email":%q,"password":"password12"}`, emailA), "", "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, recLogin.Code)
	dataA := decodeDataMap(t, recLogin)
	assert.Equal(t, slug1, dataA["tenant_id"])
	avail, _ := dataA["available_tenants"].([]any)
	assert.Len(t, avail, 2, "multi-membership login lists all tenants")
	tokenA = dataA["access_token"].(string)

	// Pre-switch baseline: A on tenant 1 sees entry1, not entry2.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries/"+entry1+"?type=note", "",
		"Bearer "+tokenA, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries/"+entry2+"?type=note", "",
		"Bearer "+tokenA, "", e2eClientIP(t))
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// Switch to tenant 2. switch-tenant shares the login/refresh rate limiter,
	// and this test's base IP already spent its budget on three logins — give
	// each throttled call below its own client IP.
	rec = doJSON(t, http.MethodPost, "/api/v1/auth/switch-tenant",
		fmt.Sprintf(`{"tenant":%q}`, slug2), "Bearer "+tokenA, "", "198.51.100.78")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	switched := decodeDataMap(t, rec)
	assert.Equal(t, slug2, switched["tenant_id"])
	tokenSwitched := switched["access_token"].(string)

	claims := jwtClaims(t, tokenSwitched)
	assert.Equal(t, slug2, claims["tenant_id"])
	assert.Equal(t, "editor", claims["tenant_role"], "role comes from the target membership")

	// Visibility flips: tenant 2's entry readable, tenant 1's entry gone.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries/"+entry2+"?type=note", "",
		"Bearer "+tokenSwitched, "", e2eClientIP(t))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries/"+entry1+"?type=note", "",
		"Bearer "+tokenSwitched, "", e2eClientIP(t))
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// D5: the pre-switch access token stays valid until its TTL — still
	// scoped to tenant 1.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/entries/"+entry1+"?type=note", "",
		"Bearer "+tokenA, "", e2eClientIP(t))
	assert.Equal(t, http.StatusOK, rec.Code, "old access token must survive the switch")

	// The switched refresh token stays on tenant 2. Distinct client IP: this
	// test already spent its per-IP auth budget on three logins.
	rec = doJSON(t, http.MethodPost, "/api/v1/auth/refresh",
		fmt.Sprintf(`{"refresh_token":%q}`, switched["refresh_token"].(string)), "", "", "198.51.100.77")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, slug2, decodeDataMap(t, rec)["tenant_id"])

	// Switching to a tenant without membership is a 403 that names no tenant.
	rec = doJSON(t, http.MethodPost, "/api/v1/auth/switch-tenant",
		fmt.Sprintf(`{"tenant":%q}`, loginB["tenant_id"]), "Bearer "+tokenB, "", "198.51.100.78")
	require.Equal(t, http.StatusOK, rec.Code, "sanity: B can re-switch to own tenant")
	rec = doJSON(t, http.MethodPost, "/api/v1/auth/switch-tenant",
		`{"tenant":"t_nonexistent"}`, "Bearer "+tokenB, "", "198.51.100.79")
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "AUTH_NOT_TENANT_MEMBER", decodeErrorCode(t, rec.Body.String()))

	// Unauthenticated switch is rejected.
	rec = doJSON(t, http.MethodPost, "/api/v1/auth/switch-tenant",
		fmt.Sprintf(`{"tenant":%q}`, slug2), "", "", "198.51.100.79")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// PR-invite: full invite → accept → switch flow, plus permission and
// email-binding rejections.
func TestE2E_InviteAcceptAndSwitch(t *testing.T) {
	requireE2E(t)

	// Owner A invites B (already registered with their own tenant).
	_, _, loginA := registerAndLogin(t, "invowner")
	tokenA := loginA["access_token"].(string)
	slugA := loginA["tenant_id"].(string)
	_, emailB, loginB := registerAndLogin(t, "invitee")
	tokenB := loginB["access_token"].(string)

	rec := doJSON(t, http.MethodPost, "/api/v1/tenants/invites",
		fmt.Sprintf(`{"email":%q,"role":"editor"}`, emailB), "Bearer "+tokenA, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	inv := decodeDataMap(t, rec)
	rawToken := inv["token"].(string)
	assert.Equal(t, slugA, inv["tenant_id"])

	// A stranger holding the token cannot accept it (email binding).
	_, _, loginC := registerAndLogin(t, "stranger")
	rec = doJSON(t, http.MethodPost, "/api/v1/tenants/invites/accept",
		fmt.Sprintf(`{"token":%q}`, rawToken), "Bearer "+loginC["access_token"].(string), "", e2eClientIP(t))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "INVITE_EMAIL_MISMATCH", decodeErrorCode(t, rec.Body.String()))

	// B accepts, then switches into A's tenant as editor.
	rec = doJSON(t, http.MethodPost, "/api/v1/tenants/invites/accept",
		fmt.Sprintf(`{"token":%q}`, rawToken), "Bearer "+tokenB, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	acc := decodeDataMap(t, rec)
	assert.Equal(t, slugA, acc["tenant_id"])
	assert.Equal(t, "editor", acc["role"])

	rec = doJSON(t, http.MethodPost, "/api/v1/auth/switch-tenant",
		fmt.Sprintf(`{"tenant":%q}`, slugA), "Bearer "+tokenB, "", "198.51.100.80")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	claims := jwtClaims(t, decodeDataMap(t, rec)["access_token"].(string))
	assert.Equal(t, slugA, claims["tenant_id"])
	assert.Equal(t, "editor", claims["tenant_role"])

	// Single-use: replaying the token reports it as used.
	rec = doJSON(t, http.MethodPost, "/api/v1/tenants/invites/accept",
		fmt.Sprintf(`{"token":%q}`, rawToken), "Bearer "+tokenB, "", e2eClientIP(t))
	require.Equal(t, http.StatusGone, rec.Code, rec.Body.String())
	assert.Equal(t, "INVITE_USED", decodeErrorCode(t, rec.Body.String()))

	// B is an editor in A's tenant — editors cannot invite (D3: 管成員 is
	// owner/admin). B's own-tenant token (owner) can.
	rec = doJSON(t, http.MethodPost, "/api/v1/auth/switch-tenant",
		fmt.Sprintf(`{"tenant":%q}`, slugA), "Bearer "+tokenB, "", "198.51.100.80")
	require.Equal(t, http.StatusOK, rec.Code)
	tokenBinA := decodeDataMap(t, rec)["access_token"].(string)
	rec = doJSON(t, http.MethodPost, "/api/v1/tenants/invites",
		`{"email":"x@example.com","role":"viewer"}`, "Bearer "+tokenBinA, "", e2eClientIP(t))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// Bad token → 404; unauthenticated accept → 401.
	rec = doJSON(t, http.MethodPost, "/api/v1/tenants/invites/accept",
		`{"token":"0000000000000000000000000000000000000000000000000000000000000000"}`,
		"Bearer "+tokenB, "", e2eClientIP(t))
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	rec = doJSON(t, http.MethodPost, "/api/v1/tenants/invites/accept",
		fmt.Sprintf(`{"token":%q}`, rawToken), "", "", e2eClientIP(t))
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
}

// PR-invite headline flow: invite an email that has NOT registered yet; the
// user registers afterwards and accepts. This also pins that the invite
// service and registration normalize emails identically — the only thing
// making the blind-index hashes comparable.
func TestE2E_InviteBeforeRegistration(t *testing.T) {
	requireE2E(t)

	_, _, loginA := registerAndLogin(t, "earlyinv")
	tokenA := loginA["access_token"].(string)
	slugA := loginA["tenant_id"].(string)

	// Invite an address that doesn't exist yet — mixed case exercises the
	// normalization agreement (whitespace is rejected by validation upfront,
	// same as login).
	lateEmail := fmt.Sprintf("Late-%s@Example.COM", uuid.NewString()[:8])
	rec := doJSON(t, http.MethodPost, "/api/v1/tenants/invites",
		fmt.Sprintf(`{"email":%q,"role":"viewer"}`, lateEmail), "Bearer "+tokenA, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rawToken := decodeDataMap(t, rec)["token"].(string)

	// Now the invitee registers with the same address (different casing).
	regBody := fmt.Sprintf(`{"username":"latecomer","email":%q,"password":"password12"}`, strings.ToLower(lateEmail))
	rec = doJSON(t, http.MethodPost, "/api/v1/users", regBody, "", "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	recLogin := doJSON(t, http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"email":%q,"password":"password12"}`, strings.ToLower(lateEmail)), "", "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, recLogin.Code)
	tokenLate := decodeDataMap(t, recLogin)["access_token"].(string)

	rec = doJSON(t, http.MethodPost, "/api/v1/tenants/invites/accept",
		fmt.Sprintf(`{"token":%q}`, rawToken), "Bearer "+tokenLate, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	acc := decodeDataMap(t, rec)
	assert.Equal(t, slugA, acc["tenant_id"])
	assert.Equal(t, "viewer", acc["role"])
}

// PR4 / plan §7 #5 (D12 settled): platform admin gets NO blanket over content.
// Their content capability comes solely from their membership role.
func TestE2E_PlatformAdminNoContentBlanket(t *testing.T) {
	requireE2E(t)
	ctx := context.Background()

	userID, email, _ := registerAndLogin(t, "padmin")
	require.NoError(t, assignRoleDirect(ctx, userID, "admin"))
	// Demote their own-tenant membership to viewer so any write success could
	// only come from a platform-admin blanket.
	tag, err := e2ePool.Exec(ctx, `UPDATE memberships SET role = 'viewer' WHERE user_id = $1::uuid`, userID)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())

	// Fresh login: token carries platform roles=[admin] + tenant_role=viewer.
	recLogin := doJSON(t, http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"email":%q,"password":"password12"}`, email), "", "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, recLogin.Code)
	token := decodeDataMap(t, recLogin)["access_token"].(string)
	claims := jwtClaims(t, token)
	require.Contains(t, claims["roles"], "admin", "precondition: platform admin")
	require.Equal(t, "viewer", claims["tenant_role"])

	// Platform plane intact: admin can list users.
	rec := doJSON(t, http.MethodGet, "/api/v1/users?limit=5", "", "Bearer "+token, "", e2eClientIP(t))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Tenant plane: viewer verbs only — reads pass, writes 403 despite admin.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/types", "", "Bearer "+token, "", e2eClientIP(t))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"blanket","label":"B","fields":[{"key":"a","type":"text","label":"A"}]}`,
		"Bearer "+token, "", e2eClientIP(t))
	assert.Equal(t, http.StatusForbidden, rec.Code, "platform admin must not blanket-write content (D12)")
	// Nor can they mint invites on a viewer membership.
	rec = doJSON(t, http.MethodPost, "/api/v1/tenants/invites",
		`{"email":"x@example.com","role":"viewer"}`, "Bearer "+token, "", e2eClientIP(t))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TKT-R2 verification (threat model §4): with dev headers disabled — as this
// harness runs — forged X-User-*/X-Tenant-* headers must be ignored entirely;
// content access without a real JWT is 401.
func TestE2E_ForgedDevHeadersIgnored(t *testing.T) {
	requireE2E(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/content/types", nil)
	req.Header.Set("X-User-Id", uuid.NewString())
	req.Header.Set("X-User-Roles", "admin")
	req.Header.Set("X-Tenant-Id", "tenant_acme")
	req.Header.Set("X-Tenant-Role", "owner")
	rec := httptest.NewRecorder()
	e2eHandler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"forged identity headers must not authenticate when AUTH_DEV_HEADERS=false")
}

// TKT-R4a: the per-tenant content quota backstop returns 429 over HTTP once
// the tenant hits its entry ceiling (harness sets both caps to 5).
// assignPlan seeds a low-limit plan (idempotent) and points the tenant at it,
// so R4b enforcement (per-plan limits) can be tripped without touching the
// shared 'free' seed.
func assignPlan(t *testing.T, slug, plan string, maxTypes, maxEntries, softPct int) {
	t.Helper()
	ctx := context.Background()
	_, err := e2ePool.Exec(ctx, `
		INSERT INTO plans (name, max_types, max_entries, max_fields_per_type, max_entry_bytes, soft_threshold_pct)
		VALUES ($1, $2, $3, 100, 1048576, $4)
		ON CONFLICT (name) DO UPDATE SET max_types = EXCLUDED.max_types,
			max_entries = EXCLUDED.max_entries, soft_threshold_pct = EXCLUDED.soft_threshold_pct
	`, plan, maxTypes, maxEntries, softPct)
	require.NoError(t, err)
	tag, err := e2ePool.Exec(ctx, `UPDATE tenants SET plan = $2 WHERE slug = $1`, slug, plan)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected(), "tenant %s not found", slug)
}

func TestE2E_ContentQuotaBackstop(t *testing.T) {
	requireE2E(t)

	_, _, login := registerAndLogin(t, "quota")
	token := login["access_token"].(string)
	slug := login["tenant_id"].(string)
	// Cap entries at 5 with an 80% soft line; the plan is resolved per-request
	// from the DB, so no token refresh is needed.
	assignPlan(t, slug, "e2e_quota_entries", 100, 5, 80)

	rec := doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"widget","label":"W","fields":[{"key":"title","type":"text","label":"Title","required":true}]}`,
		"Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// Five entries fill the plan's ceiling; the 4th (4/5 = 80%) crosses the
	// soft line and its 201 carries the usage-warning header (D3).
	for i := 0; i < 5; i++ {
		rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=widget",
			fmt.Sprintf(`{"title":"e%d"}`, i), "Bearer "+token, "", e2eClientIP(t))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		if i == 3 {
			assert.Equal(t, "entries=4/5", rec.Header().Get("X-Content-Usage-Warning"),
				"the write that crosses the soft line must carry the warning header")
		}
		if i < 3 {
			assert.Empty(t, rec.Header().Get("X-Content-Usage-Warning"), "below soft line: no header")
		}
	}

	// The sixth trips the hard limit → 429.
	rec = doJSON(t, http.MethodPost, "/api/v1/content/entries?type=widget",
		`{"title":"over"}`, "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Equal(t, "CONTENT_QUOTA_EXCEEDED", decodeErrorCode(t, rec.Body.String()))

	// usage API reflects plan + live counts.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/usage", "", "Bearer "+token, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	usage := decodeDataMap(t, rec)
	assert.Equal(t, "e2e_quota_entries", usage["plan"])
	entries := usage["entries"].(map[string]any)
	assert.EqualValues(t, 5, entries["used"])
	assert.EqualValues(t, 5, entries["limit"])

	// A different tenant on its own low-type plan exercises the type-count cap.
	_, _, login2 := registerAndLogin(t, "quotab")
	token2 := login2["access_token"].(string)
	assignPlan(t, login2["tenant_id"].(string), "e2e_quota_types", 5, 100000, 80)
	for i := 0; i < 5; i++ {
		rec = doJSON(t, http.MethodPost, "/api/v1/content/types",
			fmt.Sprintf(`{"name":"t%d","label":"T","fields":[{"key":"title","type":"text","label":"Title"}]}`, i),
			"Bearer "+token2, "", e2eClientIP(t))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}
	rec = doJSON(t, http.MethodPost, "/api/v1/content/types",
		`{"name":"t5","label":"T","fields":[{"key":"title","type":"text","label":"Title"}]}`,
		"Bearer "+token2, "", e2eClientIP(t))
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Equal(t, "CONTENT_QUOTA_EXCEEDED", decodeErrorCode(t, rec.Body.String()))
}

// TKT-R4b PR3 (D7): a platform admin changes a tenant's plan via the
// platformops endpoint; the change is real (usage reflects the new limits)
// and a non-admin is refused.
func TestE2E_PlatformAdminSetsTenantPlan(t *testing.T) {
	requireE2E(t)
	ctx := context.Background()

	// The target tenant (a normal self-serve owner, starts on free).
	targetUser, _, targetLogin := registerAndLogin(t, "planned")
	_ = targetUser
	targetToken := targetLogin["access_token"].(string)
	targetSlug := targetLogin["tenant_id"].(string)

	// A platform admin (global user_roles 'admin').
	adminUser, adminEmail, _ := registerAndLogin(t, "platadmin")
	require.NoError(t, assignRoleDirect(ctx, adminUser, "admin"))
	recLogin := doJSON(t, http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"email":%q,"password":"password12"}`, adminEmail), "", "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, recLogin.Code)
	adminToken := decodeDataMap(t, recLogin)["access_token"].(string)

	// Seed a distinctive plan and have the admin assign it.
	_, err := e2ePool.Exec(ctx, `
		INSERT INTO plans (name, max_types, max_entries, max_fields_per_type, max_entry_bytes)
		VALUES ('e2e_admin_plan', 3, 7, 100, 1048576) ON CONFLICT (name) DO NOTHING`)
	require.NoError(t, err)

	rec := doJSON(t, http.MethodPut, "/api/v1/platform/tenants/"+targetSlug+"/plan",
		`{"plan":"e2e_admin_plan"}`, "Bearer "+adminToken, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "e2e_admin_plan", decodeDataMap(t, rec)["plan"])

	// The change is real: the target tenant's usage now reflects the new plan.
	rec = doJSON(t, http.MethodGet, "/api/v1/content/usage", "", "Bearer "+targetToken, "", e2eClientIP(t))
	require.Equal(t, http.StatusOK, rec.Code)
	usage := decodeDataMap(t, rec)
	assert.Equal(t, "e2e_admin_plan", usage["plan"])
	assert.EqualValues(t, 7, usage["entries"].(map[string]any)["limit"])

	// Unknown plan → 422.
	rec = doJSON(t, http.MethodPut, "/api/v1/platform/tenants/"+targetSlug+"/plan",
		`{"plan":"no_such_plan"}`, "Bearer "+adminToken, "", e2eClientIP(t))
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())

	// A non-admin (the target tenant's own owner) cannot change plans.
	rec = doJSON(t, http.MethodPut, "/api/v1/platform/tenants/"+targetSlug+"/plan",
		`{"plan":"free"}`, "Bearer "+targetToken, "", e2eClientIP(t))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}
