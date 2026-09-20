package service

import (
	"context"

	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// ListLocalesInput bounds GET /api/v1/content/locales. Type, when non-empty,
// narrows the count to one content type; empty means every type in the
// tenant, matching ListEntriesInput.Locale's own "" = all convention.
type ListLocalesInput struct {
	Type string
}

// LocaleDTO is one row of the locales inventory: a locale in use in the
// tenant (optionally narrowed to one type) and how many entries carry it.
type LocaleDTO struct {
	Locale  string `json:"locale"`
	Entries int    `json:"entries"`
}

// ListLocales answers "which locales does this tenant actually use, and how
// much content is in each" — the one thing missing that stops a console from
// drawing a locale switcher: nothing else on the wire enumerates a tenant's
// locales, only a single entry's own Locale field (EntryDTO) or a single
// type's translation group (ListTranslations).
//
// AUDIENCE and refusal shape mirror Search exactly, for the same reason: this
// is a cross-cutting inventory read with no single content type's read rule to
// authorize against (or, with `type` set, one resolved AFTER the coarse gate,
// same order GetContentType and every per-type endpoint already use). A
// delivery or preview credential is refused outright — the public edge has no
// use for a language inventory of draft AND published content, and this
// endpoint does not distinguish the two the way ListEntries' Status does.
//
// The per-row DATA gate is dataVisibleExpr (repository/postgres_repository.go),
// the same one Search and ListPendingReview share, so a confined viewer's
// count only reflects entries they may see — never an undercount they cannot
// explain, never a count that leaks the existence of rows they are refused.
func (s *contentService) ListLocales(ctx context.Context, in ListLocalesInput) (_ []LocaleDTO, err error) {
	sub, err := s.authorize(ctx, ActionContentList, "locales", "")
	if err != nil {
		return nil, err
	}
	// Same second layer as Search: refuses delivery AND preview in one check,
	// because a preview subject is a delivery subject with PreviewEntryID set.
	if sub.PublicDelivery {
		return nil, apperrors.ErrForbidden
	}
	f := repository.ListLocalesFilter{
		TenantID:     sub.TenantID,
		ViewerRole:   sub.TenantRole,
		ViewerUserID: sub.ResponsibleUserID(),
	}
	if in.Type != "" {
		ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, in.Type)
		if err != nil {
			return nil, err
		}
		f.ContentTypeID = &ct.ID
	}
	rows, err := s.repo.ListLocales(ctx, f)
	if err != nil {
		return nil, err
	}
	out := make([]LocaleDTO, len(rows))
	for i, row := range rows {
		out[i] = LocaleDTO{Locale: row.Locale, Entries: row.Entries}
	}
	return out, nil
}
