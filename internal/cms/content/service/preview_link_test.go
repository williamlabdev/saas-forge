package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// fakeSigner records what it was asked to sign. The assertions worth making
// about minting are all about the ARGUMENTS — who is recorded as the issuer,
// which entry the token is scoped to — and a real signer would hide them inside
// an opaque string.
type fakeSigner struct {
	calls   int
	issuer  uuid.UUID
	tenant  string
	entryID uuid.UUID
}

func (f *fakeSigner) IssuePreviewToken(issuer uuid.UUID, tenant string, entry uuid.UUID) (string, time.Time, error) {
	f.calls++
	f.issuer, f.tenant, f.entryID = issuer, tenant, entry
	return "signed-for-" + entry.String(), time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC), nil
}

// newSvcWithPreview is newSvc with minting enabled.
func newSvcWithPreview() (ContentService, *fakeSigner) {
	svc, _ := newSvc()
	signer := &fakeSigner{}
	return WithPreviewLinks(svc, signer), signer
}

// The happy path, and specifically on a DRAFT: an entry that has never been
// published is the only reason to want a preview link at all, so a mint path
// that quietly required publication would be useless in exactly the case it
// exists for.
func TestPreviewLink_MintsForTheUnpublishedEntry(t *testing.T) {
	svc, signer := newSvcWithPreview()
	me := uuid.New()
	admin := ctxRoleUser("t1", "owner", me)
	e := seedEntry(t, svc, admin)

	link, err := svc.CreatePreviewLink(admin, "order", e.ID)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// The issuer is who HANDED OUT the link, not the entry's author — that is
	// the fact an audit of a leaked link needs, and the two differ whenever one
	// editor previews another's work.
	if signer.issuer != me {
		t.Fatalf("issuer = %s, want the caller (%s)", signer.issuer, me)
	}
	if signer.entryID != e.ID {
		t.Fatalf("token scoped to %s, want the entry that was loaded (%s)", signer.entryID, e.ID)
	}
	if signer.tenant != "t1" {
		t.Fatalf("tenant = %q, want t1 from the SUBJECT, never from the request", signer.tenant)
	}
	if link.EntryID != e.ID || link.Type != "order" {
		t.Fatalf("link echoes %s/%s, want %s/order", link.Type, link.EntryID, e.ID)
	}
	// The caller builds {base}/v1/{tenant}/{type}/{id}?preview_token=… — every
	// path segment but the host comes back from here, tenant included, because an
	// admin client is authenticated AS a tenant and never passed one in.
	if link.Tenant != "t1" {
		t.Fatalf("link.Tenant = %q, want the subject's tenant (t1)", link.Tenant)
	}
	if link.Token == "" || link.ExpiresAt.IsZero() {
		t.Fatalf("link = %+v, want a token and an expiry", link)
	}
}

// The escalation gate, and the reason this file exists.
//
// A preview subject passes content:read, passes guardTypeRead (a delivery
// credential is never role-refused) and passes guardOwned (confinedAuthor
// returns nil for delivery by design). So without an explicit refusal, the
// holder of ONE 30-minute preview link could mint a fresh token for ANY entry in
// the tenant, and mint another before each expiry — a credential that re-issues
// itself has no TTL, and the TTL is the whole security story for a token nothing
// records and nothing can revoke.
func TestPreviewLink_DeliveryCredentialCannotMint(t *testing.T) {
	svc, signer := newSvcWithPreview()
	admin := ctxTenant("t1")
	named := seedEntry(t, svc, admin)
	other, err := svc.CreateEntry(admin, "order", json.RawMessage(`{"title":"someone else's draft"}`))
	if err != nil {
		t.Fatalf("create second entry: %v", err)
	}

	// Both spellings of a delivery credential: the edge's own, and a preview
	// token trying to widen itself onto a different entry.
	for name, ctx := range map[string]context.Context{
		"edge's own delivery credential":              ctxDelivery("t1"),
		"a preview token widening onto another entry": ctxPreview("t1", named.ID),
	} {
		t.Run(name, func(t *testing.T) {
			before := signer.calls
			_, err := svc.CreatePreviewLink(ctx, "order", other.ID)
			appErr, ok := err.(*apperrors.AppError)
			if !ok || appErr.Code != "CONTENT_PREVIEW_MINT_FORBIDDEN" {
				t.Fatalf("got %v, want CONTENT_PREVIEW_MINT_FORBIDDEN", err)
			}
			if appErr.HTTPStatus != 403 {
				t.Fatalf("status = %d, want 403", appErr.HTTPStatus)
			}
			if signer.calls != before {
				t.Fatal("a token was signed for a credential that may not mint")
			}
		})
	}
}

// Minting is handing someone else your read of a row, so it must be refused
// wherever your own read would be. A confined editor reading a colleague's draft
// gets ErrNotFound; minting a link for it must answer the same way, and must not
// reach the signer.
func TestPreviewLink_ConfinedEditorCannotMintForAnothersDraft(t *testing.T) {
	svc, _ := newSvc()
	signer := &fakeSigner{}
	svc = WithPreviewLinks(svc, signer)
	_, bobEntry := seedConfined(t, svc)

	if _, err := svc.CreatePreviewLink(ctxRoleUser("t1", "editor", alice), "article", bobEntry); err != apperrors.ErrNotFound {
		t.Fatalf("got %v, want ErrNotFound — same answer confinement gives every other read", err)
	}
	if signer.calls != 0 {
		t.Fatal("a token was signed for a row the caller may not read")
	}
}

// An id that names no row must not produce a credential. A token minted for a
// nonexistent id would be a valid credential for a row that does not exist YET —
// and ids are client-supplied on none of these paths only by convention.
func TestPreviewLink_UnknownEntryMintsNothing(t *testing.T) {
	svc, signer := newSvcWithPreview()
	admin := ctxTenant("t1")
	seedEntry(t, svc, admin)

	if _, err := svc.CreatePreviewLink(admin, "order", uuid.New()); err == nil {
		t.Fatal("minted a link for an id that names no row")
	}
	if signer.calls != 0 {
		t.Fatal("a token was signed before the row was known to exist")
	}
}

// A deployment with no delivery key says so, rather than 500ing out of the
// signer after the caller was told the feature exists.
func TestPreviewLink_DisabledWithoutASigner(t *testing.T) {
	svc, _ := newSvc()
	admin := ctxTenant("t1")
	e := seedEntry(t, svc, admin)

	_, err := svc.CreatePreviewLink(admin, "order", e.ID)
	appErr, ok := err.(*apperrors.AppError)
	if !ok || appErr.Code != "CONTENT_PREVIEW_DISABLED" {
		t.Fatalf("got %v, want CONTENT_PREVIEW_DISABLED", err)
	}
	if appErr.HTTPStatus != 501 {
		t.Fatalf("status = %d, want 501", appErr.HTTPStatus)
	}
}

// Usage answers the tenant's PLAN, their quota ceilings and their consumption —
// commercial facts about the customer, not content. A delivery credential is
// refused it, which matters now rather than in the abstract: a preview link is
// the first delivery credential that leaves the platform's own edge by design,
// so "only the edge holds one" stopped being true (ADR-006 Amendment 2).
func TestUsage_RefusedToDeliveryCredentials(t *testing.T) {
	svc, _ := newSvcWithPreview()
	admin := ctxTenant("t1")
	e := seedEntry(t, svc, admin)

	for name, ctx := range map[string]context.Context{
		"edge's own delivery credential": ctxDelivery("t1"),
		"a preview link holder":          ctxPreview("t1", e.ID),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Usage(ctx)
			appErr, ok := err.(*apperrors.AppError)
			if !ok || appErr.Code != "CONTENT_USAGE_FORBIDDEN" {
				t.Fatalf("got %v, want CONTENT_USAGE_FORBIDDEN", err)
			}
			if appErr.HTTPStatus != 403 {
				t.Fatalf("status = %d, want 403", appErr.HTTPStatus)
			}
		})
	}

	// An ordinary tenant member still sees their own usage — the refusal above
	// must be about the credential, not about the endpoint being switched off.
	if _, err := svc.Usage(admin); err != nil {
		t.Fatalf("admin usage broke: %v", err)
	}
}
