package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// --- helpers ----------------------------------------------------------------

// idemSvc returns a service with the order type already declared, plus the
// tenant's name. Every test here creates entries of that one type.
func idemSvc(t *testing.T) (ContentService, *memRepo, context.Context) {
	t.Helper()
	svc, repo := newSvc()
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)
	return svc, repo, ctx
}

func orderPayload(t *testing.T, title string) []byte {
	t.Helper()
	return mustJSON(t, map[string]any{"title": title, "amount": 10.0, "state": "paid"})
}

// humanCtx is a second person in the same tenant — a different UserID, which is
// what human actor_key is built from.
func humanCtx(tenant string) context.Context {
	return authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(), TenantID: tenant, Roles: []string{"member"},
	})
}

// agentCtx builds an agent credential. principal and credential are separate
// arguments because the whole point of the scoping ruling is that they are
// different things: two agents minted by ONE person share a PrincipalID and
// must not share a key namespace.
func agentCtx(tenant string, principal, credential uuid.UUID, types ...string) context.Context {
	agentID := "writer-bot"
	return authn.WithSubject(context.Background(), authn.Subject{
		UserID:       principal,
		TenantID:     tenant,
		Roles:        []string{"member"},
		TenantRole:   "admin",
		Kind:         authn.ActorKindAgent,
		AgentID:      &agentID,
		PrincipalID:  &principal,
		CredentialID: &credential,
		AllowedTypes: types,
	})
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	ae, ok := apperrors.As(err)
	require.True(t, ok, "expected an application error, got %v", err)
	assert.Equal(t, code, ae.Code)
}

func countCreates(repo *memRepo) int {
	n := 0
	for _, a := range repo.activity {
		if a.Action == domain.ActivityEntryCreate {
			n++
		}
	}
	return n
}

// --- the promise ------------------------------------------------------------

// The whole point: a retry returns what the first call produced.
func TestIdempotentCreate_SameKeySameRequestReturnsTheFirstEntry(t *testing.T) {
	svc, repo, ctx := idemSvc(t)
	in := CreateLocalizedInput{Payload: orderPayload(t, "T"), IdempotencyKey: "retry-key-0001"}

	first, err := svc.CreateLocalizedEntry(ctx, "order", in)
	require.NoError(t, err)
	second, err := svc.CreateLocalizedEntry(ctx, "order", in)
	require.NoError(t, err)

	assert.Equal(t, first.ID, second.ID, "the retry must return the entry the first call made")
	assert.Len(t, repo.entries, 1, "a retry that creates a second row is the bug this table exists to prevent")
}

// The half that matters for an unattended writer. Returning the original entry
// here would answer "did you save this?" with 201 and somebody else's content.
func TestIdempotentCreate_SameKeyDifferentRequestIsRefused(t *testing.T) {
	svc, repo, ctx := idemSvc(t)
	const key = "retry-key-0001"

	_, err := svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload: orderPayload(t, "first"), IdempotencyKey: key,
	})
	require.NoError(t, err)

	_, err = svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload: orderPayload(t, "SECOND — different content, same key"), IdempotencyKey: key,
	})
	requireCode(t, err, "CONTENT_IDEMPOTENCY_KEY_REUSED")
	assert.Len(t, repo.entries, 1, "the refused call must not have created anything either")
}

// Every part of the request is digested, not just the payload: two creates that
// differ only in locale are two different entries and must not replay.
func TestIdempotentCreate_FingerprintCoversMoreThanThePayload(t *testing.T) {
	svc, _, ctx := idemSvc(t)
	const key = "retry-key-0001"
	payload := orderPayload(t, "T")

	_, err := svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload: payload, Locale: "en", IdempotencyKey: key,
	})
	require.NoError(t, err)

	_, err = svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload: payload, Locale: "fr", IdempotencyKey: key,
	})
	requireCode(t, err, "CONTENT_IDEMPOTENCY_KEY_REUSED")
}

// The default locale is digested as what it MEANS. A retry that spells out the
// default it previously omitted is the same request, and a 409 there would be
// the tool refusing a correct retry.
func TestIdempotentCreate_OmittedLocaleMatchesTheExplicitDefault(t *testing.T) {
	svc, repo, ctx := idemSvc(t)
	const key = "retry-key-0001"
	payload := orderPayload(t, "T")

	first, err := svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload: payload, IdempotencyKey: key,
	})
	require.NoError(t, err)
	second, err := svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload: payload, Locale: domain.DefaultLocale, IdempotencyKey: key,
	})
	require.NoError(t, err)

	assert.Equal(t, first.ID, second.ID)
	assert.Len(t, repo.entries, 1)
}

// The length prefixes, tested directly because no service-level call can reach
// this: it needs two requests whose parts CONCATENATE to the same bytes, and
// "ab"+"c" against "a"+"bc" is the smallest such pair.
//
// Without the prefixes these two digests are equal, and equal digests are the
// false MATCH the whole design refuses — the second caller would be handed the
// first one's entry and told it was created.
func TestFingerprintCannotCollideByRunningPartsTogether(t *testing.T) {
	payload := []byte(`{}`)
	one, err := createRequestFingerprint("ab", CreateLocalizedInput{Payload: payload, Locale: "c"})
	require.NoError(t, err)
	two, err := createRequestFingerprint("a", CreateLocalizedInput{Payload: payload, Locale: "bc"})
	require.NoError(t, err)

	assert.NotEqual(t, one, two,
		"two different requests share a digest — the parts are being concatenated without their lengths")
}

// --- scoping (william ruled 2026-08-06: tenant + issuer) --------------------

// Two people in one tenant reaching for the same obvious key ("post-1") must not
// collide. A tenant-wide namespace would hand B the entry A created.
func TestIdempotencyKeysAreScopedPerHuman(t *testing.T) {
	svc, repo, ctx := idemSvc(t)
	const key = "retry-key-0001"

	a, err := svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload: orderPayload(t, "A"), IdempotencyKey: key,
	})
	require.NoError(t, err)

	b, err := svc.CreateLocalizedEntry(humanCtx("tenant-a"), "order", CreateLocalizedInput{
		Payload: orderPayload(t, "B"), IdempotencyKey: key,
	})
	require.NoError(t, err)

	assert.NotEqual(t, a.ID, b.ID, "one person's key must not resolve another person's entry")
	assert.Len(t, repo.entries, 2)
}

// THE test for the ruling's sharp edge. Both credentials speak for the SAME
// principal — same PrincipalID, same UserID, same agent name — and differ only
// in which credential was minted.
//
// Scoping on the principal, or on AgentID (an arbitrary string chosen at mint
// time, so two people can both pick "writer-bot"), would make these two share a
// namespace and this test would return one entry.
func TestIdempotencyKeysAreScopedPerAgentCredentialNotPerPrincipal(t *testing.T) {
	svc, repo, ctx := idemSvc(t)
	principal := uuid.New()
	const key = "retry-key-0001"

	one := agentCtx("tenant-a", principal, uuid.New(), "order")
	two := agentCtx("tenant-a", principal, uuid.New(), "order")

	a, err := svc.CreateLocalizedEntry(one, "order", CreateLocalizedInput{
		Payload: orderPayload(t, "A"), IdempotencyKey: key,
	})
	require.NoError(t, err)
	b, err := svc.CreateLocalizedEntry(two, "order", CreateLocalizedInput{
		Payload: orderPayload(t, "B"), IdempotencyKey: key,
	})
	require.NoError(t, err)

	assert.NotEqual(t, a.ID, b.ID)
	assert.Len(t, repo.entries, 2)
	_ = ctx
}

// The same credential retrying IS a replay — the other side of the test above,
// so that per-credential scoping cannot be "passing" by never matching anything.
func TestIdempotencyKeysReplayForTheSameAgentCredential(t *testing.T) {
	svc, repo, _ := idemSvc(t)
	principal, credential := uuid.New(), uuid.New()
	agent := agentCtx("tenant-a", principal, credential, "order")
	in := CreateLocalizedInput{Payload: orderPayload(t, "T"), IdempotencyKey: "retry-key-0001"}

	first, err := svc.CreateLocalizedEntry(agent, "order", in)
	require.NoError(t, err)
	second, err := svc.CreateLocalizedEntry(agentCtx("tenant-a", principal, credential, "order"), "order", in)
	require.NoError(t, err)

	assert.Equal(t, first.ID, second.ID)
	assert.Len(t, repo.entries, 1)
}

// Fails closed rather than falling back to the minter's user id, which would
// merge every agent of one person into one namespace.
func TestIdempotencyKeyRefusedForAgentCredentialWithoutCredentialID(t *testing.T) {
	svc, repo, _ := idemSvc(t)
	principal := uuid.New()
	agentID := "writer-bot"
	broken := authn.WithSubject(context.Background(), authn.Subject{
		UserID: principal, TenantID: "tenant-a", Roles: []string{"member"}, TenantRole: "admin",
		Kind: authn.ActorKindAgent, AgentID: &agentID, PrincipalID: &principal,
		AllowedTypes: []string{"order"},
		// CredentialID deliberately absent.
	})

	_, err := svc.CreateLocalizedEntry(broken, "order", CreateLocalizedInput{
		Payload: orderPayload(t, "T"), IdempotencyKey: "retry-key-0001",
	})
	requireCode(t, err, "AGENT_CREDENTIAL_UNIDENTIFIED")
	assert.Empty(t, repo.entries)
}

// --- what stays unchanged ---------------------------------------------------

// No key, no promise. Every caller written before §9 passes no key, and two
// identical creates must still produce two entries — the alternative would be a
// silent behaviour change for every existing client.
func TestCreateWithoutAKeyIsUnchanged(t *testing.T) {
	svc, repo, ctx := idemSvc(t)
	in := CreateLocalizedInput{Payload: orderPayload(t, "T")}

	a, err := svc.CreateLocalizedEntry(ctx, "order", in)
	require.NoError(t, err)
	b, err := svc.CreateLocalizedEntry(ctx, "order", in)
	require.NoError(t, err)

	assert.NotEqual(t, a.ID, b.ID)
	assert.Len(t, repo.entries, 2)
}

// NormalizeKey is shared with registration, and its refusal must reach the
// caller rather than being swallowed into "no key given" — a malformed key
// silently downgraded to no-promise is a retry that duplicates.
func TestMalformedIdempotencyKeyIsRefusedNotIgnored(t *testing.T) {
	svc, repo, ctx := idemSvc(t)

	_, err := svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload: orderPayload(t, "T"), IdempotencyKey: "short",
	})
	requireCode(t, err, "INVALID_IDEMPOTENCY_KEY")
	assert.Empty(t, repo.entries)
}

// A replay created nothing, so the stream must not show a second create. One
// line per retry would make the activity feed disagree with the entry count
// about how many entries exist.
func TestReplayDoesNotRecordASecondCreate(t *testing.T) {
	svc, repo, ctx := idemSvc(t)
	in := CreateLocalizedInput{Payload: orderPayload(t, "T"), IdempotencyKey: "retry-key-0001"}

	_, err := svc.CreateLocalizedEntry(ctx, "order", in)
	require.NoError(t, err)
	require.Equal(t, 1, countCreates(repo))

	_, err = svc.CreateLocalizedEntry(ctx, "order", in)
	require.NoError(t, err)
	assert.Equal(t, 1, countCreates(repo), "a replay is not a create")
}

// --- the race ---------------------------------------------------------------

// keyRaceRepo is the loser of a two-first-tries race: the key is unspent when
// the service looks, and taken by the time it writes.
//
// The winner's record is REAL and already committed; the fake blinds the first
// LOOKUP rather than writing the winner mid-transaction. That ordering is the
// faithful one, and the first attempt at this test got it wrong: writing the
// winner inside the loser's transaction meant the loser's rollback took the
// winner's record with it, and the replay found nothing to return. A real
// winner commits in a DIFFERENT transaction, so nothing the loser rolls back
// can reach it.
type keyRaceRepo struct {
	*memRepo
	blinded bool
}

// FindEntryIdempotency misses ONCE — the instant before the winner commits.
// Afterwards it tells the truth, which is what lets the service's replay find
// the winner's entry.
func (r *keyRaceRepo) FindEntryIdempotency(ctx context.Context, tenantID, actorKey, key string) (*repository.EntryIdempotency, error) {
	if !r.blinded {
		r.blinded = true
		return nil, nil
	}
	return r.memRepo.FindEntryIdempotency(ctx, tenantID, actorKey, key)
}

func TestConcurrentFirstTriesResolveToOneEntry(t *testing.T) {
	mem := &memRepo{}
	race := &keyRaceRepo{memRepo: mem}
	ctx := ctxTenant("tenant-a")

	// The winner goes in through the UNWRAPPED fake, so its record is committed
	// before the loser starts — which is what "the other request got there first"
	// means.
	svc := NewContentService(mem, authz.NewAllowAllAuthorizer(), staticPlan(Quota{}))
	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)

	const key = "retry-key-0001"
	payload := orderPayload(t, "T")
	winner, err := svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload: payload, IdempotencyKey: key,
	})
	require.NoError(t, err)
	require.Len(t, mem.entries, 1)

	// WithTx must hand the callback THIS wrapper, or the override never applies
	// inside the transaction and no race happens.
	mem.self = race
	racing := NewContentService(race, authz.NewAllowAllAuthorizer(), staticPlan(Quota{}))
	got, err := racing.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload: payload, IdempotencyKey: key,
	})
	require.NoError(t, err, "losing the race is not the caller's problem; it gets the winner's entry")

	assert.Equal(t, winner.ID, got.ID)
	assert.Len(t, mem.entries, 1,
		"the loser's transaction must roll back — an entry left behind is the duplicate the key was sent to prevent")
	assert.Equal(t, 1, countCreates(mem), "and the abandoned create must not be in the stream either")
}
