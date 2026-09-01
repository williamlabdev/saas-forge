package jwt

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	tenantdomain "github.com/williamlabdev/saas-forge/internal/tenant/domain"
)

// Claims are access-token JWT claims.
//
// Roles carries platform-plane roles (user_roles) only; TenantRole carries the
// membership role in the active tenant. The two planes never share a claim —
// merging them would let a tenant "admin" trip the platform is_admin rule (D6/F1).
type Claims struct {
	Roles      []string `json:"roles"`
	TenantID   string   `json:"tenant_id,omitempty"`
	TenantRole string   `json:"tenant_role,omitempty"`
	Region     string   `json:"region,omitempty"`
	MFA        bool     `json:"mfa"`
	// Delivery marks a credential minted for the public content delivery edge
	// (ADR-004). It is a property of the CREDENTIAL, not a role: it can only
	// narrow what the bearer may see (published entries, reads only), never
	// widen it. Absent/false on every human login token.
	Delivery bool `json:"delivery,omitempty"`
	// PreviewEntry, when set, narrows a Delivery credential to the working copy
	// of ONE entry (ADR-006 amendment). It is only ever meaningful alongside
	// Delivery: preview is delivery-with-one-substitution, not a third plane.
	// A token carrying this without Delivery is rejected outright at parse time —
	// see ParseAccessToken — because the only key permitted to express it is the
	// delivery key, and that key may sign nothing but delivery credentials.
	PreviewEntry string `json:"preview_entry,omitempty"`
	// Kind / AgentID / Principal / AllowedTypes describe an agent credential
	// (ADR-013 §1). Like Delivery they are properties of the CREDENTIAL rather
	// than roles, and like Delivery they only narrow: an agent token is minted
	// by downgrading a tenant credential (IssueAgentToken) and its bearer
	// reaches nothing the minter could not.
	//
	// Absent on every human login token. A token carrying a malformed or
	// incomplete set is rejected outright at parse time — see ParseAccessToken
	// — for the same reason a malformed PreviewEntry is: dropping the fields
	// would leave the credential UNDERNEATH, which here is an unrestricted
	// tenant credential rather than a narrowed one.
	Kind         string   `json:"kind,omitempty"`
	AgentID      string   `json:"agent_id,omitempty"`
	Principal    string   `json:"principal,omitempty"`
	AllowedTypes []string `json:"allowed_types,omitempty"`
	jwt.RegisteredClaims
}

// Signer issues HS256 access tokens.
//
// deliverySecret, when set, is a SEPARATE key used only for public delivery
// credentials (ADR-004). It exists to bound blast radius: the public delivery
// edge must be able to mint tokens, and whatever key it holds is exposed to the
// internet-facing side. With a distinct key, a compromised edge can mint only
// delivery tokens — read-only, published-only, for content the tenant already
// chose to publish — instead of arbitrary owner tokens.
type Signer struct {
	secret         []byte
	deliverySecret []byte
	ttl            time.Duration
	agentTTL       time.Duration
}

func NewSigner(secret []byte, ttl time.Duration) *Signer {
	return &Signer{secret: secret, ttl: ttl}
}

// WithAgentTTL returns a copy that mints agent credentials with their own
// lifetime instead of the human access TTL (ruled 2026-08-06).
//
// The two TTLs are different because the two credentials fail differently. A
// human access token is short because a person is present to log in again; an
// agent runs unattended, and inheriting the 15-minute access TTL meant an agent
// process lost its credential four times an hour with nothing able to renew it
// (there is no agent refresh path — /auth/refresh reads a refresh_tokens row
// that minting an agent never creates).
//
// A long TTL is only safe alongside revocation, which is why this method and
// the agent_credentials table arrived together and why IssueAgentToken now
// demands a credential id: a 30-day bearer token that nothing can turn off is
// strictly worse than the 15-minute one it replaces.
//
// UNSET FALLS BACK TO THE SHORT TTL, deliberately. A deployment that has not
// configured AGENT_TOKEN_TTL gets the old, narrow behaviour rather than a
// month-long default it never asked for — "not configured" must read as the
// tighter answer, the same rule ErrAgentScopeUnset applies to the whitelist.
func (s *Signer) WithAgentTTL(ttl time.Duration) *Signer {
	cp := *s
	cp.agentTTL = ttl
	return &cp
}

func (s *Signer) agentLifetime() time.Duration {
	if s.agentTTL > 0 {
		return s.agentTTL
	}
	return s.ttl
}

// WithDeliveryKey returns a copy that signs and verifies delivery credentials
// with their own key. Passing an empty key leaves the delivery feature OFF —
// there is no fall back to the main key (see deliveryEnabled).
func (s *Signer) WithDeliveryKey(secret []byte) *Signer {
	cp := *s
	cp.deliverySecret = secret
	return &cp
}

// deliveryEnabled reports whether a dedicated delivery key is configured.
// Without one the delivery feature is simply OFF: no credential is issued and
// none is honoured. There is deliberately no single-key fallback — that mode
// would hand the public edge a key that also mints owner tokens, which is the
// exact failure this separation exists to prevent.
func (s *Signer) deliveryEnabled() bool { return len(s.deliverySecret) > 0 }

// ErrDeliveryDisabled is returned when delivery credentials are requested but
// no dedicated delivery key is configured (DELIVERY_JWT_SECRET_HEX).
var ErrDeliveryDisabled = errors.New("jwt: delivery credentials require a dedicated delivery key (DELIVERY_JWT_SECRET_HEX)")

func (s *Signer) IssueAccessToken(userID uuid.UUID, roles []string, tenantID, tenantRole, region string, mfa bool) (string, time.Time, error) {
	now := time.Now().UTC()
	exp := now.Add(s.ttl)
	claims := Claims{
		Roles:      roles,
		TenantID:   tenantID,
		TenantRole: tenantRole,
		Region:     region,
		MFA:        mfa,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString(s.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("jwt: sign: %w", err)
	}
	return signed, exp, nil
}

// IssueDeliveryToken mints a credential for the public content delivery edge
// (ADR-004 option A): scoped to one tenant, marked Delivery, and carrying the
// read-only membership role. It is deliberately a separate constructor from
// IssueAccessToken — the human-login path must never be able to set Delivery by
// passing one more positional argument, and this one can never grant a write
// role. The Domain API additionally refuses writes for any Delivery subject, so
// a mis-minted role cannot turn into write access.
func (s *Signer) IssueDeliveryToken(serviceID uuid.UUID, tenantSlug string) (string, time.Time, error) {
	if !s.deliveryEnabled() {
		return "", time.Time{}, ErrDeliveryDisabled
	}
	now := time.Now().UTC()
	exp := now.Add(s.ttl)
	claims := Claims{
		Roles:      nil, // no platform-plane role
		TenantID:   tenantSlug,
		TenantRole: "viewer", // read-only in the RBAC matrix
		Delivery:   true,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   serviceID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString(s.deliverySecret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("jwt: sign delivery: %w", err)
	}
	return signed, exp, nil
}

// KindHuman and KindAgent are the wire values of the actor kind claim.
//
// It is spelled out here rather than imported from authn.ActorKindAgent
// because authn imports this package to build a Subject — taking the constant
// from there would close the cycle. The two are pinned equal by a test in
// authn, the side that can see both (agent_claim_pairing_test.go); a wire
// format and the type that reads it drifting apart is exactly the kind of
// silent break no caller would notice.
const (
	KindHuman = "human"
	KindAgent = "agent"
)

// ErrNotMintable is returned when the credential offered as the minter of an
// agent token is not one this signer will downgrade from.
var ErrNotMintable = errors.New("jwt: an agent credential is minted by downgrading an ordinary tenant credential")

// ErrAgentScopeUnset is returned when an agent token is requested without a
// content type whitelist. "Not configured" must never read as "everything"
// (ADR-013 §1), and an agent that may touch nothing is not worth minting, so
// the whitelist is required to be non-empty HERE rather than left to be
// discovered as a 403 on every call.
var ErrAgentScopeUnset = errors.New("jwt: an agent credential requires a non-empty content type whitelist")

// ErrAgentCredentialUnset is returned when an agent token is requested without
// the id of the agent_credentials row that governs it. Same shape of rule as
// ErrAgentScopeUnset and for the same reason: a token with no `jti` is one that
// no revocation can ever reach, and refusing to mint it here is cheaper than
// discovering it during the incident where somebody needs it turned off.
var ErrAgentCredentialUnset = errors.New("jwt: an agent credential requires the id of its revocation record")

// ErrRoleNotMintable is returned when the requested tenant role is not one the
// minter's own role may hand out (ADR-013 補裁 S-1, ruled 2026-08-20).
//
// It is checked HERE as well as in AgentCredentialService because this is the
// only place a token actually comes into existence: the service's copy answers
// the caller in a 403 that names both roles, and this one guards every future
// call site that reaches the signer directly. The two are not redundant in the
// way a doubled validation usually is — delete this one and a caller that skips
// the service mints whatever role it asks for.
var ErrRoleNotMintable = errors.New("jwt: the requested tenant role is not one this minter may grant to an agent")

// IssueAgentToken mints an agent credential by DOWNGRADING the minter's own
// tenant credential (ADR-013 §1). A separate constructor for the same reason
// IssueDeliveryToken is one: no positional argument on the human-login path may
// turn a login into an agent.
//
// What downgrading means here, concretely:
//
//   - The platform plane is dropped (Roles: nil). A tenant owner who is also a
//     platform admin mints an agent that is neither.
//   - The tenant role is CHOSEN by the caller but bounded by the minter's own:
//     tenantRole must be in domain.MintableAgentRoles(minter.TenantRole), which
//     never contains the minter's own role. Until 2026-08-20 it was copied
//     instead, which kept "reaches nothing the minter could not" true by making
//     every agent exactly as powerful as its minter; the table keeps that
//     property and adds the ability to go strictly narrower, which is what lets
//     own_only_roles confine an agent at all (補裁 S-1).
//   - AllowedTypes narrows further, and is required (ErrAgentScopeUnset).
//   - An agent may not mint an agent: laundering one credential into another
//     with a different whitelist would make the whitelist advisory.
//   - Subject stays the PRINCIPAL's id, as it does for a preview token, so the
//     access is attributable and every path that reads UserID still names a
//     real person. Which of the two ids a write records is decided in the CMS
//     service (§2), not here.
//
// It signs with the main key: an agent credential lives in the domain plane it
// is a downgrade of. The delivery key is refused the kind claim entirely at
// parse time, so the two planes still cannot overlap.
//
// LIFECYCLE (ruled 2026-08-06, closing what this comment used to call an open
// item). credentialID is the id of the agent_credentials row that governs this
// token, carried as the `jti` claim, and the authn middleware refuses the token
// whenever that row is missing, revoked or past its expiry. It is a required
// argument rather than an optional one because the alternative — a token with
// no jti — is precisely a credential nobody can turn off, and the TTL is now
// long enough (WithAgentTTL) that "wait for it to expire" is not an answer.
//
// Minting the row is the CALLER's job, not this function's: a signer that
// touched the database would be a signer that cannot be used in the places this
// one is (parsing, tests, the delivery edge). What is enforced here is only that
// the caller cannot skip it and still get a token.
func (s *Signer) IssueAgentToken(minter *Claims, credentialID uuid.UUID, agentID, tenantRole string, allowedTypes []string) (string, time.Time, error) {
	if minter == nil || minter.TenantID == "" || minter.Delivery || minter.PreviewEntry != "" || minter.Kind != "" {
		return "", time.Time{}, ErrNotMintable
	}
	principal, err := uuid.Parse(minter.Subject)
	if err != nil {
		return "", time.Time{}, ErrNotMintable
	}
	if agentID == "" {
		return "", time.Time{}, ErrNotMintable
	}
	if credentialID == uuid.Nil {
		return "", time.Time{}, ErrAgentCredentialUnset
	}
	if len(allowedTypes) == 0 {
		return "", time.Time{}, ErrAgentScopeUnset
	}
	// An empty tenantRole is refused rather than defaulted to the minter's own.
	// Falling back to a copy would restore the pre-補裁-S-1 behaviour on exactly
	// the path where a caller forgot to decide, and "forgot" would produce the
	// widest credential available rather than the narrowest.
	if !tenantdomain.CanMintAgentRole(minter.TenantRole, tenantRole) {
		return "", time.Time{}, ErrRoleNotMintable
	}
	now := time.Now().UTC()
	exp := now.Add(s.agentLifetime())
	claims := Claims{
		Roles:        nil,             // the platform plane does not travel
		TenantID:     minter.TenantID, // same tenant, never another
		TenantRole:   tenantRole,      // chosen, but only from what the minter may grant
		Region:       minter.Region,
		Kind:         KindAgent,
		AgentID:      agentID,
		Principal:    principal.String(),
		AllowedTypes: allowedTypes,
		RegisteredClaims: jwt.RegisteredClaims{
			// ID is `jti`: the pointer from the token to its own kill switch.
			ID:        credentialID.String(),
			Subject:   principal.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString(s.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("jwt: sign agent: %w", err)
	}
	return signed, exp, nil
}

// PreviewTokenTTL bounds how long a preview link stays live.
//
// It is short and it is a constant because these tokens are STATELESS: nothing
// records that one was issued, so nothing can revoke it. Expiry is the only
// control, which makes the TTL the entire security story — not a tuning knob.
// Making it a caller argument would let each call site pick its own blast
// radius, and the call site that picks badly is the one that ships a link into
// an email thread. If revocation is ever needed, that is a table and an ADR, not
// a longer TTL here.
const PreviewTokenTTL = 30 * time.Minute

// IssuePreviewToken mints a link that shows ONE entry's working copy in the
// public delivery shape — "what this draft will look like once it ships".
//
// It signs with the DELIVERY key, not the main key, because a preview token is a
// delivery credential with one field narrowed. That placement is what keeps
// ADR-006's premise ("the delivery credential is held only by the platform's own
// edge") from being the thing that breaks when a preview link reaches an outside
// reviewer: what leaves the platform is scoped to a single entry and expires in
// PreviewTokenTTL, and it cannot be re-signed into anything wider, because the
// key that signed it may sign nothing but delivery credentials.
//
// issuerID is the human who created the link, carried as Subject so the access
// is attributable. It grants nothing: TenantRole is viewer and Delivery refuses
// every write at the Domain API, exactly as for an ordinary delivery token.
func (s *Signer) IssuePreviewToken(issuerID uuid.UUID, tenantSlug string, entryID uuid.UUID) (string, time.Time, error) {
	if !s.deliveryEnabled() {
		return "", time.Time{}, ErrDeliveryDisabled
	}
	now := time.Now().UTC()
	exp := now.Add(PreviewTokenTTL)
	claims := Claims{
		Roles:        nil,
		TenantID:     tenantSlug,
		TenantRole:   "viewer",
		Delivery:     true,
		PreviewEntry: entryID.String(),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   issuerID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString(s.deliverySecret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("jwt: sign preview: %w", err)
	}
	return signed, exp, nil
}

func (s *Signer) ParseAccessToken(raw string) (*Claims, error) {
	claims, err := s.parseWith(raw, s.secret)
	if err == nil {
		// A delivery claim is NEVER trusted from the main key — not even in
		// single-key mode. The two credential planes must not overlap.
		if claims.Delivery {
			return nil, fmt.Errorf("jwt: delivery claim not accepted from the main key")
		}
		// Checked separately rather than folded into the line above: a preview
		// claim without Delivery never reaches an audience decision (audienceFor
		// nests the preview test inside PublicDelivery), so this is not the gate
		// that prevents a leak — it is the gate that stops a token the main key
		// had no business expressing from being honoured at all. Silently
		// dropping the field instead would let such a token authenticate as an
		// ordinary user, which is a wider grant than refusing it.
		if claims.PreviewEntry != "" {
			return nil, fmt.Errorf("jwt: preview claim not accepted from the main key")
		}
		if err := validateAgentClaims(claims); err != nil {
			return nil, err
		}
		return claims, nil
	}
	if !s.deliveryEnabled() {
		return nil, err
	}
	// Fall back to the delivery key — but a token signed with it is ONLY ever
	// valid as a delivery credential. This is what keeps a compromised edge from
	// minting anything else: the key it holds cannot express a non-delivery
	// subject that the Domain API will honour.
	claims, derr := s.parseWith(raw, s.deliverySecret)
	if derr != nil {
		return nil, err // report against the main key; the token is simply invalid
	}
	if !claims.Delivery {
		return nil, fmt.Errorf("jwt: delivery key may only sign delivery credentials")
	}
	// The delivery key may not express an actor kind at all. A delivery
	// credential is already the narrowest thing this system issues; an agent
	// claim on top of it could only be an attempt to reach the agent paths with
	// the key that lives on the internet-facing side.
	if claims.Kind != "" {
		return nil, fmt.Errorf("jwt: actor kind claim not accepted from the delivery key")
	}
	return claims, nil
}

// validateAgentClaims refuses a token whose agent claims are incomplete,
// unparseable, or of a kind nothing mints.
//
// Refusing the WHOLE token, rather than dropping the fields, is the same
// judgement the preview claim gets: these fields only ever narrow, so ignoring
// an unusable set hands the bearer the credential underneath — here an
// unrestricted tenant credential, since a minted agent token carries the
// principal's own tenant role.
func validateAgentClaims(c *Claims) error {
	switch c.Kind {
	case "", KindHuman:
		// Nothing to check: no narrowing was asserted. The absent case is the
		// one every login token takes.
		return nil
	case KindAgent:
	default:
		// Includes "service": the kind exists in the vocabulary (ADR-013 §1)
		// and in the provenance column, but no signer mints it, so a token
		// carrying it came from somewhere this code does not know about.
		return fmt.Errorf("jwt: unknown actor kind %q", c.Kind)
	}
	if c.AgentID == "" {
		return fmt.Errorf("jwt: agent credential without an agent id")
	}
	if _, err := uuid.Parse(c.Principal); err != nil {
		return fmt.Errorf("jwt: agent credential without a resolvable principal")
	}
	if len(c.AllowedTypes) == 0 {
		return fmt.Errorf("jwt: agent credential without a content type whitelist")
	}
	// The jti joins this set for the same reason the other three are in it: the
	// fields are validated together, and a token missing any of them is refused
	// whole rather than honoured with the field dropped. Dropping THIS one is
	// the worst of the four — the credential underneath is not merely wider, it
	// is unrevocable, and every downstream layer would read the absence as "no
	// revocation record to check" rather than "this token is not one we minted".
	if _, err := uuid.Parse(c.ID); err != nil {
		return fmt.Errorf("jwt: agent credential without a revocation record id")
	}
	return nil
}

func (s *Signer) parseWith(raw string, key []byte) (*Claims, error) {
	tok, err := jwt.ParseWithClaims(raw, &Claims{}, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("jwt: unexpected alg %v", t.Header["alg"])
		}
		return key, nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := tok.Claims.(*Claims)
	if !ok || !tok.Valid {
		return nil, fmt.Errorf("jwt: invalid token")
	}
	return claims, nil
}
