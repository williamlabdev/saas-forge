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

// RelationRef names one field that points at a content type by name. It exists
// because relation_entity stores the type NAME, so renaming or deleting a type
// is a cross-type operation rather than a local one.
type RelationRef struct {
	TypeName string
	FieldKey string
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

// EntryCursor is the keyset a delivery caller pages by. It is (CreatedAt, ID),
// not CreatedAt alone: created_at is not unique — a seeding run writes many rows
// inside the same microsecond — and a non-unique key silently skips or repeats
// rows across page boundaries. ID breaks every tie, so the order is total.
//
// Callers never construct one: it round-trips through the opaque token the API
// hands out (service.encodeCursor).
type EntryCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
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
	// ListRelationReferrers finds fields in OTHER types of the same tenant whose
	// relation_entity names typeName.
	ListRelationReferrers(ctx context.Context, tenantID, typeName string) ([]RelationRef, error)

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
	SetEntryPublishState(ctx context.Context, e *domain.Entry, status string, publishedAt *time.Time) error
	DeleteEntry(ctx context.Context, tenantID string, contentTypeID, id uuid.UUID) error
	ListEntries(ctx context.Context, f ListEntriesFilter) ([]*domain.Entry, int, error)
	// EntryExists is used to validate relation values point at a live entry in
	// the same tenant.
	EntryExists(ctx context.Context, tenantID string, contentTypeID, id uuid.UUID) (bool, error)

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
	DecideSchemaProposal(ctx context.Context, tenantID string, id uuid.UUID,
		status string, decidedBy uuid.UUID, now time.Time) error

	// Media assets (ADR-005). Bytes live in object storage; these rows are the
	// metadata and the entry↔asset links.
	CreateMediaAsset(ctx context.Context, a *domain.MediaAsset) error
	GetMediaAsset(ctx context.Context, tenantID string, id uuid.UUID) (*domain.MediaAsset, error)
	MarkMediaUploaded(ctx context.Context, tenantID string, id uuid.UUID, size int64, contentType string) error
	// UpdateMediaAssetMetadata edits only the CLIENT-DECLARED columns and returns
	// the row as it now stands. It is separate from MarkMediaUploaded on purpose:
	// that method records what actually landed in the bucket ("never from the
	// client", per its own comment), and routing a client's claim through it would
	// make that comment false for the columns it still governs.
	UpdateMediaAssetMetadata(ctx context.Context, tenantID string, id uuid.UUID, p MediaAssetPatch) (*domain.MediaAsset, error)
	DeleteMediaAsset(ctx context.Context, tenantID string, id uuid.UUID) error
	// ReplaceEntryMedia rewrites one entry's asset links; called on every entry
	// write so the links track the payload.
	ReplaceEntryMedia(ctx context.Context, tenantID string, entryID uuid.UUID, assetIDs []uuid.UUID) error
	// AssetIsPublished reports whether the asset is referenced by at least one
	// PUBLISHED entry — the question the delivery path asks before signing a
	// read URL. This is why entry_media exists.
	AssetIsPublished(ctx context.Context, tenantID string, assetID uuid.UUID) (bool, error)

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
	// would drop every draft.
	ListPendingReview(ctx context.Context, f PendingReviewFilter) ([]*domain.Entry, error)
	// EntryFieldAuthors folds that stream down to one row per payload key: who
	// last changed each field of one entry (ADR-014 §6). `since` is exclusive.
	EntryFieldAuthors(ctx context.Context, tenantID string, entryID uuid.UUID, since *time.Time) ([]*domain.FieldAuthor, error)

	// Public delivery read volume (ADR-004 amendment). AddDeliveryReads is
	// called by the periodic flusher with an already-aggregated count — never
	// once per request; see migration 000017 for why.
	AddDeliveryReads(ctx context.Context, tenantID string, day time.Time, n int64) error
	DeliveryReadsForDay(ctx context.Context, tenantID string, day time.Time) (int64, error)
}
