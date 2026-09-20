package service

import (
	"context"
	"time"

	"github.com/google/uuid"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// PreviewSigner mints preview credentials. It is one method wide on purpose:
// the content service must be able to issue a preview link without being able
// to issue anything else, and *jwt.Signer can issue access tokens, refresh
// tokens and full delivery credentials. Depending on the narrow interface makes
// "the CMS accidentally minted a login" not merely unlikely but unspellable.
type PreviewSigner interface {
	IssuePreviewToken(issuerID uuid.UUID, tenantSlug string, entryID uuid.UUID) (string, time.Time, error)
}

// WithPreviewSigner enables preview links. Without it the endpoint answers 501
// rather than failing obscurely deeper down — the same shape as WithObjectStore,
// and for the same reason: a deployment with no delivery key configured cannot
// mint delivery credentials at all, and that is a configuration fact the caller
// deserves to be told plainly.
func (s *contentService) WithPreviewSigner(signer PreviewSigner) *contentService {
	s.preview = signer
	return s
}

// WithPreviewLinks enables preview minting on an existing service, mirroring
// WithMediaStore: the two composition roots opt in without either of them
// gaining another constructor argument.
//
// A nil signer is passed through rather than stored, so a deployment without a
// delivery key lands on ErrPreviewDisabled instead of a nil-interface panic —
// the caller writes `WithPreviewLinks(svc, maybeSigner)` unconditionally.
func WithPreviewLinks(svc ContentService, signer PreviewSigner) ContentService {
	if signer == nil {
		return svc
	}
	if cs, ok := svc.(*contentService); ok {
		return cs.WithPreviewSigner(signer)
	}
	return svc
}

// ErrPreviewDisabled reports that this deployment has no delivery signing key,
// so no preview credential can exist here.
var ErrPreviewDisabled = apperrors.New("CONTENT_PREVIEW_DISABLED", "preview links require a delivery signing key", 501)

// PreviewLinkDTO is a minted preview credential.
//
// It carries a token and an expiry, NOT a URL. The service does not know the
// public hostname of the delivery edge — that is deployment configuration, and
// baking it in would make one environment's DNS a property of the domain layer.
// The caller composes {delivery_base}/v1/{tenant}/{type}/{id}?preview_token=…,
// which is the same URL shape the edge already serves published content at.
type PreviewLinkDTO struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	// Tenant, EntryID and Type are every path segment of that URL except the
	// host. They echo what the token is scoped to: the token is opaque to
	// everyone but the Domain API, so without these the caller that has to build
	// the URL would be reading its own request back out of its own variables and
	// hoping they match what was signed.
	//
	// Tenant is the one segment the caller may genuinely not have — an admin
	// client is authenticated AS a tenant rather than passing one in, so it
	// would otherwise have to dig its own slug out of a token claim or an
	// environment variable and hope that is the tenant this link was signed for.
	Tenant  string    `json:"tenant"`
	EntryID uuid.UUID `json:"entry_id"`
	Type    string    `json:"type"`
}

// CreatePreviewLink mints a credential that shows one entry's working copy
// through the delivery edge.
//
// The gate chain is deliberately GetEntry's, one for one: same read action, same
// type-read guard, same ownership confinement, and the entry must actually be
// loaded. Minting a link is handing someone else your read of that row, so it
// must not be reachable from anywhere your own read is not. In particular the
// row is fetched rather than trusted from the path — a link minted for an id
// that does not exist would be a valid credential for a row that might be
// created later, by someone else.
//
// Publication state is deliberately NOT checked: a draft is the whole point.
func (s *contentService) CreatePreviewLink(ctx context.Context, typeName string, id uuid.UUID) (PreviewLinkDTO, error) {
	sub, err := s.authorize(ctx, ActionContentRead, id.String(), typeName)
	if err != nil {
		return PreviewLinkDTO{}, err
	}
	// THE gate. Without it, one leaked preview link is unbounded access: a
	// preview subject passes content:read, passes guardTypeRead (delivery is
	// never role-refused), and passes guardOwned (confinedAuthor returns nil for
	// a delivery credential by design) — so it could mint a fresh token for ANY
	// entry id in the tenant, and mint another before each 30-minute expiry. A
	// credential that can re-issue itself has no TTL, and PreviewTokenTTL is the
	// entire security story for a token nothing can revoke.
	//
	// Written as PublicDelivery rather than PreviewEntryID != nil: the ordinary
	// delivery credential held by the edge must not mint either, and testing the
	// wider condition means a future credential that is delivery-shaped but not
	// preview-shaped is refused by default instead of by omission.
	if sub.PublicDelivery {
		return PreviewLinkDTO{}, apperrors.New(
			"CONTENT_PREVIEW_MINT_FORBIDDEN",
			"minting a preview link is an editorial operation; a delivery credential may not issue one",
			403,
		)
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return PreviewLinkDTO{}, err
	}
	if err := guardTypeRead(ct, sub); err != nil {
		return PreviewLinkDTO{}, err
	}
	e, err := s.repo.GetEntry(ctx, sub.TenantID, ct.ID, id)
	if err != nil {
		return PreviewLinkDTO{}, err
	}
	if err := guardOwned(ct, sub, e); err != nil {
		return PreviewLinkDTO{}, err
	}
	if s.preview == nil {
		return PreviewLinkDTO{}, ErrPreviewDisabled
	}
	// sub.UserID, not the entry's author: the claim records who HANDED OUT the
	// link, which is the fact an audit of a leak needs. e.ID rather than the path
	// id so the signed scope is the row that was actually loaded and guarded.
	token, exp, err := s.preview.IssuePreviewToken(sub.UserID, sub.TenantID, e.ID)
	if err != nil {
		return PreviewLinkDTO{}, err
	}
	return PreviewLinkDTO{
		Token: token, ExpiresAt: exp,
		// sub.TenantID, not a value from the request: it is the tenant the token
		// was actually signed for, which is the whole point of echoing it.
		Tenant: sub.TenantID, EntryID: e.ID, Type: ct.Name,
	}, nil
}
