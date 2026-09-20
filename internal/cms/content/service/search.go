package service

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// snippetHalfWidth is how many runes of context ride on each side of the first
// search term's match inside a result's snippet.
const snippetHalfWidth = 60

// snippetFallbackWidth is the snippet length when the term cannot be located
// in the text it was matched against (e.g. it matched a different term, or
// search_text lagged behind an edit that has since overwritten the match —
// see ADR-021's staleness note). The prefix is still informative, just not
// centred on anything.
const snippetFallbackWidth = 120

// SearchInput bounds one cross-type search (ADR-021 §3).
type SearchInput struct {
	// Query is required; ListEntries' 200-char cap and multi-term-AND semantics
	// apply here identically (see ListEntriesInput.Query).
	Query string
	// Locale narrows to one language; empty means every locale.
	Locale string
	// Status narrows to one editorial state; empty means all states.
	Status string
	Limit  int
}

// SearchResultDTO is one row of a cross-type search result.
//
// It carries no payload and no title beyond the snippet, for the same reason
// PendingEntryDTO does not carry one: this query spans every content type in
// the tenant, so there is no single type's read rule or field mask to check a
// full payload against. Snippet is safe to ship because it is built from
// search_text (ADR-021), which already excludes the fields ExtractSearchText
// was told to skip (enum/relation/file/number/date/boolean) — but it is still
// PROSE, not a value, and it is computed here rather than trusted verbatim so
// a future field-level read restriction has one function (SnippetFor) to
// change rather than every call site.
type SearchResultDTO struct {
	ID uuid.UUID `json:"id"`
	// ContentType is the type's slug (domain.ContentType.Name), matching every
	// other place a content type identifies itself on the wire.
	ContentType string    `json:"content_type"`
	Locale      string    `json:"locale"`
	Status      string    `json:"status"`
	UpdatedAt   time.Time `json:"updated_at"`
	Snippet     string    `json:"snippet"`
}

// Search is the cross-type search endpoint: every entry in the tenant, across
// every content type, whose search_text matches every term of in.Query,
// newest-edited first (ADR-021 §3).
//
// AUDIENCE: staff only, mirroring ListEntries' own admin path — reusing
// ActionContentList as the coarse gate (viewer included, same as any other
// list read) rather than minting a narrower verb the way ListPendingReview
// did, because the task this satisfies asked for "the same read-authorization
// as existing list endpoints", and content:list is what every per-type list
// already runs on. A delivery credential is refused outright: delivery already
// has its own per-type `q` (ListEntries, reading published_search_text under
// its own audience rules) and reads across every type in one call is not a
// shape delivery's per-type authorization was built to reason about. A preview
// credential is refused by the very same PublicDelivery check below, not a
// separate guardPreviewCollection call: audienceFor (dto.go) can only ever
// return audiencePreview when PublicDelivery is already true, so a dedicated
// preview guard here would be unreachable dead code — see ListPendingReview,
// which is refused the identical way for the identical reason.
//
// The per-row DATA gate is dataVisibleExpr (repository/postgres_repository.go)
// — the same EXISTS-against-content_types check ListPendingReview's SQL runs,
// reused here rather than duplicated so the two cross-type queries cannot
// silently diverge on what "visible" means.
//
// Always reads search_text (the WORKING copy's extracted text), never
// published_search_text: this is a staff surface, and a viewer/editor/admin
// looking for "the page about X" expects to find their own unpublished draft,
// not just what is live. Nothing here bypasses per-entry read restrictions
// beyond what search_text itself excludes (see SearchResultDTO's comment) —
// there is no payload in the response for a restricted field to leak through.
func (s *contentService) Search(ctx context.Context, in SearchInput) (_ []SearchResultDTO, err error) {
	sub, err := s.authorize(ctx, ActionContentList, "search", "")
	if err != nil {
		return nil, err
	}
	// Second layer, same shape as ListPendingReview's: refuses delivery AND
	// preview in one check, because a preview subject is a delivery subject
	// with PreviewEntryID set (see the comment above).
	if sub.PublicDelivery {
		return nil, apperrors.ErrForbidden
	}
	// Same cap as ListEntries' `q`, and the same reason: bound on the raw
	// string before it is split into terms, so the bound is on request size,
	// not on a term count checked separately.
	const maxSearchQueryLen = 200
	if len(in.Query) > maxSearchQueryLen {
		return nil, apperrors.New("CONTENT_SEARCH_QUERY_TOO_LONG", "q must be at most 200 characters", 422).
			WithDetails(map[string]any{"limit": maxSearchQueryLen, "length": len(in.Query)})
	}
	if strings.TrimSpace(in.Query) == "" {
		return nil, apperrors.New("CONTENT_SEARCH_QUERY_REQUIRED", "q is required", 400)
	}
	if in.Status != "" && !domain.ValidStatus(in.Status) {
		return nil, apperrors.New("CONTENT_STATUS_INVALID", "status must be draft or published", 400).
			WithDetails(map[string]any{"status": in.Status, "allowed": domain.AllowedStatuses()})
	}
	if in.Locale != "" && !domain.ValidLocale(in.Locale) {
		return nil, apperrors.New("CONTENT_LOCALE_INVALID", "locale must be a valid language tag", 400).
			WithDetails(map[string]any{"locale": in.Locale})
	}
	rows, err := s.repo.SearchEntries(ctx, repository.SearchEntriesFilter{
		TenantID:     sub.TenantID,
		ViewerRole:   sub.TenantRole,
		ViewerUserID: sub.ResponsibleUserID(),
		Query:        in.Query,
		Locale:       in.Locale,
		Status:       in.Status,
		Limit:        in.Limit,
	})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return []SearchResultDTO{}, nil
	}
	// One read of the type list, not one per row — same reasoning as
	// ListPendingReview: ListContentTypes already loads every type in one
	// grouped query, and asking per row reintroduces the N+1 this endpoint
	// exists to avoid.
	types, err := s.repo.ListContentTypes(ctx, sub.TenantID)
	if err != nil {
		return nil, err
	}
	slugByID := make(map[uuid.UUID]string, len(types))
	for _, ct := range types {
		slugByID[ct.ID] = ct.Name
	}
	firstTerm := strings.Fields(in.Query)[0]
	out := make([]SearchResultDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, SearchResultDTO{
			ID: row.ID,
			// A type the list did not return (a race with a concurrent schema
			// delete) leaves ContentType empty rather than dropping the row —
			// same choice ListPendingReview makes and the same reason: a row
			// that is visible and unlabelled is diagnosable, a silently
			// vanished one is not.
			ContentType: slugByID[row.ContentTypeID],
			Locale:      row.Locale,
			Status:      row.Status,
			UpdatedAt:   row.UpdatedAt,
			Snippet:     SnippetFor(row.SearchText, firstTerm),
		})
	}
	return out, nil
}

// SnippetFor extracts a rune-safe window of text around the first
// case-insensitive occurrence of term inside text — snippetHalfWidth runes on
// each side — or, when term cannot be found (a stale search_text row, or a
// later AND term is what actually matched), the first snippetFallbackWidth
// runes of text.
//
// Byte-index math from strings.Index would slice a multi-byte UTF-8 rune in
// half on a CJK-heavy row; converting to a rune slice first is what keeps this
// safe for exactly the text ADR-021 exists to make searchable.
func SnippetFor(text, term string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	term = strings.TrimSpace(term)
	if term == "" {
		return firstRunes(text, snippetFallbackWidth)
	}
	byteIdx := strings.Index(strings.ToLower(text), strings.ToLower(term))
	if byteIdx < 0 {
		return firstRunes(text, snippetFallbackWidth)
	}
	runes := []rune(text)
	matchStart := utf8.RuneCountInString(text[:byteIdx])
	matchLen := utf8.RuneCountInString(term)
	start := matchStart - snippetHalfWidth
	if start < 0 {
		start = 0
	}
	end := matchStart + matchLen + snippetHalfWidth
	if end > len(runes) {
		end = len(runes)
	}
	return string(runes[start:end])
}

func firstRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
