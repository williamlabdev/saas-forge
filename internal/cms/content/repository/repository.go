package repository

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// ErrVersionConflict is returned by UpdateEntry when the entry's stored version
// no longer matches the version the caller read — an optimistic-lock conflict.
// It renders as HTTP 409 so the client re-reads and retries.
var ErrVersionConflict = apperrors.New(
	"CONTENT_VERSION_CONFLICT",
	"entry was modified by another writer; re-read and retry",
	http.StatusConflict,
)

// ErrTranslationExists is returned when a translation group already has a row
// in the requested locale — the (tenant, translation_group_id, locale) unique
// index is what makes "the English version" unambiguous.
var ErrTranslationExists = apperrors.New(
	"CONTENT_TRANSLATION_EXISTS",
	"this entry already has a translation in that locale",
	http.StatusConflict,
)

// Op is a whitelisted filter operator. The service maps the public filter
// grammar (e.g. "state:eq:paid") to one of these; anything else is rejected
// before it reaches the SQL builder.
type Op string

const (
	OpEq       Op = "eq"
	OpNeq      Op = "neq"
	OpGt       Op = "gt"
	OpGte      Op = "gte"
	OpLt       Op = "lt"
	OpLte      Op = "lte"
	OpIn       Op = "in"
	OpContains Op = "contains"
	// OpHas / OpNhas are set membership on a MULTI-VALUED field, and exist
	// instead of overloading OpEq because the two are different predicates: eq
	// asks "is the value x", has asks "does the set contain x". A grammar whose
	// operator meaning depends on schema state the caller may not have loaded is
	// one you cannot read off the query string.
	//
	// Overloading would also have been silently wrong rather than merely
	// confusing: containmentDoc builds {key: scalar}, and Postgres evaluates
	// '{"tags":["ai"]}' @> '{"tags":"ai"}' as FALSE — the top-level
	// array-contains-scalar exception does not apply at a nested key. A filter
	// that returns zero rows is worse than one that 400s.
	OpHas  Op = "has"
	OpNhas Op = "nhas"
)

// ValidOp reports whether op is a supported filter operator. Whether it is legal
// for a GIVEN field additionally depends on that field's cardinality; the
// service checks that before any SQL is built, because the scalar comparison
// operators raise a cast error (a 500) rather than a validation failure when
// they meet an array.
func ValidOp(op Op) bool {
	switch op {
	case OpEq, OpNeq, OpGt, OpGte, OpLt, OpLte, OpIn, OpContains, OpHas, OpNhas:
		return true
	default:
		return false
	}
}

// OpsForCardinality lists the operators legal on a field, by cardinality. Both
// directions are enforced: a scalar field refuses has/nhas too, so a caller
// cannot stumble into containment semantics by accident.
func OpsForCardinality(multiple bool) []Op {
	if multiple {
		return []Op{OpHas, OpNhas}
	}
	return []Op{OpEq, OpNeq, OpGt, OpGte, OpLt, OpLte, OpIn, OpContains}
}

// DuplicateValue is one (locale, value) pair that more than one entry holds.
type DuplicateValue struct {
	Locale  string `json:"locale"`
	Value   string `json:"value"`
	Entries int    `json:"entries"`
}

// RelationRef names one field that points at a content type by name. It exists
// because relation_entity stores the type NAME, so renaming or deleting a type
// is a cross-type operation rather than a local one.
type RelationRef struct {
	TypeName string
	// Component is set instead of TypeName when the referring field is a
	// component's sub-field (ADR-020): the relation reaches the type through
	// every content type that uses the component.
	Component string
	FieldKey  string
}

// ComponentRef is one content-type field that takes its shape from a
// component (ADR-020): the `used_by` a GET /components/{name} reports, and the
// fan-out every sub-field schema change has to rewrite.
type ComponentRef struct {
	TypeID   uuid.UUID
	TypeName string
	FieldKey string
	Multiple bool
	// Zone marks a referrer that reaches the component through a DYNAMIC ZONE
	// allowed-list rather than a `component` field (ADR-020 Amendment 1). One
	// ref type serves both because every caller wants the same fan-out — which
	// types, which field, rewrite the items — and the only difference is the
	// SQL helper that finds the items: a zone's are tagged and heterogeneous,
	// so they are selected by __component, and Multiple is meaningless (a zone
	// is always a list).
	//
	// A parallel ZoneRef type would have duplicated ListComponentReferrers,
	// used_by, the media relink loop and every cross-type gate, and the first
	// one someone forgot to update would be a silent hole rather than a
	// compile error.
	Zone bool
}

// OpAllowedFor reports whether op may be applied to a field of this cardinality.
func OpAllowedFor(op Op, multiple bool) bool {
	for _, x := range OpsForCardinality(multiple) {
		if x == op {
			return true
		}
	}
	return false
}

// FieldFilter is a resolved, type-aware filter clause. Field is the matching
// content_type_fields definition (so the SQL builder knows how to type the
// value); Value is the raw operand from the request. The repository binds both
// the key and the value as query parameters — it never concatenates them into
// the SQL or JSONB path.
type FieldFilter struct {
	Field domain.Field
	Op    Op
	Value string
}

// SortSpec is a resolved sort directive over a single defined field.
type SortSpec struct {
	Field domain.Field
	Desc  bool
}

// ListEntriesFilter is the fully-validated input to ListEntries. TenantID and
// ContentTypeID are always set from the authenticated subject + resolved type;
// the repository unconditionally constrains every query to them.
type ListEntriesFilter struct {
	TenantID      string
	ContentTypeID uuid.UUID
	Filters       []FieldFilter
	Sort          *SortSpec
	// Status narrows to one editorial state (domain.StatusDraft |
	// domain.StatusPublished). Empty means "all states" — that is the admin
	// default. A public delivery path must always set StatusPublished; it is a
	// column predicate, never a caller-supplied payload filter.
	Status string
	// Locale narrows to one language. Empty = every locale (the admin default).
	Locale string
	// TranslationGroupID narrows to one entry and its translations.
	TranslationGroupID uuid.UUID
	// CreatedBy confines the result to one author's entries — the data-level
	// permission of migration 000027, resolved by the service into an id.
	//
	// It is a COLUMN predicate for the same reason Status and Locale are: it must
	// be inside the query that produces the page AND the COUNT(*) beside it.
	// Filtering the page in Go afterwards would leave `total` counting rows the
	// caller may not see — so the hidden count is recoverable by subtraction —
	// and would hand back short pages that look like the end of the collection.
	//
	// nil means unconfined. It is never inferred here: a repository that decided
	// confinement for itself would be a second copy of the rule, and the copy
	// that runs is whichever one the call site remembered.
	CreatedBy *uuid.UUID
	Limit     int
	Offset    int
	// CursorPaged switches this query to keyset pagination: Offset is ignored,
	// no COUNT(*) is issued (total is meaningless and unaffordable at scale),
	// and the order is forced to the cursor's key. Set for the delivery
	// audience; the admin audience keeps offset paging and its total.
	CursorPaged bool
	// After is the exclusive lower bound in cursor order — rows STRICTLY past
	// this key. nil = first page. Only meaningful with CursorPaged.
	After *EntryCursor
	// MatchPublished evaluates every caller-supplied predicate — Filters and
	// Sort alike, the WHERE and the ORDER BY — against published_payload
	// instead of payload. The delivery audience sets it (ADR-006 Amendment 4):
	// the snapshot is the only copy that audience may observe, and a WHERE on
	// the working copy is an oracle on unpublished edits even when every row
	// handed back is a snapshot — hit-or-miss is the leak, not the data.
	//
	// It is a separate flag and not inferred from Status = published, because
	// the ADMIN audience asking for status=published means "rows that are
	// published", matched on the working copy like every other admin list.
	// The repository refuses the flag without Status = published: a draft's
	// published_payload is NULL, and after an unpublish it is a retained
	// snapshot (migration 000033) nobody may serve — matching either would
	// answer for a copy the caller is not allowed to see.
	MatchPublished bool
	// Query is the validated, already-trimmed `q` free-text search term (ADR-021).
	// Empty means "no search narrowing" — the pre-existing behaviour. Non-empty
	// is split on whitespace into terms, each becoming an ILIKE '%term%' against
	// search_text (or published_search_text when MatchPublished), ANDed with
	// each other AND with Filters — the two are composable, not alternatives.
	Query string
}

// PendingReviewFilter scopes the release queue (ADR-014 §2). It carries the
// tenant and a bound and nothing else on purpose: the queue's whole definition
// is "not what the public can see", and every narrowing a caller could apply —
// by type, by author, by status — is a way to hide something from the person
// whose job is to notice it.
type PendingReviewFilter struct {
	TenantID string
	// ViewerRole and ViewerUserID carry the DATA-level permission question into
	// the query (ADR-009's second layer). They are not optional and there is no
	// "unrestricted" zero value on purpose: an empty role matches no read_roles
	// list, so a caller the service forgot to describe sees restricted types not
	// at all rather than all of them.
	//
	// They live on the FILTER rather than being applied afterwards in Go because
	// type_permission.go's rule for lists is that confinement must be a WHERE
	// clause — a page filtered after the database produced it has holes in it and
	// a total the caller may not see. This queue has no total, but the holes are
	// reason enough, and the next reader who adds a COUNT would inherit the bug.
	ViewerRole   string
	ViewerUserID uuid.UUID
	Limit        int
}

// pendingReviewLimitDefault / pendingReviewLimitMax bound one read, matching the
// activity stream's shape beneath it on the same page. The queue is the top half
// of a landing page, not an export.
const (
	pendingReviewLimitDefault = 50
	pendingReviewLimitMax     = 200
)

// SearchEntriesFilter scopes GET /api/v1/content/search (ADR-021 §3) — the
// second cross-type entry query this repository serves, alongside
// PendingReviewFilter, and shaped the same way for the same reason: a query
// with no single content type to check permission against carries the
// data-level visibility pair (ViewerRole/ViewerUserID) itself rather than
// leaning on a per-type gate the service would apply afterwards, because a
// page filtered after the database produced it has holes in it (see
// PendingReviewFilter's own comment).
//
// It is STAFF-ONLY, like the pending-review queue: the service refuses
// sub.PublicDelivery before this is ever built. Delivery already has its own
// per-type search (ListEntriesFilter.Query against a single type, matched on
// published_search_text) — this endpoint exists to search ACROSS types for
// someone editing the tenant, not to give the public a second way to browse
// draft content.
type SearchEntriesFilter struct {
	TenantID     string
	ViewerRole   string
	ViewerUserID uuid.UUID
	// Query is the validated `q` (trimmed, non-empty, <=200 chars — the service
	// enforces both before this is built). Split into whitespace-separated
	// terms and ANDed as search_text ILIKE '%term%', exactly as
	// ListEntriesFilter.Query is against whichever entry column MatchPublished
	// would have picked — this endpoint always reads the WORKING copy
	// (search_text), never published_search_text; see the type comment.
	Query string
	// Locale and Status are optional column predicates; empty means
	// unconfined, matching ListEntriesFilter's convention for the same fields.
	Locale string
	Status string
	Limit  int
}

// searchEntriesLimitDefault / searchEntriesLimitMax bound one read of the
// cross-type search endpoint — smaller than the pending-review queue's,
// because this is a person typing into a search box and rereading the top of
// a results list, not a landing-page widget.
const (
	searchEntriesLimitDefault = 20
	searchEntriesLimitMax     = 50
)

// SearchEntryRow is one row of the cross-type search endpoint's result.
//
// It carries SearchText — the FULL column, not a pre-cut snippet — because
// the snippet is centred on the first matched TERM's position, which is a
// rune-indexed operation the service is better placed to do in Go than a SQL
// expression tangled around jsonb_path_query and multi-term OR position() is.
// ContentTypeID, not a name: resolving names is one extra ListContentTypes
// call for the whole result set, the same N+1-avoiding shape ListPendingReview
// already uses one field below.
type SearchEntryRow struct {
	ID            uuid.UUID
	ContentTypeID uuid.UUID
	Locale        string
	Status        string
	UpdatedAt     time.Time
	SearchText    string
}

// ListLocalesFilter scopes GET /api/v1/content/locales. Shaped like
// SearchEntriesFilter for the same reason: a query with no single content type
// to check permission against (or, here, an OPTIONAL one) carries the
// data-level visibility pair itself.
type ListLocalesFilter struct {
	TenantID     string
	ViewerRole   string
	ViewerUserID uuid.UUID
	// ContentTypeID narrows the count to one type; nil means every type in the
	// tenant. Resolved from the `type` slug by the service, same as every other
	// endpoint that accepts one, so the repository never parses a name.
	ContentTypeID *uuid.UUID
}

// LocaleCountRow is one row of the locales endpoint's result: a locale in use
// in the tenant and how many (visible) entries carry it.
type LocaleCountRow struct {
	Locale  string
	Entries int
}

// EntryCursor is the keyset a delivery caller pages by. It is never a single
// column: created_at is not unique — a seeding run writes many rows inside the
// same microsecond — and neither is a payload sort key, and a non-unique key
// silently skips or repeats rows across page boundaries. ID breaks every tie,
// so the order is total.
//
// WHICH key the cursor names is decided by the SAME ListEntriesFilter that
// carries it: with Sort set, the window is (SortValue, ID); without it,
// (CreatedAt, ID). The cursor does not repeat the sort key, because a cursor
// that could disagree with the request's ORDER BY is a cursor that skips rows —
// the service refuses a mismatched pair before it ever reaches here (ADR-006
// Amendment 5), so the repository has one order to mirror, not two.
//
// Callers never construct one: it round-trips through the opaque token the API
// hands out (service.encodeCursor).
type EntryCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
	// SortValue is the last row's value for the sort key, as `->>` renders it —
	// the text form, cast by orderedValue exactly as the ORDER BY expression is.
	// nil means the row had NO value there (key absent, or JSON null), which is
	// a position in the order, not the absence of one: null rows sort LAST in
	// both directions, so a nil SortValue means "resume inside the null block".
	//
	// Read only when the filter carries a Sort.
	SortValue *string
}

// MediaAssetPatch is a PER-FIELD edit of an asset's client-declared metadata.
//
// Each group carries its own Set flag rather than relying on a nil pointer to
// mean "unchanged", because nil is a legal target value here: clearing alt_text
// back to "not recorded" is a real operation. Without the flags, a caller fixing
// one field would silently blank every field it did not send — and a repository
// that overwrites what it was never told about is indistinguishable from a lost
// update.
//
// Dimensions share ONE flag deliberately. The DB requires both or neither
// (media_assets_dimensions_check), so a struct that let a caller express "set
// width, leave height" would only be able to produce a constraint violation.
// Making the invalid state unrepresentable here means the 422 comes from the
// service naming the field, never from the driver.
type MediaAssetPatch struct {
	SetFilename bool
	Filename    *string

	SetAltText bool
	AltText    *string

	SetDimensions bool
	WidthPx       *int
	HeightPx      *int
}

// IsEmpty reports that the patch names no field at all.
func (p MediaAssetPatch) IsEmpty() bool {
	return !p.SetFilename && !p.SetAltText && !p.SetDimensions
}

// MediaListFilter is the fully-validated input to ListMediaAssets. TenantID
// travels as its own parameter rather than a field here, matching
// GetMediaAsset and every other media method — it is never optional the way
// the two filters below are.
type MediaListFilter struct {
	// Query, when non-empty, narrows to filenames containing it
	// (case-insensitive). Empty means unfiltered.
	Query string
	// ContentTypePrefix, when non-empty, narrows to content_type LIKE
	// '<prefix>%' — "image/", "video/" or "application/", chosen by the
	// service from the caller's `kind`. Empty means every kind.
	ContentTypePrefix string
	Limit             int
	Offset            int
	// Orphan, when true, narrows to assets referenced by NEITHER entry_media
	// nor entry_media_published — ADR-005's unresolved paragraph ("orphan
	// assets are not automatically cleaned up") given a way to find them, not
	// a way to delete them: see ADR-024 for why a batch-delete endpoint is
	// deliberately out of scope.
	Orphan bool
}

// EntryRelationRef is one (field, target) pair collected while validating a
// relation field (checkRelationsIn) — the write-time twin of RelationRef,
// which describes a SCHEMA relationship (a field that CAN point at a type)
// rather than one entry's actual, validated value.
type EntryRelationRef struct {
	FieldPath string
	TargetID  uuid.UUID
}

// ReferencedByRow is one referring entry in a reverse-reference lookup
// (ADR-024): "what points at this entry / this media asset". Draft and
// Published are independent because the SAME entry can hold the reference in
// its working copy, its last published snapshot, or both at once — the two
// link tables (draft / published) are never merged into one row anywhere else
// in the schema, so EntryReferencedBy / MediaReferencedBy do that merge
// before returning.
type ReferencedByRow struct {
	EntryID       uuid.UUID
	ContentTypeID uuid.UUID
	TypeName      string
	Title         string
	// FieldPath names the field that holds the reference, dot-joined the same
	// way entry_relations.field_path is (see 000050). Always "" for
	// MediaReferencedBy: entry_media predates per-field tracking (000019) and
	// widening it is out of scope here — see ADR-024's known limitations.
	FieldPath string
	Draft     bool
	Published bool
}

// ReferencedByResult is one page of a reverse-reference lookup: Items is the
// requested page, Total counts every DISTINCT (entry, field) pair regardless
// of page — the same total/page split ListEntries and ListMediaAssets use.
type ReferencedByResult struct {
	Items []ReferencedByRow
	Total int
}

// ContentRepository is the persistence port for the content domain. A single
// repository serves every content type — that genericity is the whole point.
type ContentRepository interface {
	// WithTx runs fn against a repository whose every operation joins ONE
	// transaction, so a caller applying several schema verbs gets all of them
	// or none. Nesting is a no-op for the same tenant and an error for a
	// different one: the transaction's app.tenant_id is set when it opens, so a
	// second tenant inside would silently run under the first one's RLS scope.
	//
	// It is on the interface rather than the concrete type because the service
	// is what needs to open the boundary, and a caller that can only reach it by
	// type-asserting would quietly lose atomicity against any other
	// implementation — the failure being a half-applied schema, which is the
	// state ADR-007 exists to prevent.
	WithTx(ctx context.Context, tenantID string, fn func(ContentRepository) error) error

	// Webhooks (ADR-011): the tenant's registered receivers of content events.
	// Registry CRUD only — delivery-time reads go through the concrete type's
	// ActiveWebhookEndpoints, which the WORKER holds as outbox.WebhookDirectory;
	// the service has no business enumerating secrets it will not sign with.
	CreateWebhook(ctx context.Context, w *domain.Webhook) error
	ListWebhooks(ctx context.Context, tenantID string) ([]*domain.Webhook, error)
	DeleteWebhook(ctx context.Context, tenantID string, id uuid.UUID) error

	// Schema (content types + fields).
	CreateContentType(ctx context.Context, ct *domain.ContentType) error
	AddField(ctx context.Context, tenantID string, f *domain.Field) error
	GetContentTypeByName(ctx context.Context, tenantID, name string) (*domain.ContentType, error)
	// GetContentTypeByID serves the one caller that starts from an id rather
	// than a URL segment: the scheduler worker (ADR-017), which holds a schedule
	// row and no request.
	GetContentTypeByID(ctx context.Context, tenantID string, id uuid.UUID) (*domain.ContentType, error)
	ListContentTypes(ctx context.Context, tenantID string) ([]*domain.ContentType, error)

	// --- schema mutation ------------------------------------------------------
	//
	// Every write below is ONE transaction, and that is the whole design: a
	// definition change and the stored-data migration it implies must land
	// together. validatePayload checks the WHOLE document, so an entry left
	// describing a schema that no longer exists is not merely stale — it becomes
	// un-PATCHable, with an error naming a field the caller never touched, while
	// SetEntryStatus (which does not re-validate) will happily keep publishing it.

	// UpdateFieldDefinition writes the mutable properties (label, required,
	// enum_values, read_roles, write_roles). Type, multiple and relation_entity
	// are refused by the service and never reach here.
	//
	// It writes the permission lists UNCONDITIONALLY, from the field the service
	// hands it. That makes an omitted list in a PATCH a no-op only because the
	// service copies the stored value forward first; a partial UPDATE here that
	// touched read_roles only when it "looked set" would be the same nil-versus-
	// empty ambiguity that makes empty mean unrestricted, resolved silently in
	// the direction of opening access.
	UpdateFieldDefinition(ctx context.Context, tenantID string, ct *domain.ContentType, f domain.Field, now time.Time) error
	// UpdateContentTypeDefinition writes a type's mutable properties: label and
	// the three data-level permission lists (migration 000027). `name` is absent
	// because renaming is its own verb.
	//
	// It writes the permission lists UNCONDITIONALLY, from the type the service
	// hands it — the same contract UpdateFieldDefinition carries, and it matters
	// here for the same reason. An UPDATE that touched read_roles only when it
	// "looked set" would resolve the nil-versus-empty ambiguity silently, in the
	// direction of opening a collection; the service copies the stored lists
	// forward so an omitted list in a PATCH is a no-op it decided on purpose.
	//
	// It replaces UpdateContentTypeLabel rather than sitting beside it. Two
	// methods, one writing a subset of the other's columns, is an invitation to
	// call the narrow one and wonder why the permissions reverted.
	UpdateContentTypeDefinition(ctx context.Context, tenantID string, ct *domain.ContentType, now time.Time) error
	// DeleteField removes the definition AND strips the key from both payload
	// copies of every entry of the type, rebuilding the media links when the
	// field was a file.
	//
	// It takes an actor because it CHANGES CONTENT, in bulk, and every other
	// path that changes content records who did it (ADR-014 §4/§5). Without it
	// the revision rows these writes produce would have to copy each entry's
	// PREVIOUS editor, which attributes a schema admin's bulk deletion to
	// whoever last touched each entry — the specific false answer 000031
	// refused to write.
	DeleteField(ctx context.Context, tenantID string, ct *domain.ContentType, f domain.Field, actor domain.WriteActor, now time.Time) error
	// RenameField moves the key in both payload copies. It is not sugar for
	// delete+add: that pair destroys every stored value, and callers reach for it
	// anyway, so the lossless path has to exist.
	//
	// Takes an actor for DeleteField's reason.
	RenameField(ctx context.Context, tenantID string, ct *domain.ContentType, oldKey, newKey string, actor domain.WriteActor, now time.Time) error
	// RenameContentType also rewrites relation_entity on every field pointing at
	// the old name, WITHIN THIS TENANT — see the implementation for why the
	// tenant join is load-bearing rather than stylistic.
	RenameContentType(ctx context.Context, tenantID string, id uuid.UUID, oldName, newName string, now time.Time) error
	DeleteContentType(ctx context.Context, tenantID string, id uuid.UUID) error

	// Guards for the above. Each answers a question the service must ask BEFORE
	// mutating, because the alternative to refusing is leaving data that no
	// longer validates.
	CountEntriesForType(ctx context.Context, tenantID string, contentTypeID uuid.UUID) (int, error)
	CountEntriesWithField(ctx context.Context, tenantID string, contentTypeID uuid.UUID, key string) (int, error)
	// CountEntriesMissingField treats an explicit JSON null as missing, matching
	// validatePayload. A bare key-existence test would disagree with the
	// validator on exactly the rows that make tightening `required` unsafe.
	CountEntriesMissingField(ctx context.Context, tenantID string, contentTypeID uuid.UUID, key string) (int, error)
	// CountEntriesWithValuesOutside counts entries holding a value for key that
	// is not in allowed — the guard for narrowing an enum. Handles multi-valued
	// fields by checking every element.
	CountEntriesWithValuesOutside(ctx context.Context, tenantID string, contentTypeID uuid.UUID, f domain.Field, allowed []string) (int, error)
	// CountEntriesWithoutAuthor counts entries of a type with no recorded
	// created_by — rows predating migration 000021, and any written by a
	// non-human. It guards TIGHTENING own_only_roles: those rows match no author,
	// so a confined role would simply stop seeing them, and an entry that
	// disappears without a refusal is indistinguishable from data loss.
	CountEntriesWithoutAuthor(ctx context.Context, tenantID string, contentTypeID uuid.UUID) (int, error)
	// ListDuplicateFieldValues reports the (locale, value) pairs of f held by
	// more than one entry of the type, counting every entry's working copy and
	// its live snapshot — the question `unique` asks before it is switched on,
	// and the one UpdateFieldDefinition asks the database again, under a lock,
	// when it is. Capped at a page: the caller reports, it does not iterate.
	ListDuplicateFieldValues(ctx context.Context, tenantID string, contentTypeID uuid.UUID, f domain.Field) ([]DuplicateValue, error)
	// ScanFieldValues calls fn once per entry of the type with the value of key
	// in the working copy and in the LIVE snapshot (nil where absent, null, or
	// not published), decoded the way validatePayload sees them. It is how a
	// format/pattern/range tightening is guarded: the caller applies the SAME
	// rule the validator applies, so the guard cannot disagree with the write it
	// predicts — which a second, SQL-side spelling of a regular expression would.
	ScanFieldValues(ctx context.Context, tenantID string, contentTypeID uuid.UUID, key string, fn func(entryID uuid.UUID, working, live any) error) error
	// ListRelationReferrers finds fields in OTHER types of the same tenant whose
	// relation_entity names typeName.
	ListRelationReferrers(ctx context.Context, tenantID, typeName string) ([]RelationRef, error)

	// Reusable components (ADR-020). A component's sub-fields live in
	// content_type_fields with component_id set; the verbs below are the
	// component-side twins of the type-side schema verbs above, with one
	// difference that shapes every signature: a sub-field change fans out to
	// EVERY entry of EVERY type that references the component, which is why
	// RenameComponentField / DeleteComponentField take the referrer list and
	// rewrite the inline values set-wise, in one statement per referring field.
	CreateComponent(ctx context.Context, c *domain.Component) error
	GetComponentByName(ctx context.Context, tenantID, name string) (*domain.Component, error)
	GetComponentByID(ctx context.Context, tenantID string, id uuid.UUID) (*domain.Component, error)
	ListComponents(ctx context.Context, tenantID string) ([]*domain.Component, error)
	CountComponents(ctx context.Context, tenantID string) (int, error)
	// UpdateComponentDefinition persists the mutable scalars (label).
	UpdateComponentDefinition(ctx context.Context, tenantID string, c *domain.Component, now time.Time) error
	// DeleteComponent removes a component and its sub-fields. The service
	// refuses it while referrers exist; the FK RESTRICT is the layer below.
	DeleteComponent(ctx context.Context, tenantID string, id uuid.UUID) error
	// ListComponentReferrers lists the content-type fields of the tenant that
	// reference the component, ordered by type name then key.
	ListComponentReferrers(ctx context.Context, tenantID string, componentID uuid.UUID) ([]ComponentRef, error)
	AddComponentField(ctx context.Context, tenantID string, componentID uuid.UUID, f *domain.Field) error
	UpdateComponentFieldDefinition(ctx context.Context, tenantID string, c *domain.Component, f domain.Field, now time.Time) error
	// RenameComponentField renames the sub-field and rewrites the key inside
	// every item of every entry of every referrer, in both payload copies,
	// with the same version / provenance / revision rules as RenameField.
	RenameComponentField(ctx context.Context, tenantID string, c *domain.Component, refs []ComponentRef, oldKey, newKey string, actor domain.WriteActor, now time.Time) error
	// DeleteComponentField drops the sub-field and prunes it from every item
	// likewise. Media links are NOT relinked (the same standing as richtext
	// images today): the next entry write rebuilds them.
	DeleteComponentField(ctx context.Context, tenantID string, c *domain.Component, refs []ComponentRef, key string, actor domain.WriteActor, now time.Time) error
	// CountEntriesWithComponentSubKey counts the entries of one referring
	// type in which ANY item under fieldKey carries subKey, in either copy —
	// the data guard for a sub-field delete.
	CountEntriesWithComponentSubKey(ctx context.Context, tenantID string, ref ComponentRef, componentName, subKey string) (int, error)

	// CountEntriesWithZoneComponent counts the entries of one referring type
	// that hold at least one item of componentName in ref.FieldKey, in either
	// payload copy (ADR-020 Amendment 1). It is what refuses REMOVING a name
	// from a zone's allowed-list while content still depends on it.
	CountEntriesWithZoneComponent(ctx context.Context, tenantID string, ref ComponentRef, componentName string) (int, error)

	// RenameComponent renames a component and, in the same transaction,
	// rewrites every place the OLD NAME is stored: the __component tag inside
	// every zone item of every referring entry (both payload copies), and the
	// allowed-list of every dynamic zone that accepts it. `component` fields
	// need no rewrite — they store the id.
	RenameComponent(ctx context.Context, tenantID string, c *domain.Component, refs []ComponentRef, newName string, actor domain.WriteActor, now time.Time) error

	// CountContentTypes / CountEntriesForTenant back the per-tenant quota
	// backstop (TKT-R4a). CountEntriesForTenant spans all of a tenant's types.
	CountContentTypes(ctx context.Context, tenantID string) (int, error)
	CountEntriesForTenant(ctx context.Context, tenantID string) (int, error)

	// Entries (runtime documents).
	CreateEntry(ctx context.Context, e *domain.Entry) error
	GetEntry(ctx context.Context, tenantID string, contentTypeID, id uuid.UUID) (*domain.Entry, error)
	// UpdateEntry saves the WORKING copy. It deliberately never touches the
	// published snapshot — that is what keeps an edit to live content invisible
	// until someone publishes it (ADR-006).
	UpdateEntry(ctx context.Context, e *domain.Entry) error
	// SetEntryPublishState flips editorial state and moves the published
	// snapshot (payload → published_payload, and the asset links with it) in one
	// transaction. publishedAt is stored as given, so a re-publish can preserve
	// the original first-release timestamp.
	//
	// A publish ALSO writes one entry_publish_revisions row in that same
	// transaction and trims the entry to domain.MaxPublishRevisionsPerEntry
	// (ADR-018). It lives here rather than in the service because the scheduler
	// worker reaches this method WITHOUT going through the service, so a
	// service-side write would record hand-pressed releases and silently skip
	// scheduled ones. An unpublish writes no revision: it moves no snapshot.
	//
	// The optional origin says WHY the publish happened, for that row's `via`
	// pair. It is variadic so the callers that do not know — and the many tests
	// that do not care — keep the zero value, which means a direct, hand-pressed
	// publish. Passing more than one is an error, not a merge.
	SetEntryPublishState(ctx context.Context, e *domain.Entry, status string, publishedAt *time.Time, opts ...domain.PublishOrigin) error
	DeleteEntry(ctx context.Context, tenantID string, contentTypeID, id uuid.UUID) error
	ListEntries(ctx context.Context, f ListEntriesFilter) ([]*domain.Entry, int, error)
	// GetEntriesByIDs fetches many entries of the same tenant in ONE query. It
	// backs relation populate (ADR-006 Amendment 6), where a page of N entries
	// each carrying M relation values would otherwise cost N×M round trips —
	// the N+1 that makes a populate parameter unusable at page sizes anyone
	// would ask for.
	//
	// It deliberately takes NO content_type_id, unlike GetEntry. The ids of one
	// page span every type the populated fields point at, and splitting them by
	// type would put the query count back in proportion to the schema. The
	// CALLER matches each row's ContentTypeID against the type it expected —
	// which it must do anyway, because a row's type decides which field-level
	// read permissions mask it.
	//
	// matchPublished narrows to rows the public may see: published status AND a
	// snapshot present. Both halves are needed — a retracted entry KEEPS its
	// published_payload (migration 000033), so the snapshot alone would serve
	// content that was deliberately taken down.
	//
	// FALSE is the admin expansion (ADR-006 Amendment 7): an editor expands the
	// working copy at both ends, so a target that has never been published is an
	// ordinary draft to them rather than a dangling reference. The flag is the
	// only difference between the two audiences' queries — tenancy is bound the
	// same way in both, by the WHERE predicate and by RLS.
	//
	// Rows the tenant does not own, or that no longer exist, are simply absent
	// from the result. Order is unspecified: the caller holds the id order that
	// matters (the one in the parent entry's payload) and indexes by id.
	GetEntriesByIDs(ctx context.Context, tenantID string, ids []uuid.UUID, matchPublished bool) ([]*domain.Entry, error)
	// EntryExists is used to validate relation values point at a live entry in
	// the same tenant.
	EntryExists(ctx context.Context, tenantID string, contentTypeID, id uuid.UUID) (bool, error)

	// Scheduled publish / unpublish (ADR-017). A schedule is an INTENT the
	// requester filed against a specific version, not a job: the worker may
	// quite properly decline to execute it, and every exit from 'pending' is
	// terminal.
	//
	// The stale/superseded transitions are NOT on this interface, deliberately.
	// They fire inside UpdateEntry and SetEntryPublishState, in those
	// statements' own transactions, because a schedule invalidated in a second
	// call leaves a window where the worker publishes content the editor has
	// already moved past. Making them callable would invite exactly that.
	CreateEntrySchedule(ctx context.Context, s *domain.EntrySchedule) error
	// GetPendingEntrySchedule returns (nil, nil) when the entry has none — the
	// ordinary case, which is why it is not an error.
	GetPendingEntrySchedule(ctx context.Context, tenantID string, entryID uuid.UUID) (*domain.EntrySchedule, error)
	// GetActionableEntrySchedule returns the newest pending OR stale row, or
	// (nil, nil). It is what decorates an admin EntryDTO: a stale schedule is a
	// release that will not happen until someone acts, so hiding it is hiding
	// the one state that needs a person (ADR-017 §2).
	GetActionableEntrySchedule(ctx context.Context, tenantID string, entryID uuid.UUID) (*domain.EntrySchedule, error)
	// ListActionableEntrySchedules is GetActionableEntrySchedule for a page of
	// entries at once — one query per LIST request rather than one per row, so
	// decorating a page of N entries costs the same single round trip the page
	// itself did. Entries with no pending-or-stale schedule are simply absent
	// from the map; the caller indexes by entry id the same way populateFor's
	// callers do.
	ListActionableEntrySchedules(ctx context.Context, tenantID string, entryIDs []uuid.UUID) (map[uuid.UUID]*domain.EntrySchedule, error)
	// GetLatestEntrySchedule returns the newest row in ANY state, or
	// apperrors.ErrNotFound. Terminal rows are the interesting ones: they carry
	// the answer to "why did this not go live".
	GetLatestEntrySchedule(ctx context.Context, tenantID string, entryID uuid.UUID) (*domain.EntrySchedule, error)
	// CancelEntrySchedule moves the pending row to 'cancelled' and returns it;
	// apperrors.ErrNotFound when nothing was pending. The row is never deleted.
	CancelEntrySchedule(ctx context.Context, tenantID string, entryID uuid.UUID) (*domain.EntrySchedule, error)
	// ListDueEntrySchedules is the worker's CROSS-TENANT candidate scan. It does
	// not lock anything and two replicas may see the same row; ClaimDueEntrySchedule
	// settles the race. See its implementation for why it cannot go through the
	// ordinary tenant RLS scope.
	ListDueEntrySchedules(ctx context.Context, now time.Time, limit int) ([]*domain.EntrySchedule, error)
	// ClaimDueEntrySchedule takes the row lock that IS the claim, and must be
	// called inside WithTx by a caller that performs the whole operation there.
	// (nil, nil) means another replica has it.
	ClaimDueEntrySchedule(ctx context.Context, tenantID string, id uuid.UUID, now time.Time) (*domain.EntrySchedule, error)
	// FinishEntrySchedule moves a claimed schedule to a terminal state. The
	// executedAt/errMsg pair must match the state — the database enforces both
	// biconditionals (migration 000041).
	FinishEntrySchedule(ctx context.Context, tenantID string, id uuid.UUID, state string, executedAt *time.Time, errMsg string) error

	// Publish revisions (ADR-018). These two are the READ half; the write half
	// is deliberately absent, because recordPublishRevision must run inside
	// SetEntryPublishState's own transaction and an interface method would let a
	// caller record a release in a transaction of its own — a revision that
	// survives a rolled-back publish is a release that never happened, offered
	// by name to a restore that will happily perform it.
	//
	// This is the same reasoning that keeps ListEntryRevisions (000034's
	// working-copy history) OFF this interface entirely. The difference is that
	// these two have real callers: ADR-018 gives publish revisions the read and
	// restore paths §5 withheld, and the masking §5 was waiting for is applied
	// by the service before either payload reaches a response.
	//
	// ListEntryPublishRevisions returns metadata only, newest first, and needs no
	// limit: retention bounds it at domain.MaxPublishRevisionsPerEntry.
	ListEntryPublishRevisions(ctx context.Context, tenantID string, entryID uuid.UUID) ([]domain.EntryPublishRevision, error)
	// GetEntryPublishRevision returns one release WITH its payload, or
	// apperrors.ErrNotFound — which covers "no such ordinal" and "retention
	// discarded it" alike, because the answer to both is the same.
	GetEntryPublishRevision(ctx context.Context, tenantID string, entryID uuid.UUID, revisionNo int) (*domain.EntryPublishRevision, error)

	// Review decisions (ADR-014 Amendment: review decisions). A decision row is
	// append-only history, same shape as a publish revision; there is no update
	// or withdraw on this interface for the same reason ListEntryPublishRevisions
	// has none — a reviewer who changes their mind approves the next version,
	// which is a new fact, not a correction to an old one.
	CreateEntryReviewDecision(ctx context.Context, d *domain.EntryReviewDecision) error
	// GetLatestEntryReviewDecision returns the newest decision in ANY relation
	// to the entry's current version — (nil, nil) when none exists. The caller
	// (dto.go's withReviewDecision, the wasPending fix in UpdateEntry) is what
	// compares EntryVersion against the entry's current version; this method
	// does not filter on it, so both callers can ask the same question against
	// whichever version they hold.
	GetLatestEntryReviewDecision(ctx context.Context, tenantID string, entryID uuid.UUID) (*domain.EntryReviewDecision, error)
	// ListLatestEntryReviewDecisions is GetLatestEntryReviewDecision for a page
	// of entries at once — the same one-query-per-list-request shape
	// ListActionableEntrySchedules gives GetActionableEntrySchedule. Entries
	// with no decision at all are simply absent from the map.
	ListLatestEntryReviewDecisions(ctx context.Context, tenantID string, entryIDs []uuid.UUID) (map[uuid.UUID]*domain.EntryReviewDecision, error)
	// ListEntryReviewDecisions returns full history, newest first, capped at
	// domain.MaxReviewDecisionsPerEntry — the GET .../review-decisions read.
	ListEntryReviewDecisions(ctx context.Context, tenantID string, entryID uuid.UUID) ([]domain.EntryReviewDecision, error)

	// FindEntryIdempotency / RecordEntryIdempotency back idempotent creation
	// (ADR-013 §9). They are methods on THIS interface rather than a separate
	// store because the record must be written in the same transaction as the
	// entry it names: a commit that lands one without the other is precisely the
	// duplicate the table exists to prevent, arriving one crash later.
	//
	// That is also why they are not internal/pkg/idempotency. Its store takes a
	// raw pgx.Tx, which the content repository never exposes — WithTx hands out a
	// bound ContentRepository instead.
	//
	// FindEntryIdempotency returns nil, nil when the key has not been spent.
	FindEntryIdempotency(ctx context.Context, tenantID, actorKey, idemKey string) (*EntryIdempotency, error)
	// RecordEntryIdempotency returns ErrIdempotencyKeyTaken when the key was
	// spent between the lookup and this insert — two concurrent first-tries of
	// the same key, which is what a client timing out and retrying while the
	// original is still in flight looks like from here.
	RecordEntryIdempotency(ctx context.Context, rec EntryIdempotency) error

	// Schema proposals (ADR-013 §3 step 8): a schema change filed for a person
	// to approve. On THIS interface, not a store of its own, because approving
	// must record the decision in the same transaction that applies the change —
	// a commit that lands one without the other leaves the audit trail saying
	// something that did not happen, or a spent proposal that can be spent again.
	CreateSchemaProposal(ctx context.Context, p *SchemaProposal) error
	GetSchemaProposal(ctx context.Context, tenantID string, id uuid.UUID) (*SchemaProposal, error)
	// GetOwnSchemaProposal is the proposer's own single read. The credential —
	// principal AND agent name, not the principal alone — is part of the match,
	// because every agent a person mints shares that person's principal id.
	GetOwnSchemaProposal(ctx context.Context, tenantID string, id uuid.UUID,
		proposedBy uuid.UUID, kind string, agentID *string) (*SchemaProposal, error)
	// ListOwnSchemaProposals is the same ownership match without an id. It takes
	// the credential rather than deriving it, for the same reason the single
	// read does: "own" is the CREDENTIAL, and a signature that accepted only a
	// principal would compile against a WHERE clause that lists a sibling
	// agent's proposals.
	ListOwnSchemaProposals(ctx context.Context, tenantID string,
		proposedBy uuid.UUID, kind string, agentID *string, limit int) ([]*SchemaProposal, error)
	ListSchemaProposals(ctx context.Context, tenantID string, limit int) ([]*SchemaProposal, error)
	// DecideSchemaProposal returns ErrProposalNotPending when the row was
	// already decided or has expired — the concurrency answer, not a repeat of
	// the service's own check.
	//
	// appliedSteps is JSONB (an array of indices into the stored plan, or nil)
	// written in the SAME statement that flips status, so an approval and the
	// record of what it ran cannot land as two writes that disagree (缺口計畫
	// 5.1). Reject and the two failure paths above pass nil.
	DecideSchemaProposal(ctx context.Context, tenantID string, id uuid.UUID,
		status string, decidedBy uuid.UUID, now time.Time, appliedSteps []byte) error

	// Media assets (ADR-005). Bytes live in object storage; these rows are the
	// metadata and the entry↔asset links.
	CreateMediaAsset(ctx context.Context, a *domain.MediaAsset) error
	GetMediaAsset(ctx context.Context, tenantID string, id uuid.UUID) (*domain.MediaAsset, error)
	// ListMediaAssets is the admin media library: only UPLOADED assets (a
	// reservation whose bytes never landed is not content), newest first, one
	// COUNT(*) and one SELECT sharing the same WHERE — the same shape as
	// ListEntries and for the same reason: total and the page must never
	// disagree about what was filtered.
	ListMediaAssets(ctx context.Context, tenantID string, f MediaListFilter) ([]*domain.MediaAsset, int, error)
	MarkMediaUploaded(ctx context.Context, tenantID string, id uuid.UUID, size int64, contentType string) error
	// UpdateMediaAssetMetadata edits only the CLIENT-DECLARED columns and returns
	// the row as it now stands. It is separate from MarkMediaUploaded on purpose:
	// that method records what actually landed in the bucket ("never from the
	// client", per its own comment), and routing a client's claim through it would
	// make that comment false for the columns it still governs.
	UpdateMediaAssetMetadata(ctx context.Context, tenantID string, id uuid.UUID, p MediaAssetPatch) (*domain.MediaAsset, error)
	// DeleteMediaAsset removes the row (variant rows cascade) and returns the
	// storage keys of the variant objects the caller must remove from the
	// bucket — gathered under lock in the delete's own transaction, see the
	// implementation for the race that makes that necessary.
	DeleteMediaAsset(ctx context.Context, tenantID string, id uuid.UUID) ([]string, error)
	// ReplaceEntryMedia rewrites one entry's asset links; called on every entry
	// write so the links track the payload.
	ReplaceEntryMedia(ctx context.Context, tenantID string, entryID uuid.UUID, assetIDs []uuid.UUID) error
	// AssetIsPublished reports whether the asset is referenced by at least one
	// PUBLISHED entry — the question the delivery path asks before signing a
	// read URL. This is why entry_media exists.
	AssetIsPublished(ctx context.Context, tenantID string, assetID uuid.UUID) (bool, error)
	// MediaReferencedBy is the media-asset reverse lookup (ADR-024, 2.6a/2.8a):
	// every entry (draft, published, or both) whose entry_media /
	// entry_media_published rows name this asset. It backs both the
	// referenced-by endpoint and the delete-time in-use check, so the same
	// merge and the same page shape answer both questions.
	MediaReferencedBy(ctx context.Context, tenantID string, assetID uuid.UUID, limit, offset int) (ReferencedByResult, error)

	// Reverse references (ADR-024, 2.8a). entry_relations tracks every
	// relation field's value the same way entry_media tracks file/richtext
	// references — maintained on write, not scanned at read time.
	//
	// ReplaceEntryRelations rewrites one entry's relation links in one
	// transaction: delete-then-insert, mirroring ReplaceEntryMedia. Called at
	// the same three write points ReplaceEntryMedia is (create, update,
	// restore), plus a published-snapshot rewrite inside SetEntryPublishState
	// that ReplaceEntryMedia has no equivalent call for (entry_media_published
	// is instead copied wholesale from entry_media at publish time; relations
	// need the same copy — see the implementation).
	ReplaceEntryRelations(ctx context.Context, tenantID string, entryID uuid.UUID, refs []EntryRelationRef) error
	// EntryReferencedBy is entry_relations' reverse lookup: every entry whose
	// working copy, published snapshot, or both hold a relation value naming
	// targetID, one row per (referring entry, field).
	EntryReferencedBy(ctx context.Context, tenantID string, targetID uuid.UUID, limit, offset int) (ReferencedByResult, error)

	// Media variants (ADR-019). Rows exist iff the asset is an uploaded image;
	// the worker owns every state change after the enqueue.
	//
	// EnqueueMediaVariants upserts every preset row (and `original`) to
	// pending. Called inside WithTx next to MarkMediaUploaded so the two facts
	// commit together, and again by the admin re-enqueue endpoint.
	EnqueueMediaVariants(ctx context.Context, tenantID string, assetID uuid.UUID, storageKey string) error
	ListMediaVariants(ctx context.Context, tenantID string, assetID uuid.UUID) ([]*domain.MediaVariant, error)
	// ListPendingMediaVariantAssets is the worker's CROSS-TENANT candidate scan
	// (one entry per asset with a due pending row). Same contract as
	// ListDueEntrySchedules: no locks, replicas may overlap, the claim settles it.
	ListPendingMediaVariantAssets(ctx context.Context, now time.Time, limit int) ([]MediaVariantAssetRef, error)
	// ClaimPendingMediaVariants locks one asset's due rows (FOR UPDATE SKIP
	// LOCKED) and must run inside WithTx; empty means nothing to do.
	ClaimPendingMediaVariants(ctx context.Context, tenantID string, assetID uuid.UUID, now time.Time) ([]*domain.MediaVariant, error)
	// FinishMediaVariant writes one claimed row's outcome; false means the row
	// vanished (asset deleted) and the caller should discard what it produced.
	FinishMediaVariant(ctx context.Context, tenantID string, assetID uuid.UUID, preset string, o MediaVariantOutcome) (bool, error)

	// Activity record (ADR-014 §3): who did what to which thing, and did it
	// work — refusals included, as first-class rows.
	//
	// RecordActivity joins the caller's transaction when there is one, so a
	// write and its record commit together; see the implementation for why that
	// matters and why a refusal cannot have it.
	RecordActivity(ctx context.Context, a *domain.Activity) error
	ListActivity(ctx context.Context, f ActivityFilter) ([]*domain.Activity, error)
	// ListPendingReview is the release queue (ADR-014 §2): every entry in the
	// tenant, across all types, whose working copy is not what the public sees.
	// Its criterion is pendingReviewExpr, which is deliberately WIDER than
	// unpublishedChangesExpr — see that variable for why reusing the narrow one
	// would drop every draft. reviewDecisionNotSentBackExpr (ADR-014 Amendment:
	// review decisions) additionally excludes an entry a reviewer sent back that
	// nobody has touched since.
	ListPendingReview(ctx context.Context, f PendingReviewFilter) ([]*domain.Entry, error)
	// SearchEntries is the cross-type search endpoint (ADR-021 §3): every entry
	// in the tenant, across all types, whose search_text matches every term of
	// f.Query, ordered by updated_at desc. Shares ListPendingReview's
	// data-level visibility gate (see SearchEntriesFilter).
	SearchEntries(ctx context.Context, f SearchEntriesFilter) ([]SearchEntryRow, error)
	// ListLocales is GET /api/v1/content/locales: every locale in use in the
	// tenant (optionally narrowed to one content type) and how many entries
	// carry it, ordered by locale. One GROUP BY query, not a scan per locale —
	// this is what lets a console draw a locale switcher without guessing what
	// the tenant localised. Shares SearchEntries' data-level visibility gate.
	ListLocales(ctx context.Context, f ListLocalesFilter) ([]LocaleCountRow, error)
	// EntryFieldAuthors folds that stream down to one row per payload key: who
	// last changed each field of one entry (ADR-014 §6). `since` is exclusive.
	EntryFieldAuthors(ctx context.Context, tenantID string, entryID uuid.UUID, since *time.Time) ([]*domain.FieldAuthor, error)

	// Public delivery read volume (ADR-004 amendment). AddDeliveryReads is
	// called by the periodic flusher with an already-aggregated count — never
	// once per request; see migration 000017 for why.
	AddDeliveryReads(ctx context.Context, tenantID string, day time.Time, n int64) error
	DeliveryReadsForDay(ctx context.Context, tenantID string, day time.Time) (int64, error)
}
