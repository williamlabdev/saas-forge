package jwt

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var secret = []byte("test-secret-at-least-32-bytes-long!!")

var deliverySecret = []byte("delivery-secret-at-least-32-bytes!!!")

func TestSigner_IssueThenParse(t *testing.T) {
	s := NewSigner(secret, time.Hour)
	uid := uuid.New()

	tok, exp, err := s.IssueAccessToken(uid, []string{"admin", "member"}, "tenant-1", "editor", "eu", true)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().UTC().Add(time.Hour), exp, 5*time.Second)

	claims, err := s.ParseAccessToken(tok)
	require.NoError(t, err)
	assert.Equal(t, uid.String(), claims.Subject)
	assert.Equal(t, []string{"admin", "member"}, claims.Roles)
	assert.Equal(t, "tenant-1", claims.TenantID)
	assert.Equal(t, "editor", claims.TenantRole)
	assert.Equal(t, "eu", claims.Region)
	assert.True(t, claims.MFA)
}

func TestSigner_ParseRejectsWrongSecret(t *testing.T) {
	tok, _, err := NewSigner(secret, time.Hour).IssueAccessToken(uuid.New(), nil, "", "", "", false)
	require.NoError(t, err)

	other := NewSigner([]byte("a-completely-different-secret-key-xx"), time.Hour)
	_, err = other.ParseAccessToken(tok)
	require.Error(t, err)
}

func TestSigner_ParseRejectsExpiredToken(t *testing.T) {
	// Negative TTL => already expired.
	s := NewSigner(secret, -time.Minute)
	tok, _, err := s.IssueAccessToken(uuid.New(), nil, "", "", "", false)
	require.NoError(t, err)

	_, err = s.ParseAccessToken(tok)
	require.Error(t, err)
}

func TestSigner_ParseRejectsTamperedToken(t *testing.T) {
	s := NewSigner(secret, time.Hour)
	tok, _, err := s.IssueAccessToken(uuid.New(), []string{"member"}, "", "", "", false)
	require.NoError(t, err)

	// Corrupt the signature segment.
	_, err = s.ParseAccessToken(tok + "tamper")
	require.Error(t, err)
}

func TestSigner_ParseRejectsGarbage(t *testing.T) {
	s := NewSigner(secret, time.Hour)
	_, err := s.ParseAccessToken("not.a.jwt")
	require.Error(t, err)
}

// Without a dedicated delivery key the feature is OFF — deliberately no
// single-key fallback, since that mode would hand the public edge a key that
// also mints owner tokens.
func TestSigner_DeliveryDisabledWithoutDedicatedKey(t *testing.T) {
	s := NewSigner(secret, time.Hour)
	_, _, err := s.IssueDeliveryToken(uuid.New(), "tenant-1")
	require.ErrorIs(t, err, ErrDeliveryDisabled)
}

func TestSigner_DeliveryTokenRoundTrip(t *testing.T) {
	s := NewSigner(secret, time.Hour).WithDeliveryKey(deliverySecret)
	svcID := uuid.New()

	tok, exp, err := s.IssueDeliveryToken(svcID, "tenant-1")
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().UTC().Add(time.Hour), exp, 5*time.Second)

	claims, err := s.ParseAccessToken(tok)
	require.NoError(t, err)
	assert.True(t, claims.Delivery, "delivery marker must survive the round trip")
	assert.Equal(t, "tenant-1", claims.TenantID)
	assert.Equal(t, "viewer", claims.TenantRole, "delivery credentials are read-only in the RBAC matrix")
	assert.Empty(t, claims.Roles, "delivery credentials carry no platform-plane role")
	assert.False(t, claims.MFA)
}

// A normal login token must never come out marked as a delivery credential —
// the two issuing paths are separate precisely so this can't happen by accident.
func TestSigner_AccessTokenIsNeverDelivery(t *testing.T) {
	s := NewSigner(secret, time.Hour)
	tok, _, err := s.IssueAccessToken(uuid.New(), []string{"admin"}, "tenant-1", "owner", "eu", true)
	require.NoError(t, err)

	claims, err := s.ParseAccessToken(tok)
	require.NoError(t, err)
	assert.False(t, claims.Delivery)
}

// The point of a separate delivery key: a compromised public edge holds ONLY
// that key, and that key cannot express anything but a delivery credential.
func TestSigner_DeliveryKeyCannotMintNonDeliveryToken(t *testing.T) {
	edge := NewSigner(deliverySecret, time.Hour) // what a compromised edge has
	api := NewSigner(secret, time.Hour).WithDeliveryKey(deliverySecret)

	// The attacker mints a full owner token with the key they hold.
	forged, _, err := edge.IssueAccessToken(uuid.New(), []string{"admin"}, "tenant-1", "owner", "eu", true)
	require.NoError(t, err)

	_, err = api.ParseAccessToken(forged)
	require.Error(t, err, "a non-delivery token signed with the delivery key must be refused")
}

// And the reverse: with split keys the main key must not be able to assert a
// delivery claim either, or the two planes are not really separated.
// A delivery claim signed with the MAIN key must be refused, however it was
// produced — the two credential planes must not overlap.
func TestSigner_MainKeyCannotMintDeliveryToken(t *testing.T) {
	// Forge it directly: the main key is used to sign a Delivery=true claim.
	forger := NewSigner(secret, time.Hour).WithDeliveryKey(secret)
	tok, _, err := forger.IssueDeliveryToken(uuid.New(), "tenant-1")
	require.NoError(t, err)

	api := NewSigner(secret, time.Hour).WithDeliveryKey(deliverySecret)
	_, err = api.ParseAccessToken(tok)
	require.Error(t, err, "delivery claim signed with the main key must be refused")

	// Even a signer with NO delivery key must refuse it, rather than treating a
	// main-key delivery claim as valid.
	off := NewSigner(secret, time.Hour)
	_, err = off.ParseAccessToken(tok)
	require.Error(t, err, "delivery claim must be refused when delivery is disabled")
}

func TestSigner_SplitKeys_DeliveryRoundTrip(t *testing.T) {
	edge := NewSigner(secret, time.Hour).WithDeliveryKey(deliverySecret)
	api := NewSigner(secret, time.Hour).WithDeliveryKey(deliverySecret)

	tok, _, err := edge.IssueDeliveryToken(uuid.New(), "tenant-1")
	require.NoError(t, err)

	claims, err := api.ParseAccessToken(tok)
	require.NoError(t, err)
	assert.True(t, claims.Delivery)
	assert.Equal(t, "tenant-1", claims.TenantID)
}

// Normal human logins keep working untouched under split keys.
func TestSigner_SplitKeys_AccessTokenStillWorks(t *testing.T) {
	api := NewSigner(secret, time.Hour).WithDeliveryKey(deliverySecret)

	tok, _, err := api.IssueAccessToken(uuid.New(), []string{"admin"}, "tenant-1", "owner", "eu", true)
	require.NoError(t, err)

	claims, err := api.ParseAccessToken(tok)
	require.NoError(t, err)
	assert.False(t, claims.Delivery)
	assert.Equal(t, "owner", claims.TenantRole)
}

// A delivery token from a DIFFERENT delivery key must not verify.
func TestSigner_SplitKeys_RejectsForeignDeliveryKey(t *testing.T) {
	foreign := NewSigner(secret, time.Hour).WithDeliveryKey([]byte("other-delivery-key-at-least-32-byte!"))
	api := NewSigner(secret, time.Hour).WithDeliveryKey(deliverySecret)

	tok, _, err := foreign.IssueDeliveryToken(uuid.New(), "tenant-1")
	require.NoError(t, err)

	_, err = api.ParseAccessToken(tok)
	require.Error(t, err)
}
