package service

import (
	"context"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Schema export (ADR-008). The artifact is the same information GET /types
// already returns, in an envelope that can leave this database: no tenant, no
// ids, no timestamps.

// ExportSchema returns the calling tenant's whole schema as an artifact.
//
// Authorisation reuses the list action rather than inventing a verb. The
// response contains strictly what ListContentTypes contains, so a separate
// permission would be theatre — it would suggest a boundary that the existing
// endpoint already lets a caller walk around one type at a time.
//
// A delivery credential is refused outright, and NOT because the bytes are
// secret. It is the same rule the entry projection enforces (OD2-023 F2): a
// public reader has no business enumerating the shape of a collection, and an
// export is the most convenient possible spelling of that enumeration. The
// audience question is answered here explicitly rather than inherited, which is
// the discipline ProjectEntry exists to impose.
func (s *contentService) ExportSchema(ctx context.Context) (domain.Artifact, error) {
	sub, err := s.authorize(ctx, ActionContentList, "collection", "")
	if err != nil {
		return domain.Artifact{}, err
	}
	if sub.PublicDelivery {
		return domain.Artifact{}, apperrors.New("CONTENT_SCHEMA_EXPORT_FORBIDDEN", "schema export is an admin operation", 403)
	}
	cts, err := s.repo.ListContentTypes(ctx, sub.TenantID)
	if err != nil {
		return domain.Artifact{}, err
	}
	return domain.NewArtifact(cts), nil
}
