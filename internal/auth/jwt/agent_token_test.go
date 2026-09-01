package jwt

import (
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func agentSigner() *Signer {
	return NewSigner([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
}

// minterClaims is what an ordinary tenant login parses back to.
func minterClaims(t *testing.T, s *Signer, tenantRole string) (*Claims, uuid.UUID) {
	t.Helper()
	principal := uuid.New()
	raw, _, err := s.IssueAccessToken(principal, []string{"admin"}, "tenant-a", tenantRole, "eu", true)
	require.NoError(t, err)
	claims, err := s.ParseAccessToken(raw)
	require.NoError(t, err)
	return claims, principal
}

// ADR-013 §1: an agent credential is minted by DOWNGRADING a tenant
// credential. Every assertion here is one clause of "reaches nothing its
// minter could not".
func TestIssueAgentTokenDowngradesTheMinter(t *testing.T) {
	s := agentSigner()
	minter, principal := minterClaims(t, s, "admin")

	// A credential id that is NOT the principal and NOT the tenant, so that an
	// implementation which wrote the wrong uuid into `jti` would be visible
	// rather than accidentally agreeing with the assertion below.
	credID := uuid.New()
	raw, exp, err := s.IssueAgentToken(minter, credID, "content-bot", "editor", []string{"post"})
	require.NoError(t, err)
	require.True(t, exp.After(time.Now()))

	got, err := s.ParseAccessToken(raw)
	require.NoError(t, err)

	require.Equal(t, KindAgent, got.Kind)
	require.Equal(t, "content-bot", got.AgentID)
	require.Equal(t, principal.String(), got.Principal)
	require.Equal(t, []string{"post"}, got.AllowedTypes)

	require.Nil(t, got.Roles, "the platform plane must not travel into an agent credential")
	require.Equal(t, "tenant-a", got.TenantID, "same tenant as the minter, never another")
	require.Equal(t, "editor", got.TenantRole,
		"the tenant role is the one asked for — an ADMIN minted an EDITOR, which a copy could not produce")
	require.Equal(t, principal.String(), got.Subject,
		"the subject stays the principal so every path reading UserID still names a person")
	require.Equal(t, credID.String(), got.ID,
		"the jti points at the agent_credentials row that can turn this token off")
	require.False(t, got.Delivery)
	require.Empty(t, got.PreviewEntry)
}

// An agent must not mint an agent: laundering one credential into another with
// a different whitelist would make the whitelist advisory.
func TestIssueAgentTokenRefusesNonTenantMinters(t *testing.T) {
	s := agentSigner().WithDeliveryKey([]byte("fedcba9876543210fedcba9876543210"))
	minter, _ := minterClaims(t, s, "admin")

	agentRaw, _, err := s.IssueAgentToken(minter, uuid.New(), "bot", "editor", []string{"post"})
	require.NoError(t, err)
	agentClaims, err := s.ParseAccessToken(agentRaw)
	require.NoError(t, err)

	deliveryRaw, _, err := s.IssueDeliveryToken(uuid.New(), "tenant-a")
	require.NoError(t, err)
	deliveryClaims, err := s.ParseAccessToken(deliveryRaw)
	require.NoError(t, err)

	noTenant := *minter
	noTenant.TenantID = ""

	unresolvable := *minter
	unresolvable.Subject = "not-a-uuid"

	for name, minter := range map[string]*Claims{
		"nil":              nil,
		"no tenant":        &noTenant,
		"unresolvable sub": &unresolvable,
		"an agent":         agentClaims,
		"a delivery token": deliveryClaims,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := s.IssueAgentToken(minter, uuid.New(), "bot", "editor", []string{"post"})
			require.ErrorIs(t, err, ErrNotMintable)
		})
	}
}

func TestIssueAgentTokenRequiresScopeAndIdentity(t *testing.T) {
	s := agentSigner()
	minter, _ := minterClaims(t, s, "admin")

	_, _, err := s.IssueAgentToken(minter, uuid.New(), "", "editor", []string{"post"})
	require.ErrorIs(t, err, ErrNotMintable, "an agent with no id cannot be recorded as the writer of anything")

	_, _, err = s.IssueAgentToken(minter, uuid.New(), "bot", "editor", nil)
	require.ErrorIs(t, err, ErrAgentScopeUnset, "unset is not everything (ADR-013 §1)")

	_, _, err = s.IssueAgentToken(minter, uuid.New(), "bot", "editor", []string{})
	require.ErrorIs(t, err, ErrAgentScopeUnset, "an agent permitted nothing is not worth minting")
}

// signRaw forges a token with arbitrary claims — the attacker's half of the
// fail-closed rules, which a mint-then-parse round trip cannot reach.
func signRaw(t *testing.T, secret []byte, c Claims) string {
	t.Helper()
	now := time.Now().UTC()
	c.IssuedAt = gojwt.NewNumericDate(now)
	c.ExpiresAt = gojwt.NewNumericDate(now.Add(time.Hour))
	if c.Subject == "" {
		c.Subject = uuid.NewString()
	}
	raw, err := gojwt.NewWithClaims(gojwt.SigningMethodHS256, c).SignedString(secret)
	require.NoError(t, err)
	return raw
}

// Incomplete agent claims invalidate the WHOLE token. Dropping them would be an
// escalation, not a tidy-up: what remains underneath is a tenant credential
// carrying the principal's own role, unrestricted by any whitelist.
func TestParseRefusesIncompleteAgentClaims(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	s := NewSigner(secret, time.Hour)
	principal := uuid.NewString()

	cases := map[string]Claims{
		"no agent id":       {Kind: KindAgent, Principal: principal, AllowedTypes: []string{"post"}, TenantID: "t", TenantRole: "editor"},
		"no principal":      {Kind: KindAgent, AgentID: "bot", AllowedTypes: []string{"post"}, TenantID: "t", TenantRole: "editor"},
		"bad principal":     {Kind: KindAgent, AgentID: "bot", Principal: "nope", AllowedTypes: []string{"post"}, TenantID: "t", TenantRole: "editor"},
		"no whitelist":      {Kind: KindAgent, AgentID: "bot", Principal: principal, TenantID: "t", TenantRole: "editor"},
		"empty whitelist":   {Kind: KindAgent, AgentID: "bot", Principal: principal, AllowedTypes: []string{}, TenantID: "t", TenantRole: "editor"},
		"unknown kind":      {Kind: "robot", TenantID: "t", TenantRole: "editor"},
		"unminted kind":     {Kind: "service", TenantID: "t", TenantRole: "editor"},
		"kind agent, empty": {Kind: KindAgent, TenantID: "t", TenantRole: "editor"},
		// Complete in every OTHER respect, and refused: a token with no `jti`
		// names no agent_credentials row, so no revocation can ever reach it.
		// It is last in validateAgentClaims' order, which is why every case
		// above still fails for its own reason rather than for this one.
		"no revocation record": {
			Kind: KindAgent, AgentID: "bot", Principal: principal,
			AllowedTypes: []string{"post"}, TenantID: "t", TenantRole: "editor",
		},
		"unparseable revocation record": {
			Kind: KindAgent, AgentID: "bot", Principal: principal,
			AllowedTypes: []string{"post"}, TenantID: "t", TenantRole: "editor",
			RegisteredClaims: gojwt.RegisteredClaims{ID: "not-a-uuid"},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := s.ParseAccessToken(signRaw(t, secret, c))
			require.Error(t, err, "the whole token must be refused, not the claim dropped")
		})
	}

	// The control: the same forging path produces an ACCEPTED token when the
	// agent claims are complete. Without it, a parser that refused everything
	// would pass every case above.
	ok := Claims{
		Kind: KindAgent, AgentID: "bot", Principal: principal, AllowedTypes: []string{"post"},
		TenantID: "t", TenantRole: "editor",
		RegisteredClaims: gojwt.RegisteredClaims{ID: uuid.NewString()},
	}
	got, err := s.ParseAccessToken(signRaw(t, secret, ok))
	require.NoError(t, err)
	require.Equal(t, KindAgent, got.Kind)

	// A human token is untouched by any of this.
	human, err := s.ParseAccessToken(signRaw(t, secret, Claims{TenantID: "t", TenantRole: "editor"}))
	require.NoError(t, err)
	require.Empty(t, human.Kind)
}

// The delivery key may sign nothing but delivery credentials, and an actor kind
// on top of one could only be an attempt to reach the agent paths with the key
// that lives on the internet-facing side.
func TestDeliveryKeyMayNotExpressAnActorKind(t *testing.T) {
	mainSecret := []byte("0123456789abcdef0123456789abcdef")
	deliverySecret := []byte("fedcba9876543210fedcba9876543210")
	s := NewSigner(mainSecret, time.Hour).WithDeliveryKey(deliverySecret)

	forged := Claims{
		Delivery: true, Kind: KindAgent, AgentID: "bot",
		Principal: uuid.NewString(), AllowedTypes: []string{"post"},
		TenantID: "t", TenantRole: "viewer",
	}
	_, err := s.ParseAccessToken(signRaw(t, deliverySecret, forged))
	require.Error(t, err)

	// Control: the same delivery key still signs an ordinary delivery
	// credential, so the refusal above is about the kind claim and not about
	// the key having stopped working.
	ok, err := s.ParseAccessToken(signRaw(t, deliverySecret, Claims{Delivery: true, TenantID: "t", TenantRole: "viewer"}))
	require.NoError(t, err)
	require.True(t, ok.Delivery)
}

// An agent claim signed with the MAIN key is honoured (that is where agent
// credentials live), but the main key still may not express delivery.
func TestAgentClaimIsAcceptedFromTheMainKeyOnly(t *testing.T) {
	mainSecret := []byte("0123456789abcdef0123456789abcdef")
	deliverySecret := []byte("fedcba9876543210fedcba9876543210")
	s := NewSigner(mainSecret, time.Hour).WithDeliveryKey(deliverySecret)
	minter, _ := minterClaims(t, s, "admin")

	raw, _, err := s.IssueAgentToken(minter, uuid.New(), "bot", "editor", []string{"post"})
	require.NoError(t, err)
	_, err = s.ParseAccessToken(raw)
	require.NoError(t, err)

	// The same claims signed with the delivery key are refused: an agent
	// credential is not a delivery credential.
	agentOnDeliveryKey := Claims{
		Kind: KindAgent, AgentID: "bot", Principal: uuid.NewString(),
		AllowedTypes: []string{"post"}, TenantID: "t", TenantRole: "editor",
	}
	_, err = s.ParseAccessToken(signRaw(t, deliverySecret, agentOnDeliveryKey))
	require.Error(t, err)
}

// The two TTLs are separate because the two credentials fail differently: a
// person logs back in, an unattended agent cannot (ruled 2026-08-06).
func TestAgentTokenUsesItsOwnTTL(t *testing.T) {
	const accessTTL = 15 * time.Minute
	const agentTTL = 30 * 24 * time.Hour
	s := NewSigner([]byte("0123456789abcdef0123456789abcdef"), accessTTL).WithAgentTTL(agentTTL)
	minter, principal := minterClaims(t, s, "admin")

	_, agentExp, err := s.IssueAgentToken(minter, uuid.New(), "bot", "editor", []string{"post"})
	require.NoError(t, err)
	_, humanExp, err := s.IssueAccessToken(principal, nil, "tenant-a", "editor", "eu", false)
	require.NoError(t, err)

	// Written as a span rather than as an equality against agentTTL: the point
	// is that the agent credential outlives the human one by roughly the
	// configured amount, and an assertion on the exact instant would only be
	// re-deriving time.Now().
	require.Greater(t, agentExp.Sub(humanExp), 29*24*time.Hour,
		"the agent credential must not inherit the human access TTL")

	// And the fallback: a signer with no agent TTL configured keeps the SHORT
	// one. "Not configured" reads as the tighter answer, never as a month.
	plain := NewSigner([]byte("0123456789abcdef0123456789abcdef"), accessTTL)
	plainMinter, _ := minterClaims(t, plain, "admin")
	_, plainExp, err := plain.IssueAgentToken(plainMinter, uuid.New(), "bot", "editor", []string{"post"})
	require.NoError(t, err)
	require.Less(t, time.Until(plainExp), accessTTL+time.Minute,
		"an unconfigured deployment must get the narrow TTL, not a long default")
}

// A token with no revocation record is one nothing can ever turn off, and the
// TTL is now long enough that waiting it out is not an answer.
func TestIssueAgentTokenRequiresARevocationRecord(t *testing.T) {
	s := agentSigner()
	minter, _ := minterClaims(t, s, "admin")

	_, _, err := s.IssueAgentToken(minter, uuid.Nil, "bot", "editor", []string{"post"})
	require.ErrorIs(t, err, ErrAgentCredentialUnset)
}

// 補裁 S-1 (ruled 2026-08-20): the signer is the second layer on the minting
// table. AgentCredentialService checks it first and answers with a 403 that
// names both roles; this layer exists so that a call site which never goes
// through that service cannot mint a role its minter may not grant.
//
// The load-bearing cell is "admin may not mint admin". A table replaced by
// "allow anything" still passes an owner-mints-editor test, and an ordering
// (rank(minter) >= rank(target)) still passes every cell except this one.
func TestIssueAgentTokenBoundsTheRoleByTheMinters(t *testing.T) {
	s := agentSigner()

	for name, tc := range map[string]struct {
		minterRole string
		want       string
		ok         bool
	}{
		"owner grants admin":          {"owner", "admin", true},
		"owner grants editor":         {"owner", "editor", true},
		"owner grants viewer":         {"owner", "viewer", true},
		"admin grants editor":         {"admin", "editor", true},
		"admin grants viewer":         {"admin", "viewer", true},
		"owner may not grant owner":   {"owner", "owner", false},
		"admin may not grant admin":   {"admin", "admin", false},
		"admin may not grant owner":   {"admin", "owner", false},
		"editor may not grant at all": {"editor", "viewer", false},
		"viewer may not grant at all": {"viewer", "viewer", false},
		"no role asked for":           {"owner", "", false},
		"role that does not exist":    {"owner", "superuser", false},
	} {
		t.Run(name, func(t *testing.T) {
			minter, _ := minterClaims(t, s, tc.minterRole)
			raw, _, err := s.IssueAgentToken(minter, uuid.New(), "bot", tc.want, []string{"post"})
			if !tc.ok {
				require.ErrorIs(t, err, ErrRoleNotMintable)
				require.Empty(t, raw, "a refused mint must not also return a usable token")
				return
			}
			require.NoError(t, err)
			got, err := s.ParseAccessToken(raw)
			require.NoError(t, err)
			require.Equal(t, tc.want, got.TenantRole)
			require.NotEqual(t, minter.TenantRole, got.TenantRole,
				"no cell of this table grants the minter's own role")
		})
	}
}
