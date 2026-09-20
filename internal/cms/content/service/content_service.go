package service

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/idempotency"
	"github.com/williamlabdev/saas-forge/internal/pkg/objectstore"
)

// Authz action contracts for this domain. They mirror the stable string
// convention used by internal/pkg/authz (e.g. "user:read") and work as-is under
// AUTHZ_MODE=allow; tenant isolation is enforced unconditionally regardless of
// the authorizer (see authorize + every repo call below).
const (
	ActionContentList   = authz.ActionContentList
	ActionContentRead   = authz.ActionContentRead
	ActionContentCreate = authz.ActionContentCreate
	ActionContentUpdate = authz.ActionContentUpdate
	ActionContentDelete = authz.ActionContentDelete
	// Releasing an entry. Split off content:update so that writing the working
	// copy and putting it in front of the public are separately grantable
	// (ADR-014 §1). Same tenant roles as the content verbs — no person loses
	// anything; the agent gate is what the split is spent on. Retracting stays
	// on content:update, deliberately.
	ActionContentPublish = authz.ActionContentPublish
	// Reading the tenant-wide activity stream (§3) and the release queue (§2).
	// Both used to ride content:list, which handed them to viewer; ruled the
	// other way 2026-08-06, so each has a verb of its own and viewer has neither.
	// Separate verbs on purpose — see the authz package for why the roles
	// coinciding today is not the claim that they are one power.
	ActionContentActivityRead = authz.ActionContentActivityRead
	ActionContentReviewList   = authz.ActionContentReviewList
	// Destructive schema verbs are owner/admin only — an editor keeps every
	// content capability but cannot drop a collection.
	ActionContentSchemaWrite = authz.ActionContentSchemaWrite
	// Planning a schema change is its own verb so that it can be granted apart
	// from applying one (ADR-013 §3). Same roles as write today.
	ActionContentSchemaPlan = authz.ActionContentSchemaPlan
	// Filing a schema change for a person to approve (ADR-013 §3 step 8). Same
	// roles again; the split from plan is that proposing writes a row.
	ActionContentSchemaPropose = authz.ActionContentSchemaPropose
	// Additive schema edits (add field, edit label). Split off content:update so
	// that entry-write access does not carry schema-reshaping with it (ADR-013
	// §B). Same tenant roles as the content verbs — no person loses anything.
	ActionContentSchemaAmend = authz.ActionContentSchemaAmend
)

const resourceType = "content"

// ContentService is the runtime-dynamic content API: define types, grow their
// schema at runtime, and read/write entries of any type through one surface.
type ContentService interface {
	CreateContentType(ctx context.Context, in CreateTypeInput) (ContentTypeDTO, error)
	AddField(ctx context.Context, typeName string, in FieldInput) (ContentTypeDTO, error)

	// Schema mutation (schema_mutation.go). Each of these either migrates the
	// stored data in the same transaction as the definition change, or refuses —
	// see that file's header for why there is no third option.
	UpdateContentType(ctx context.Context, typeName string, in UpdateTypeInput) (ContentTypeDTO, error)
	RenameContentType(ctx context.Context, typeName string, in RenameInput) (ContentTypeDTO, error)
	DeleteContentType(ctx context.Context, typeName string) error
	UpdateField(ctx context.Context, typeName, key string, in UpdateFieldInput) (ContentTypeDTO, error)
	RenameField(ctx context.Context, typeName, key string, in RenameInput) (ContentTypeDTO, error)
	DeleteField(ctx context.Context, typeName, key string, force bool) (ContentTypeDTO, error)

	// Reusable components (component.go, ADR-020): the component-side twins
	// of the schema verbs above. Writes are admin-only by construction; the
	// two reads are gated by the component's referrers for an agent.
	CreateComponent(ctx context.Context, in CreateComponentInput) (ComponentDTO, error)
	ListComponents(ctx context.Context) ([]ComponentDTO, error)
	// Built-in component templates (ADR-020 Amendment 2): a curated catalog
	// (code, not tenant rows) a caller can install as a real component in one
	// call. ListComponentTemplates is authorized like ListComponents;
	// InstallComponentTemplate runs through CreateComponent's own path, so it
	// is authorized, validated, quota-checked and audited identically.
	ListComponentTemplates(ctx context.Context) ([]ComponentTemplateDTO, error)
	InstallComponentTemplate(ctx context.Context, name string) (ComponentDTO, error)
	GetComponent(ctx context.Context, name string) (ComponentDTO, error)
	UpdateComponent(ctx context.Context, name string, in UpdateComponentInput) (ComponentDTO, error)
	RenameComponent(ctx context.Context, name string, in RenameInput) (ComponentDTO, error)
	DeleteComponent(ctx context.Context, name string) error
	AddComponentField(ctx context.Context, name string, in FieldInput) (ComponentDTO, error)
	UpdateComponentField(ctx context.Context, name, key string, in UpdateFieldInput) (ComponentDTO, error)
	RenameComponentField(ctx context.Context, name, key string, in RenameInput) (ComponentDTO, error)
	DeleteComponentField(ctx context.Context, name, key string, force bool) (ComponentDTO, error)

	// Schema as a portable artifact (artifact.go, ADR-008).
	ExportSchema(ctx context.Context) (domain.Artifact, error)
	PlanSchema(ctx context.Context, art domain.Artifact, prune bool) (PlanResult, error)
	ApplySchema(ctx context.Context, art domain.Artifact, prune bool) (PlanResult, error)

	// Schema proposals (schema_proposal.go, ADR-013 §3 step 8). Propose is the
	// agent's half; the other four are the approver's, and they authorize the
	// apply verb because approving is applying.
	ProposeSchema(ctx context.Context, art domain.Artifact, prune bool) (SchemaProposalDTO, error)
	ListSchemaProposals(ctx context.Context) ([]SchemaProposalDTO, error)
	GetSchemaProposal(ctx context.Context, id uuid.UUID) (SchemaProposalDTO, error)
	// GetOwnSchemaProposal is the proposer's narrowed read of the row it filed —
	// the one proposal surface an agent may reach.
	GetOwnSchemaProposal(ctx context.Context, id uuid.UUID) (OwnSchemaProposalDTO, error)
	// ListOwnSchemaProposals is the same narrowed view without an id — the only
	// way a proposer can find a proposal again once the response that filed it
	// is gone.
	ListOwnSchemaProposals(ctx context.Context) ([]OwnSchemaProposalDTO, error)
	// ApproveSchemaProposal applies the stored plan. steps is nil for "approve
	// everything" (unchanged pre-5.1 behaviour, byte-for-byte); non-nil selects
	// a subset by index into the STORED plan's Steps — see the doc comment on
	// the implementation in schema_proposal.go for the full contract (缺口計畫
	// 5.1).
	ApproveSchemaProposal(ctx context.Context, id uuid.UUID, steps []int) (PlanResult, error)
	RejectSchemaProposal(ctx context.Context, id uuid.UUID) error
	ListContentTypes(ctx context.Context) ([]ContentTypeDTO, error)
	GetContentType(ctx context.Context, typeName string) (ContentTypeDTO, error)

	CreateEntry(ctx context.Context, typeName string, payload json.RawMessage) (EntryDTO, error)
	ListEntries(ctx context.Context, typeName string, in ListEntriesInput) (*EntryListResult, error)
	GetEntry(ctx context.Context, typeName string, id uuid.UUID, in GetEntryInput) (EntryDTO, error)
	UpdateEntry(ctx context.Context, typeName string, id uuid.UUID, payload json.RawMessage, expectedVersion int) (EntryDTO, error)
	DeleteEntry(ctx context.Context, typeName string, id uuid.UUID) error

	// CreateLocalizedEntry creates an entry in a specific locale. With
	// TranslationOf set the new row joins that entry's translation group — a
	// translation, which publishes independently of its siblings. Without it the
	// entry starts a group of its own. CreateEntry is the default-locale case of
	// this; it stays a separate method so the common call site reads plainly.
	CreateLocalizedEntry(ctx context.Context, typeName string, in CreateLocalizedInput) (EntryDTO, error)

	// ListTranslations returns every locale of one entry's translation group.
	ListTranslations(ctx context.Context, typeName string, id uuid.UUID) ([]EntryDTO, error)

	// CreatePreviewLink mints a short-lived credential that shows ONE entry's
	// working copy through the public delivery edge (ADR-006). It is the only
	// way a preview token comes into existence.
	CreatePreviewLink(ctx context.Context, typeName string, id uuid.UUID) (PreviewLinkDTO, error)

	// Media assets (ADR-005). Bytes go client↔storage directly; the platform
	// only reserves keys, confirms uploads, and decides who may read.
	CreateMediaUpload(ctx context.Context, in CreateMediaUploadInput) (MediaUploadDTO, error)
	CompleteMediaUpload(ctx context.Context, id uuid.UUID) (MediaAssetDTO, error)
	GetMediaAsset(ctx context.Context, id uuid.UUID) (MediaAssetDTO, error)
	// ListMediaAssets is the admin media library: paginated, newest first,
	// UPLOADED assets only. Items carry no variants — see MediaListResult.
	ListMediaAssets(ctx context.Context, in ListMediaInput) (MediaListResult, error)
	// UpdateMediaAsset patches an asset's client-declared metadata (filename, alt
	// text, dimensions). Separate from CompleteMediaUpload because that method
	// records observed facts and this one records claims — see service/media.go.
	UpdateMediaAsset(ctx context.Context, id uuid.UUID, in UpdateMediaAssetInput) (MediaAssetDTO, error)
	// ResolveMediaURL signs a read URL for the original ("" preset) or a
	// finished rendition (ADR-019); an unfinished rendition falls back to the
	// original, an unknown preset is a 400.
	ResolveMediaURL(ctx context.Context, id uuid.UUID, preset string) (string, time.Time, error)
	// DeleteMediaAsset removes an asset's metadata and stored bytes. force=false
	// refuses with 409 CONTENT_MEDIA_IN_USE when entry_media or
	// entry_media_published still names the asset; force=true skips that check
	// (ADR-024).
	DeleteMediaAsset(ctx context.Context, id uuid.UUID, force bool) error
	// SweepOrphans deletes one bounded batch of orphaned assets (neither link
	// table names, older than OlderThan — overdue reservations included).
	// Same verb as DeleteMediaAsset; per-item fate in SweepResult.
	SweepOrphans(ctx context.Context, in SweepInput) (SweepResult, error)
	// EnqueueMediaVariants (re)queues every preset of an uploaded image for the
	// transform worker and returns the asset with its variant rows reset.
	EnqueueMediaVariants(ctx context.Context, id uuid.UUID) (MediaAssetDTO, error)

	// EntryReferencedBy and MediaReferencedBy answer "what points at this" —
	// the reverse direction of a relation field / a file or richtext media
	// reference (ADR-024, 2.8a). Both merge a same entry's draft and published
	// link rows into one row per (entry, field); an agent credential's result is
	// narrowed to the content types it may see, the same way toComponentDTO
	// narrows used_by.
	EntryReferencedBy(ctx context.Context, typeName string, id uuid.UUID, limit, offset int) (ReferencedByDTO, error)
	MediaReferencedBy(ctx context.Context, id uuid.UUID, limit, offset int) (ReferencedByDTO, error)

	// SetEntryStatus moves an entry between editorial states (draft |
	// published). Publishing is always explicit — no write path flips status as
	// a side effect. expectedVersion carries the same optimistic-lock semantics
	// as UpdateEntry, so a publish can't silently land on top of a concurrent
	// content edit.
	SetEntryStatus(ctx context.Context, typeName string, id uuid.UUID, status string, expectedVersion int) (EntryDTO, error)

	// Scheduled publish / unpublish (schedule.go, ADR-017). ScheduleEntry files
	// the intent and authorizes it with the SAME verb the immediate operation
	// takes, so a schedule can never release content its requester could not
	// have released by hand. The worker that executes it later does not
	// re-authorize — see ADR-017 §4.
	ScheduleEntry(ctx context.Context, typeName string, id uuid.UUID, in ScheduleEntryInput) (EntryScheduleDTO, error)
	// CancelEntrySchedule withdraws the pending intent. GetEntrySchedule returns
	// the newest schedule in ANY state — the terminal ones carry the answer to
	// "why did this not go live".
	CancelEntrySchedule(ctx context.Context, typeName string, id uuid.UUID) error
	GetEntrySchedule(ctx context.Context, typeName string, id uuid.UUID) (EntryScheduleDTO, error)

	// Publish revisions and restore (publish_revision.go, ADR-018). Every publish
	// leaves one stored snapshot of what went live; these read them back and
	// replay one into the WORKING COPY.
	//
	// RestoreEntryRevision takes content:update, not content:publish, because it
	// does not publish: it writes the draft and leaves the live snapshot exactly
	// as it was. Releasing the restored draft stays a separate act by a person
	// holding content:publish, which is what keeps ADR-014 §1's human gate
	// intact rather than routed around. A delivery, preview or AGENT credential
	// is refused all four.
	ListEntryRevisions(ctx context.Context, typeName string, id uuid.UUID) ([]EntryRevisionDTO, error)
	GetEntryRevision(ctx context.Context, typeName string, id uuid.UUID, revisionNo int) (EntryRevisionDetailDTO, error)
	// DiffEntryRevision compares the revision against the CURRENT WORKING COPY,
	// not against the live snapshot: the question it answers is what pressing
	// restore would change about the draft in front of you.
	DiffEntryRevision(ctx context.Context, typeName string, id uuid.UUID, revisionNo int) (EntryRevisionDiffDTO, error)
	// RestoreEntryRevision is a FULL REPLACE of the working copy, not a PATCH
	// merge, and it runs through the same validation an ordinary save does —
	// including the optimistic lock, which expectedVersion carries.
	RestoreEntryRevision(ctx context.Context, typeName string, id uuid.UUID, in RestoreEntryInput, expectedVersion int) (RestoreEntryResultDTO, error)

	// Review decisions (review_decision.go, ADR-014 Amendment: review
	// decisions). RequestChanges takes content:publish, the SAME verb a publish
	// itself takes — a reviewer sending an entry back is exercising the identical
	// human gate a reviewer clearing it would, and giving the refusal a
	// second, narrower verb would be new role plumbing for a decision that
	// already sits behind the right one.
	//
	// It is version-scoped rather than a status a workflow would move the entry
	// through: the decision names the working-copy version it was made against,
	// and the entry leaves the queue only while that version is still current.
	// An author's next save moves the version on, and the entry — now unreviewed
	// content, by construction — falls back into the release queue on its own,
	// with no separate "re-submit" action to remember or a workflow state to
	// mismanage.
	RequestChanges(ctx context.Context, typeName string, id uuid.UUID, in RequestChangesInput, expectedVersion int) (ReviewDecisionDTO, error)
	// ListEntryReviewDecisions returns the entry's decision history, newest
	// first — authorized exactly like reading the entry, since a decision names
	// no more than GetEntry itself already exposes about who touched it.
	ListEntryReviewDecisions(ctx context.Context, typeName string, id uuid.UUID) ([]ReviewDecisionDTO, error)

	// Usage reports the tenant's plan, its content limits, and live counts (R4b).
	Usage(ctx context.Context) (UsageDTO, error)

	// ListActivity returns the tenant's activity stream — who did what to which
	// thing and whether it worked, refusals included (activity.go, ADR-014 §3).
	ListActivity(ctx context.Context, in ListActivityInput) ([]ActivityDTO, error)

	// ListPendingReview is the release queue — everything in the tenant whose
	// working copy is not what the public sees, across all content types
	// (pending_review.go, ADR-014 §2). It carries no payloads; the diff does.
	ListPendingReview(ctx context.Context, in ListPendingReviewInput) ([]PendingEntryDTO, error)

	// Search is the cross-type content search endpoint (search.go, ADR-021 §3):
	// every entry in the tenant, across all content types, matching in.Query,
	// newest-edited first. See ListEntriesInput.Query for the per-type
	// equivalent this complements.
	Search(ctx context.Context, in SearchInput) ([]SearchResultDTO, error)

	// ListLocales is GET /api/v1/content/locales: every locale in use in the
	// tenant, and how many entries carry it — the inventory a console needs to
	// draw a locale switcher, since nothing else on the wire names which
	// locales a tenant actually opted into (locales.go).
	ListLocales(ctx context.Context, in ListLocalesInput) ([]LocaleDTO, error)

	// EntryFieldAttribution answers "who last changed each field" for one entry,
	// which is what the release screen puts beside each line of the diff so the
	// person approving it knows whose work they are endorsing (ADR-014 §6).
	EntryFieldAttribution(ctx context.Context, typeName string, id uuid.UUID) (EntryAttributionDTO, error)

	// Webhooks (webhook.go, ADR-011): the tenant's registered receivers of
	// content events. Create returns the signing secret exactly once; rotation
	// is delete-and-register.
	CreateWebhook(ctx context.Context, in CreateWebhookInput) (WebhookCreatedDTO, error)
	ListWebhooks(ctx context.Context) ([]WebhookDTO, error)
	DeleteWebhook(ctx context.Context, id uuid.UUID) error
}

// FieldInput describes a field at definition time. Mirrors EntityField in
// admin-app.schema.json.
type FieldInput struct {
	Key      string `json:"key"`
	Type     string `json:"type"`
	Label    string `json:"label"`
	Required bool   `json:"required"`
	// Multiple makes the field hold an array of Type. Legal only on the types in
	// domain.AllowedMultipleTypes(), and immutable once the field exists.
	Multiple   bool     `json:"multiple"`
	EnumValues []string `json:"enum_values"`
	// ReadRoles / WriteRoles are the tenant roles allowed to see and to send this
	// field's value. Omitted or empty means unrestricted, so every existing
	// caller keeps working unchanged (migration 000026, domain/field_permission.go).
	ReadRoles      []string `json:"read_roles"`
	WriteRoles     []string `json:"write_roles"`
	RelationEntity string   `json:"relation_entity"`
	// Component names the reusable component a `component` field takes its
	// shape from (ADR-020). Required on that type, refused on every other.
	Component string `json:"component"`
	// Components is the allowed-list of a `dynamiczone` field: the components
	// its items may declare (ADR-020 Amendment 1). Required (non-empty, no
	// duplicates, each one existing) on that type, refused on every other.
	// Unlike Component it is MUTABLE — see UpdateFieldInput.
	Components []string `json:"components"`
	// Description is the author's sentence about the field, shown under the
	// control in the admin form. Documentation, not a rule — nothing validates
	// against it, and no value can contradict it.
	Description string `json:"description"`
	// The constraint attributes (unique, format, pattern, min, max) are
	// promoted from the domain type so the wire keys are the artifact's keys.
	// Which of them a type may carry is decided by
	// domain.ValidateFieldConstraints, at definition time, by name.
	domain.FieldConstraints
}

type CreateTypeInput struct {
	Name   string       `json:"name"`
	Label  string       `json:"label"`
	Fields []FieldInput `json:"fields"`
	// ReadRoles / WriteRoles / OwnOnlyRoles are DATA-level permission: which
	// tenant roles may read and write the ENTRIES of this type, and which of them
	// see only the entries they created. Omitted or empty means unrestricted, so
	// every existing caller keeps working unchanged (migration 000027,
	// domain/type_permission.go).
	ReadRoles    []string `json:"read_roles"`
	WriteRoles   []string `json:"write_roles"`
	OwnOnlyRoles []string `json:"own_only_roles"`
}

// CreateLocalizedInput describes a localized create.
type CreateLocalizedInput struct {
	Payload json.RawMessage
	// Locale defaults to domain.DefaultLocale when empty.
	Locale string
	// TranslationOf, when set, is an existing entry whose translation group the
	// new row joins. It must belong to the same tenant and content type.
	TranslationOf *uuid.UUID
	// CopyFromSource, when true, seeds the new entry's payload from
	// TranslationOf's WORKING COPY: the source's payload is the base and
	// Payload's top-level keys overlay it, then the merged document runs
	// through the SAME validation (and the same field-write and required-field
	// guards) an ordinary create does. Requires TranslationOf — there is no
	// source to copy from otherwise.
	//
	// Off by default, and deliberately not inferred from an empty Payload: a
	// caller that sends `{}` without asking for a copy gets the existing
	// behaviour (an entry that fails required-field validation, same as
	// CreateEntry always has), not a silent duplicate of the source. Implicit
	// copying would also mean "I forgot a field" and "I want the source's
	// content" produce the same request, which is exactly the ambiguity this
	// flag exists to remove.
	CopyFromSource bool
	// IdempotencyKey, when non-empty, makes this create replayable: the same key
	// and the same request return the FIRST entry it produced instead of a second
	// one (ADR-013 §9). The same key with a different request is refused, which
	// is the half that matters for an unattended writer — see 000036.
	//
	// Empty means no promise, which is what every caller written before this
	// existed passes. It is create-only by design: PATCH carries the caller's
	// expected version and unpublish is idempotent in the status column, so
	// neither can produce the duplicate this guards against.
	IdempotencyKey string
}

type ListEntriesInput struct {
	Filters []string // each "key:op:value"
	Sort    string   // "key:asc" | "key:desc"
	// Fields narrows each entry's payload to the named keys (ADR-013 §7). Empty
	// means the whole payload, so every caller written before this existed is
	// unaffected.
	//
	// It exists for a token budget, not for security: an agent that needs three
	// keys out of forty pays for forty on every row, and the model's context is
	// the scarce resource the whole tool surface is shaped around. It narrows
	// what a caller may ALREADY read — the field permission gate runs first and
	// independently, so this can only ever subtract.
	Fields []string
	// Populate names the RELATION fields whose targets to expand into each
	// entry's `related` (ADR-006 Amendment 6; Amendment 7 for admin). Empty
	// means no expansion, which is what every caller written before this got.
	//
	// Delivery and admin both expand, each in its OWN view — snapshots for
	// delivery, working copies for admin — and preview is refused rather than
	// ignored (see guardPopulateAudience). It does not change `data`: the ids
	// stay where they were, and the expansion arrives beside them, so this
	// parameter can never alter the type of a field a client already decodes.
	Populate []string
	// Status narrows to one editorial state; empty means all states (the admin
	// default). Validated against domain.ValidStatus before it reaches SQL.
	Status string
	// Locale narrows to one language; empty means every locale.
	Locale string
	Limit  int
	Offset int
	// Cursor is the opaque token from a previous page's next_cursor. Delivery
	// audience only — it and Offset are mutually exclusive, and each audience
	// refuses the other's parameter rather than ignoring it.
	Cursor string
	// Query is the free-text `q` parameter (ADR-021): non-empty enables a
	// search, split on whitespace into terms each ANDed as a substring match
	// against search_text (admin) or published_search_text (delivery, via
	// f.MatchPublished — see buildWhere's predicateSearchColumn). Composes with
	// Filters — a caller may combine `q` with an ordinary field filter, and both
	// predicates apply. Capped at 200 characters (validated in ListEntries) so
	// one request cannot force an unbounded number of ILIKE clauses onto the
	// query; the `contains` operator on an individual field filter is unaffected
	// and stays exactly as it was — see parseFilter's comment.
	Query string
}

// GetEntryInput carries the read options of a SINGLE entry fetch.
//
// It is a struct with one field rather than a bare []string parameter because
// the single-entry read has, until now, had nothing to negotiate — and the next
// thing it grows (a projection, most likely, since ?fields= is asymmetric
// between the two reads today) should not be a fourth positional argument on a
// method every read path calls.
type GetEntryInput struct {
	// Populate names the relation fields to expand, exactly as
	// ListEntriesInput.Populate does and under the same audience rule. Empty
	// means no expansion.
	Populate []string
}

// Quota holds a tenant's content limits for one request; 0 = unlimited.
// Resolved per-request from the tenant's plan (TKT-R4b) — no longer a static
// process-wide value.
type Quota struct {
	PlanName            string
	MaxTypesPerTenant   int
	MaxEntriesPerTenant int
	MaxFieldsPerType    int
	MaxEntryBytes       int
	// SoftThresholdPct is the plan's soft-warning line (e.g. 80); crossing it
	// on a successful write emits a usage warning (not a block).
	SoftThresholdPct int
}

// PlanResolver maps a tenant slug to its content Quota (TKT-R4b). Defined here
// (consumer side) so content doesn't import the tenant module; the composition
// root adapts tenant.PlanForTenant into a PlanFunc.
type PlanResolver interface {
	QuotaForTenant(ctx context.Context, tenantSlug string) (Quota, error)
}

// PlanFunc adapts a plain function to PlanResolver.
type PlanFunc func(ctx context.Context, tenantSlug string) (Quota, error)

func (f PlanFunc) QuotaForTenant(ctx context.Context, slug string) (Quota, error) {
	return f(ctx, slug)
}

type contentService struct {
	repo  repository.ContentRepository
	authz authz.Authorizer
	plans PlanResolver
	// delivery counts public read volume per tenant; nil = not recorded.
	delivery *DeliveryCounter
	// store holds media bytes; nil = media disabled on this deployment.
	store objectstore.Store
	// preview mints preview credentials; nil = no delivery key, so no previews.
	preview PreviewSigner
	// notify pushes "a proposal is waiting" / "an entry needs review" to the
	// tenant's owner/admin; nil = both stay pull-only (see proposal_notify.go
	// and notifyEntryPendingReview below).
	notify Notifier
	// now is the service's clock, nil meaning time.Now().UTC — see clock().
	//
	// It became injectable with scheduled publish (ADR-017), and not for
	// convenience: "does this schedule run yet" and "is this run_at in the
	// future" are decisions about time itself, and a test that can only assert
	// them by sleeping is a test that is slow when it passes and flaky when it
	// fails. Everything that reads a clock in this service now goes through
	// clock(), so a fixed clock is total rather than partial — a test that froze
	// half the service's time would be worse than one that froze none.
	now func() time.Time
}

// clock is the service's only source of "now".
//
// Nil-defaulting rather than requiring every constructor to supply one: the
// zero value must be the production clock, or a constructor added later that
// forgets the field would produce a service stuck at the zero time — and the
// symptom of that (published_at in year 1, schedules never due) is far enough
// from the cause to cost a day.
func (s *contentService) clock() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

// WithClock replaces the service's clock. Tests only — nothing in production
// calls it, and the nil default is what production uses.
func (s *contentService) WithClock(now func() time.Time) *contentService {
	s.now = now
	return s
}

// WithObjectStore enables the media flow (ADR-005). Without it the media
// endpoints answer 501 rather than failing obscurely deeper down.
func (s *contentService) WithObjectStore(store objectstore.Store) *contentService {
	s.store = store
	return s
}

func NewContentService(repo repository.ContentRepository, authz authz.Authorizer, plans PlanResolver) ContentService {
	return &contentService{repo: repo, authz: authz, plans: plans}
}

// NewContentServiceWithDelivery additionally records public delivery read
// volume into the tenant's daily bucket (ADR-004 amendment). Counting is
// buffered in the counter, not written per request — see DeliveryCounter.
func NewContentServiceWithDelivery(repo repository.ContentRepository, authz authz.Authorizer, plans PlanResolver, counter *DeliveryCounter) ContentService {
	return &contentService{repo: repo, authz: authz, plans: plans, delivery: counter}
}

// ErrQuotaExceeded is the per-tenant content backstop tripping (429). It is an
// abuse guard; the limit now comes from the tenant's plan (R4b) rather than a
// global env cap (R4a).
var ErrQuotaExceeded = apperrors.New("CONTENT_QUOTA_EXCEEDED", "content quota exceeded for this tenant", 429)

// guardEntryBytes rejects a stored payload that exceeds the plan's per-entry cap.
func guardEntryBytes(q Quota, payload json.RawMessage) error {
	if q.MaxEntryBytes > 0 && len(payload) > q.MaxEntryBytes {
		return ErrQuotaExceeded.WithDetails(map[string]any{
			"resource": "entry_bytes", "limit": q.MaxEntryBytes,
		})
	}
	return nil
}

// ErrNoTenant rejects subjects without an active tenant. Without this guard,
// every tenant-less token would read and write the same "" tenant bucket —
// the exact isolation collapse R1 describes (plan §6).
var ErrNoTenant = apperrors.New("CONTENT_NO_TENANT", "content requires an active tenant membership", 403)

// authorize resolves the subject and runs the policy check. The returned
// subject's TenantID is the only source of tenancy — the API never accepts a
// tenant parameter.
// contentType names the content type the request concerns, or "" when the
// request concerns no single type (media, webhooks, usage, whole-schema
// artifacts, the type list itself).
//
// It is a separate parameter rather than a reuse of resourceID because
// resourceID means different things on different paths — an entry UUID on
// update/get/delete/publish, the literal "collection" on create/list — so most
// entry paths cannot see the type through it. resourceID also travels into the
// OPA input as Resource{Type, ID}; changing what it carries would change policy
// semantics, which adding a parameter does not (ADR-013 §4).
//
// It is read by refuseOutsideAgentScope below — the per-credential AllowedTypes
// check — at this one chokepoint.
func (s *contentService) authorize(ctx context.Context, action, resourceID, contentType string) (authn.Subject, error) {
	return s.authorizeAgentScope(ctx, action, resourceID, func(sub authn.Subject) error {
		return refuseOutsideAgentScope(sub, contentType)
	})
}

// authorizeArtifact is authorize() for the requests whose content types arrive
// in the BODY instead of in a parameter (ADR-013 補裁 E).
//
// A whole-schema artifact names no type in any parameter, so authorize() would
// pass "" and §4's untyped rule would shut an agent out — which is what it did
// to schema:plan, the verb §3 created FOR agents. The ruling took candidate 2:
// enforce the whitelist against what the artifact actually lists, and leave the
// "" rule untouched. It is untouched for the reason §A gave: relaxing it puts
// media, webhooks and usage back within reach in the same edit.
//
// This is a SECOND enforcement point, and the ADR says so in as many words:
// "the only chokepoint is authorize()" is no longer literally true. What keeps
// that from decaying into "remember to add the check" is the shape — a rule
// over a KIND of request rather than an exception for one endpoint — plus a
// structural test that walks every artifact-taking method on the interface.
func (s *contentService) authorizeArtifact(ctx context.Context, action, resourceID string, art domain.Artifact) (authn.Subject, error) {
	return s.authorizeAgentScope(ctx, action, resourceID, func(sub authn.Subject) error {
		return refuseArtifactOutsideAgentScope(sub, art)
	})
}

// authorizeAgentScope is the body both share. The two differ only in HOW the
// agent's whitelist is applied, so that is the parameter; everything else —
// subject, tenant, the delivery-credential rule, the authorizer — stays in one
// place. Two copies of this would be two places for the next rule to be added
// to, and one of them would be missed.
func (s *contentService) authorizeAgentScope(ctx context.Context, action, resourceID string, agentScope func(authn.Subject) error) (authn.Subject, error) {
	sub, ok := authn.SubjectFromContext(ctx)
	if !ok {
		return authn.Subject{}, apperrors.ErrUnauthorized
	}
	if sub.TenantID == "" {
		return authn.Subject{}, ErrNoTenant
	}
	// A public delivery credential is read-only, enforced HERE rather than left
	// to the RBAC role it was minted with (ADR-004: the published-only property
	// must not depend on a single layer getting it right). Every write path in
	// this service goes through authorize, so this is the one chokepoint.
	if sub.PublicDelivery && !isReadAction(action) {
		return authn.Subject{}, apperrors.ErrForbidden
	}
	if err := agentScope(sub); err != nil {
		return authn.Subject{}, err
	}
	if err := s.authz.Allow(ctx, authz.Input{
		Action:   action,
		Resource: authz.Resource{Type: resourceType, ID: resourceID},
	}); err != nil {
		return authn.Subject{}, err
	}
	return sub, nil
}

// refuseOutsideAgentScope enforces ADR-013 §4: an agent credential may only
// touch the content types its AllowedTypes whitelist names.
//
// Three named refusals rather than one because they mean different things to
// whoever reads the log: a credential minted without a scope, a credential
// pointed at a path that has no type, and a credential pointed at a type
// somebody else's credential owns. None of them leaks anything the caller does
// not already know — this is the credential's own scope, not another tenant's
// data — so unlike guardOwned they are allowed to say what happened.
//
// The empty contentType is the load-bearing case. Media, webhooks, usage,
// whole-schema artifacts and the type list itself cannot name a type because
// they concern none, so an agent is shut out of them BY CONSTRUCTION. Widening
// this case to make one of those paths work would silently reopen all of them
// (ADR-013 §A); the answer there was to change the caller — cms_describe walks
// AllowedTypes and fetches types one at a time — not to relax this.
func refuseOutsideAgentScope(sub authn.Subject, contentType string) error {
	if err := refuseUnscopedAgentCredential(sub); err != nil {
		return err
	}
	if !sub.IsAgent() {
		return nil
	}
	if contentType == "" {
		return apperrors.New(
			"CONTENT_AGENT_SCOPE_UNTYPED",
			"an agent credential may not use a path that concerns no single content type",
			403,
		)
	}
	if !sub.AllowsContentType(contentType) {
		return refuseContentType(contentType)
	}
	return nil
}

// refuseUnscopedAgentCredential holds the two refusals that are facts about the
// CREDENTIAL rather than about what it was pointed at, so both enforcement
// points ask them in the same order and neither can drift into asking only the
// type question of a credential that has no scope at all.
func refuseUnscopedAgentCredential(sub authn.Subject) error {
	if !sub.IsAgent() {
		return nil
	}
	if sub.AllowedTypes == nil {
		// Unset is not "everything". A credential that never said what it may
		// touch may touch nothing (ADR-013 §1). Unreachable through
		// IssueAgentToken, which refuses to mint one; kept because the class of
		// bug it guards is a new minting path that forgets.
		return apperrors.New(
			"CONTENT_AGENT_SCOPE_UNSET",
			"an agent credential without a content type whitelist may touch no content",
			403,
		)
	}
	if sub.PrincipalID == nil {
		// No principal means no party who answers for this credential's writes,
		// and own_only would confine its reads to an author no row can match.
		// The schema refuses such a row outright
		// (entries_created_by_agent_has_principal_check), so without this the
		// same refusal arrives as a 500 from the database on write and as
		// silently empty results on read. Refused here, once, as what it is.
		return apperrors.New(
			"CONTENT_AGENT_PRINCIPAL_MISSING",
			"an agent credential without a principal has nobody to answer for its writes",
			403,
		)
	}
	return nil
}

// refuseArtifactOutsideAgentScope enforces ADR-013 補裁 E: an agent may submit a
// whole-schema artifact only if EVERY type the document lists is inside its
// whitelist.
//
// All-or-nothing rather than per-step filtering, because a partially accepted
// artifact would mean the plan an agent gets back is not a plan for the document
// it submitted — the caller would have to diff the answer against its own file
// to discover which half was ignored.
//
// An artifact listing no types passes vacuously. That is not a gap: with nothing
// to touch, the plan it produces is a diff of the agent's own whitelist against
// the schema, which is what the credential already knows. Applying it is a
// separate verb the agent does not hold (§3).
func refuseArtifactOutsideAgentScope(sub authn.Subject, art domain.Artifact) error {
	if err := refuseUnscopedAgentCredential(sub); err != nil {
		return err
	}
	if !sub.IsAgent() {
		return nil
	}
	for _, t := range art.Types {
		// A type with an empty name lands on AllowsContentType("") and is
		// refused. Fail-closed is the right answer for an unnamed type here:
		// it is a malformed document, not a request for the untyped paths.
		if !sub.AllowsContentType(t.Name) {
			return refuseContentType(t.Name)
		}
	}
	return nil
}

// refuseContentType is one refusal with one code, so a caller cannot tell from
// the answer WHICH enforcement point produced it — the two are the same rule.
// The type is named because it is the caller's own scope, not another tenant's
// data (see the note above refuseOutsideAgentScope).
func refuseContentType(contentType string) error {
	return apperrors.New(
		"CONTENT_AGENT_TYPE_NOT_ALLOWED",
		"this agent credential is not scoped to that content type",
		403,
	).WithDetail("content_type", contentType)
}

// actor returns the id to record as the author of a write, or nil when there is
// no PERSON to name. nil is stored as SQL NULL and reads as "not recorded" —
// the honest answer, and better than a plausible-looking id nobody can resolve.
//
// PublicDelivery is refused every write at authorize() above, so that branch is
// unreachable through any route that exists today. It is here so a future
// non-human writer — an import job, a tenant API key — cannot silently deposit
// a service-account uuid in a column an admin UI will render as a person's
// name. When such a writer really exists, the answer is an actor-KIND
// discriminator, not overloading this one; ADR-009's trigger conditions record
// it. (This comment said ADR-006 until 2026-08-05; ADR-006 never carried that
// trigger — the citation was wrong in both places that made it.)
//
// uuid.Nil is reachable, not theoretical: the dev-header middleware accepts
// X-User-Id: 00000000-0000-0000-0000-000000000000 because uuid.Parse succeeds
// on it.
//
// SINCE ADR-013 THAT WRITER EXISTS, and the answer is the one this comment
// anticipated: an actor-kind discriminator, with the column meaning refined
// from "who typed" to "who answers for it". For an agent credential that is
// the principal who minted it (Subject.ResponsibleUserID) — a real person, one
// an admin UI can resolve, and the party actually accountable for what their
// agent wrote. The service-account uuid this comment refused is still refused;
// what changed is that there is now a nameable person to record instead of it.
func actor(sub authn.Subject) *uuid.UUID {
	id := sub.ResponsibleUserID()
	if sub.PublicDelivery || id == uuid.Nil {
		return nil
	}
	return &id
}

// provenanceOf answers, for one write, both halves of ADR-013 §2: who is
// answerable, and what did the typing.
//
// Returned together, and assigned together at the one call site that creates a
// row, because they are one fact split across three columns and the schema
// refuses the combinations that would mean nothing (an agent id without the
// agent kind, an agent row without an answerable principal). Splitting them
// into three separate helpers is how a caller ends up setting one and not the
// others.
func provenanceOf(sub authn.Subject) (createdBy *uuid.UUID, kind string, agentID *string) {
	if !sub.IsAgent() {
		return actor(sub), domain.ActorKindHuman, nil
	}
	return actor(sub), domain.ActorKindAgent, sub.AgentID
}

// writeActorOf packages provenanceOf's trio for the repository methods that
// change content in bulk (DeleteField / RenameField) rather than one entry at a
// time, and so have no domain.Entry to stamp.
//
// It exists so those two call sites cannot assemble the trio themselves — the
// same reason provenanceOf returns three values at once. See domain.WriteActor.
func writeActorOf(sub authn.Subject) domain.WriteActor {
	id, kind, agentID := provenanceOf(sub)
	return domain.WriteActor{Kind: kind, UserID: id, AgentID: agentID}
}

// recordUpdateProvenance stamps the LAST-WRITE trio on an entry — who answers,
// what did the typing, and which bot if it was one (ADR-014 §4).
//
// It takes the entry and assigns, rather than returning three values, for the
// reason provenanceOf states about its own trio: the schema refuses every
// half-written combination, so a call site that can set one of these without
// setting the others is a call site that will eventually do it. There are two
// such sites and they have nothing else in common — one saves a draft, the
// other moves the published snapshot — so the shared part is exactly this.
func recordUpdateProvenance(e *domain.Entry, sub authn.Subject) {
	id, kind, agentID := provenanceOf(sub)
	e.UpdatedBy, e.UpdatedByKind, e.UpdatedByAgent = id, &kind, agentID
}

// isReadAction reports whether an action only reads. Listed explicitly (rather
// than "not one of the writes") so a future verb is refused for delivery
// credentials until someone deliberately classifies it — fail closed.
func isReadAction(action string) bool {
	switch action {
	case ActionContentList, ActionContentRead:
		return true
	default:
		return false
	}
}

// resolveQuota fetches the tenant's plan limits for this request (TKT-R4b).
func (s *contentService) resolveQuota(ctx context.Context, tenantSlug string) (Quota, error) {
	return s.plans.QuotaForTenant(ctx, tenantSlug)
}

// --- content types ----------------------------------------------------------

func (s *contentService) CreateContentType(ctx context.Context, in CreateTypeInput) (_ ContentTypeDTO, err error) {
	act := s.activityWrite(ctx, domain.ActivitySchemaWrite, strings.TrimSpace(in.Name))
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentCreate, "collection", "")
	if err != nil {
		return ContentTypeDTO{}, err
	}
	name := strings.TrimSpace(in.Name)
	if !domain.ValidName(name) {
		return ContentTypeDTO{}, apperrors.New("CONTENT_TYPE_NAME_INVALID", "invalid content type name", 422).
			WithDetails(map[string]any{"name": in.Name})
	}
	if len(in.Fields) == 0 {
		return ContentTypeDTO{}, apperrors.New("CONTENT_TYPE_NO_FIELDS", "content type needs at least one field", 422)
	}
	quota, err := s.resolveQuota(ctx, sub.TenantID)
	if err != nil {
		return ContentTypeDTO{}, err
	}
	if quota.MaxFieldsPerType > 0 && len(in.Fields) > quota.MaxFieldsPerType {
		return ContentTypeDTO{}, ErrQuotaExceeded.WithDetails(map[string]any{
			"resource": "fields", "limit": quota.MaxFieldsPerType,
		})
	}
	if quota.MaxTypesPerTenant > 0 {
		count, err := s.repo.CountContentTypes(ctx, sub.TenantID)
		if err != nil {
			return ContentTypeDTO{}, err
		}
		if count >= quota.MaxTypesPerTenant {
			return ContentTypeDTO{}, ErrQuotaExceeded.WithDetails(map[string]any{
				"resource": "content_types", "limit": quota.MaxTypesPerTenant,
			})
		}
	}

	// Data-level permission, validated at DEFINITION time for the reason the
	// field lists are: an unknown role is a list nobody matches, so the
	// collection silently closes to everyone and the operator finds out from a
	// support ticket. No backfill guard is needed on a create — a type with no
	// entries has none that could vanish.
	readRoles, err := normalizeTypeRoles(name, "read_roles", in.ReadRoles)
	if err != nil {
		return ContentTypeDTO{}, err
	}
	writeRoles, err := normalizeTypeRoles(name, "write_roles", in.WriteRoles)
	if err != nil {
		return ContentTypeDTO{}, err
	}
	ownOnlyRoles, err := normalizeTypeRoles(name, "own_only_roles", in.OwnOnlyRoles)
	if err != nil {
		return ContentTypeDTO{}, err
	}
	now := time.Now().UTC()
	ct := &domain.ContentType{
		ID:           uuid.New(),
		TenantID:     sub.TenantID,
		Name:         name,
		Label:        strings.TrimSpace(in.Label),
		ReadRoles:    readRoles,
		WriteRoles:   writeRoles,
		OwnOnlyRoles: ownOnlyRoles,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	seen := map[string]bool{}
	for _, fi := range in.Fields {
		f, err := buildField(ct.ID, fi, now)
		if err != nil {
			return ContentTypeDTO{}, err
		}
		if seen[f.Key] {
			return ContentTypeDTO{}, apperrors.New("CONTENT_FIELD_DUPLICATE", "duplicate field key", 422).
				WithDetails(map[string]any{"field": f.Key})
		}
		seen[f.Key] = true
		ct.Fields = append(ct.Fields, f)
	}
	if err := s.resolveComponentRefs(ctx, sub.TenantID, ct.Fields); err != nil {
		return ContentTypeDTO{}, err
	}

	if err := s.repo.CreateContentType(ctx, ct); err != nil {
		return ContentTypeDTO{}, err
	}
	if quota.MaxTypesPerTenant > 0 {
		if count, err := s.repo.CountContentTypes(ctx, sub.TenantID); err == nil {
			softWarn(ctx, "content_types", count, quota.MaxTypesPerTenant, quota.SoftThresholdPct)
		}
	}
	return s.contentTypeDTO(ctx, ct, sub), nil
}

func (s *contentService) AddField(ctx context.Context, typeName string, in FieldInput) (_ ContentTypeDTO, err error) {
	act := s.activityWrite(ctx, domain.ActivitySchemaWrite, typeName)
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentSchemaAmend, typeName, typeName)
	if err != nil {
		return ContentTypeDTO{}, err
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return ContentTypeDTO{}, err
	}
	now := time.Now().UTC()
	f, err := buildField(ct.ID, in, now)
	if err != nil {
		return ContentTypeDTO{}, err
	}
	if _, exists := ct.FieldByKey(f.Key); exists {
		return ContentTypeDTO{}, apperrors.New("CONTENT_FIELD_EXISTS", "field already defined", 409).
			WithDetails(map[string]any{"field": f.Key})
	}
	quota, err := s.resolveQuota(ctx, sub.TenantID)
	if err != nil {
		return ContentTypeDTO{}, err
	}
	if quota.MaxFieldsPerType > 0 && len(ct.Fields) >= quota.MaxFieldsPerType {
		return ContentTypeDTO{}, ErrQuotaExceeded.WithDetails(map[string]any{
			"resource": "fields", "limit": quota.MaxFieldsPerType,
		})
	}
	one := []domain.Field{f}
	if err := s.resolveComponentRefs(ctx, sub.TenantID, one); err != nil {
		return ContentTypeDTO{}, err
	}
	f = one[0]
	if err := s.repo.AddField(ctx, sub.TenantID, &f); err != nil {
		return ContentTypeDTO{}, err
	}
	ct.Fields = append(ct.Fields, f)
	return s.contentTypeDTO(ctx, ct, sub), nil
}

func (s *contentService) ListContentTypes(ctx context.Context) (_ []ContentTypeDTO, err error) {
	act := s.activityRead(ctx, domain.ActivityTypeList, "")
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentList, "collection", "")
	if err != nil {
		return nil, err
	}
	cts, err := s.repo.ListContentTypes(ctx, sub.TenantID)
	if err != nil {
		return nil, err
	}
	out := make([]ContentTypeDTO, len(cts))
	for i, ct := range cts {
		out[i] = s.contentTypeDTO(ctx, ct, sub)
	}
	return out, nil
}

func (s *contentService) GetContentType(ctx context.Context, typeName string) (_ ContentTypeDTO, err error) {
	act := s.activityRead(ctx, domain.ActivityTypeRead, typeName)
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentRead, typeName, typeName)
	if err != nil {
		return ContentTypeDTO{}, err
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return ContentTypeDTO{}, err
	}
	return s.contentTypeDTO(ctx, ct, sub), nil
}

// buildField validates a FieldInput and converts it to a domain.Field.
func buildField(contentTypeID uuid.UUID, in FieldInput, now time.Time) (domain.Field, error) {
	key := strings.TrimSpace(in.Key)
	if !domain.ValidFieldKey(key) {
		return domain.Field{}, apperrors.New("CONTENT_FIELD_KEY_INVALID", "invalid field key", 422).
			WithDetails(map[string]any{"field": in.Key})
	}
	if !domain.ValidFieldType(in.Type) {
		return domain.Field{}, apperrors.New("CONTENT_FIELD_TYPE_UNKNOWN", "unknown field type", 422).
			WithDetails(map[string]any{"field": key, "type": in.Type})
	}
	if in.Multiple && !domain.MultipleAllowedFor(in.Type) {
		return domain.Field{}, errFieldMultipleUnsupported(key, in.Type)
	}
	if in.Type == domain.FieldTypeEnum && len(in.EnumValues) == 0 {
		return domain.Field{}, apperrors.New("CONTENT_FIELD_ENUM_EMPTY", "enum field needs enum_values", 422).
			WithDetails(map[string]any{"field": key})
	}
	if in.Type == domain.FieldTypeRelation && strings.TrimSpace(in.RelationEntity) == "" {
		return domain.Field{}, apperrors.New("CONTENT_FIELD_RELATION_MISSING", "relation field needs relation_entity", 422).
			WithDetails(map[string]any{"field": key})
	}
	component := strings.TrimSpace(in.Component)
	if in.Type == domain.FieldTypeComponent && component == "" {
		return domain.Field{}, errComponentRequired(key)
	}
	if in.Type != domain.FieldTypeComponent && component != "" {
		return domain.Field{}, errComponentNotApplicable(key, in.Type)
	}
	zone, err := normalizeZoneComponents(key, in.Type, in.Components)
	if err != nil {
		return domain.Field{}, err
	}
	constraints := normalizeConstraints(in.FieldConstraints)
	if cerr := domain.ValidateFieldConstraints(in.Type, in.Multiple, constraints); cerr != nil {
		return domain.Field{}, errConstraintDefinition(key, cerr)
	}
	// Non-enum fields carry no enum_values; JSON omits the key so in.EnumValues
	// is nil. The enum_values column is TEXT[] NOT NULL, and pgx encodes a nil
	// slice as SQL NULL — so normalize nil to an empty (non-nil) slice to avoid
	// a NOT NULL violation on insert.
	enumValues := in.EnumValues
	if enumValues == nil {
		enumValues = []string{}
	}
	// Permission lists are validated HERE, at definition time, and not merely
	// interpreted at read time. An unknown role in write_roles is a list nobody
	// matches — the field silently becomes unwritable by everyone, and the
	// operator finds out when an editor files a bug about a form that will not
	// save. Refusing the typo is the only moment it is still cheap.
	readRoles, err := normalizeRoles(key, in.ReadRoles)
	if err != nil {
		return domain.Field{}, err
	}
	writeRoles, err := normalizeRoles(key, in.WriteRoles)
	if err != nil {
		return domain.Field{}, err
	}
	return domain.Field{
		ID:               uuid.New(),
		ContentTypeID:    contentTypeID,
		Key:              key,
		Type:             in.Type,
		Label:            strings.TrimSpace(in.Label),
		Required:         in.Required,
		Multiple:         in.Multiple,
		EnumValues:       enumValues,
		ReadRoles:        readRoles,
		WriteRoles:       writeRoles,
		RelationEntity:   strings.TrimSpace(in.RelationEntity),
		ComponentName:    component,
		ZoneComponents:   zone,
		Description:      strings.TrimSpace(in.Description),
		FieldConstraints: constraints,
		CreatedAt:        now,
	}, nil
}

// normalizeZoneComponents validates the `components` attribute against the
// field's type and returns the list to store: nil for every type but
// dynamiczone, a trimmed, duplicate-free, order-preserving copy for that one.
//
// Order is preserved rather than sorted because it is the order an editor's
// "add a block" menu offers, which is a schema decision the author made; a
// sorted list would silently discard it and there would be no way to get it
// back. Duplicates are refused rather than deduped for the reason the
// multi-value duplicate check gives: deduping returns bytes the caller did not
// send.
//
// Whether each name EXISTS is not decided here — that needs the repository,
// and it is resolveComponentRefs' job, which is where a `component` field's
// name is resolved too.
func normalizeZoneComponents(key, fieldType string, in []string) ([]string, error) {
	names := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			return nil, errZoneComponentDuplicate(key, name)
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	if fieldType != domain.FieldTypeDynamicZone {
		if len(names) > 0 {
			return nil, errZoneComponentsNotApplicable(key, fieldType)
		}
		return nil, nil
	}
	if len(names) == 0 {
		return nil, errZoneComponentsRequired(key)
	}
	return names, nil
}

// resolveComponentRefs turns the component NAME a `component` field was
// declared with into the id the row stores and the sub-fields the type needs
// to validate against (ADR-020). Same tenant, by construction: the lookup is
// tenant-scoped, so another tenant's component of the same name is simply
// not found.
func (s *contentService) resolveComponentRefs(ctx context.Context, tenantID string, fields []domain.Field) error {
	cache := map[string]*domain.Component{}
	lookup := func(fieldKey, name string) (*domain.Component, error) {
		if c, ok := cache[name]; ok {
			return c, nil
		}
		c, err := s.repo.GetComponentByName(ctx, tenantID, name)
		if err != nil {
			if errors.Is(err, apperrors.ErrNotFound) {
				return nil, errComponentNotFound(fieldKey, name)
			}
			return nil, err
		}
		cache[name] = c
		return c, nil
	}
	for i := range fields {
		f := &fields[i]
		switch f.Type {
		case domain.FieldTypeComponent:
			c, err := lookup(f.Key, f.ComponentName)
			if err != nil {
				return err
			}
			id := c.ID
			f.ComponentID = &id
			f.ComponentFields = c.Fields
		case domain.FieldTypeDynamicZone:
			// A zone stores NAMES, so there is no id to resolve — but each name
			// must still exist, and the sub-fields must be attached, or the
			// validator would accept any item body for an unknown component.
			// ZoneFields is keyed by name because that is what the item carries.
			f.ZoneFields = make(map[string][]domain.Field, len(f.ZoneComponents))
			for _, name := range f.ZoneComponents {
				c, err := lookup(f.Key, name)
				if err != nil {
					return err
				}
				f.ZoneFields[name] = c.Fields
			}
		}
	}
	return nil
}

// normalizeConstraints trims the two string attributes. Whitespace around a
// pattern is never meant, and a format of " slug" is a typo the CHECK would
// refuse with a message naming a column instead of a field.
func normalizeConstraints(c domain.FieldConstraints) domain.FieldConstraints {
	c.Format = strings.TrimSpace(c.Format)
	c.Pattern = strings.TrimSpace(c.Pattern)
	return c
}

// --- entries ----------------------------------------------------------------

// CreateEntry is the default-locale, own-group case of CreateLocalizedEntry.
// Both go through one implementation so the two paths cannot drift apart.
func (s *contentService) CreateEntry(ctx context.Context, typeName string, payload json.RawMessage) (EntryDTO, error) {
	return s.CreateLocalizedEntry(ctx, typeName, CreateLocalizedInput{Payload: payload})
}

func (s *contentService) CreateLocalizedEntry(ctx context.Context, typeName string, in CreateLocalizedInput) (_ EntryDTO, err error) {
	act := s.activityWrite(ctx, domain.ActivityEntryCreate, typeName)
	defer func() { act.finish(ctx, err) }()
	payload := in.Payload
	locale := in.Locale
	if locale == "" {
		locale = domain.DefaultLocale
	}
	if !domain.ValidLocale(locale) {
		return EntryDTO{}, apperrors.New("CONTENT_LOCALE_INVALID", "locale must be a valid language tag", 400).
			WithDetails(map[string]any{"locale": in.Locale})
	}
	sub, err := s.authorize(ctx, ActionContentCreate, typeName, typeName)
	if err != nil {
		return EntryDTO{}, err
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return EntryDTO{}, err
	}
	// Idempotency is checked here — after authorize and after the type resolves,
	// before any of the work below. After authorize, because a caller who may not
	// create in this type must not be able to learn which keys are spent; before
	// the quota check, because a replay creates nothing and charging it against
	// the quota would let a retry storm exhaust a tenant that added one entry.
	idemKey, err := idempotency.NormalizeKey(in.IdempotencyKey)
	if err != nil {
		return EntryDTO{}, err
	}
	var actorKey string
	var fingerprint []byte
	if idemKey != "" {
		if actorKey, err = idempotencyActorKey(sub); err != nil {
			return EntryDTO{}, err
		}
		if fingerprint, err = createRequestFingerprint(typeName, in); err != nil {
			return EntryDTO{}, err
		}
		replay, err := s.replayCreate(ctx, sub, ct, actorKey, idemKey, fingerprint)
		if err != nil {
			return EntryDTO{}, err
		}
		if replay != nil {
			// Nothing was created, so nothing is recorded. This is cancel()'s
			// documented shape — a method that succeeded without doing anything —
			// and the stream would otherwise show one create line per retry for an
			// entry that exists once.
			act.cancel()
			return *replay, nil
		}
	}
	quota, err := s.resolveQuota(ctx, sub.TenantID)
	if err != nil {
		return EntryDTO{}, err
	}
	if quota.MaxEntriesPerTenant > 0 {
		count, err := s.repo.CountEntriesForTenant(ctx, sub.TenantID)
		if err != nil {
			return EntryDTO{}, err
		}
		if count >= quota.MaxEntriesPerTenant {
			return EntryDTO{}, ErrQuotaExceeded.WithDetails(map[string]any{
				"resource": "entries", "limit": quota.MaxEntriesPerTenant,
			})
		}
	}
	// DATA-level permission comes before the field-level checks, because it is
	// the coarser refusal: a caller who may not write this collection at all
	// should hear that, not a complaint about one key of a document they were
	// never entitled to create.
	if err := guardTypeWrite(ct, sub); err != nil {
		return EntryDTO{}, err
	}
	// Field permission, before validation for the same reason UpdateEntry checks
	// before the merge: the refusal must name the key the CALLER sent. The
	// required check comes first because it is the dead end — a caller who may
	// not write a required field cannot create an entry of this type at all, and
	// hearing about the key they did send would send them round the loop again.
	if err := guardRequiredWritable(ct, sub); err != nil {
		return EntryDTO{}, err
	}
	if in.CopyFromSource && in.TranslationOf == nil {
		return EntryDTO{}, apperrors.New("CONTENT_COPY_FROM_SOURCE_REQUIRES_TRANSLATION_OF",
			"copy_from_source requires translation_of", 400)
	}
	// A translation joins an existing group; resolving the source through the
	// tenant-scoped repo is also the check that it belongs to this tenant and
	// this content type (404 otherwise) — the caller cannot name a foreign id.
	// Resolved here, before the field-write and validation guards below, because
	// copy_from_source needs the source's working copy to build the effective
	// payload those guards must judge — the CALLER's payload is only half the
	// document once a copy is asked for.
	groupID := uuid.New()
	var src *domain.Entry
	if in.TranslationOf != nil {
		var gerr error
		src, gerr = s.repo.GetEntry(ctx, sub.TenantID, ct.ID, *in.TranslationOf)
		if gerr != nil {
			return EntryDTO{}, gerr
		}
		// A confined caller may only translate their OWN entry. Without this,
		// naming a colleague's id in translation_of both confirms it exists and
		// joins their group — which is a read of a row they were refused, dressed
		// up as a write of one they are allowed.
		if err := guardOwned(ct, sub, src); err != nil {
			return EntryDTO{}, err
		}
		groupID = src.TranslationGroupID
	}
	if in.CopyFromSource {
		merged, merr := mergeShallowJSON(src.Payload, payload)
		if merr != nil {
			return EntryDTO{}, merr
		}
		payload = merged
	}
	if err := guardWritableKeys(ct, sub, payload); err != nil {
		return EntryDTO{}, err
	}
	clean, mediaRefs, relationRefs, err := s.validateAndNormalize(ctx, sub.TenantID, sub, ct, payload)
	if err != nil {
		return EntryDTO{}, err
	}
	if err := guardEntryBytes(quota, clean); err != nil {
		return EntryDTO{}, err
	}
	now := time.Now().UTC()
	e := &domain.Entry{
		ID:            uuid.New(),
		TenantID:      sub.TenantID,
		ContentTypeID: ct.ID,
		Payload:       clean,
		// SearchText is computed here, not in the repository, because only the
		// service layer holds ct.Fields (the schema) — see domain.Entry's doc
		// comment on the field. PublishedSearchText stays empty: a fresh entry
		// is always a draft (below), so there is nothing published to index yet.
		SearchText:         domain.ExtractSearchText(ct.Fields, clean),
		Version:            1,
		Locale:             locale,
		TranslationGroupID: groupID,
		// New entries are always drafts. Publishing is a deliberate, separate
		// call (SetEntryStatus) — never a side effect of creation, or a public
		// delivery path would start serving content nobody chose to publish.
		Status:    domain.StatusDraft,
		CreatedAt: now,
		UpdatedAt: now,
	}
	e.CreatedBy, e.CreatedByKind, e.CreatedByAgent = provenanceOf(sub)
	// Every key of a create is a changed key: the document went from nothing to
	// this. Compared against an empty payload rather than listed from the map,
	// so the two paths that answer "what changed" cannot disagree.
	act.forEntry(e.ID).describe(ct.Fields, clean).changedKeys(domain.ChangedKeysBetween(nil, clean))
	// ONE transaction for the row, its asset links, and the idempotency record.
	//
	// The record is the reason this wrapper exists: committed without the entry it
	// names, a key points at nothing; committed after it, a crash in between
	// leaves an entry that no key claims, and the retry that follows creates the
	// duplicate the key was sent to prevent. Neither is expressible if they land
	// together. The asset links join the same unit because they were already
	// two separate commits away from the row they belong to.
	err = s.repo.WithTx(ctx, sub.TenantID, func(r repository.ContentRepository) error {
		if err := r.CreateEntry(ctx, e); err != nil {
			return err
		}
		if err := r.ReplaceEntryMedia(ctx, sub.TenantID, e.ID, mediaRefs); err != nil {
			return err
		}
		if err := r.ReplaceEntryRelations(ctx, sub.TenantID, e.ID, relationRefs); err != nil {
			return err
		}
		if idemKey == "" {
			return nil
		}
		return r.RecordEntryIdempotency(ctx, repository.EntryIdempotency{
			TenantID: sub.TenantID, ActorKey: actorKey, Key: idemKey,
			Fingerprint: fingerprint, EntryID: e.ID,
		})
	})
	if errors.Is(err, repository.ErrIdempotencyKeyTaken) {
		// Two first-tries of one key raced and this one lost. The transaction
		// rolled back, so the entry built above is gone — which is why the record
		// has to be written inside it rather than after. What the caller gets is
		// what a retry arriving a moment later would have got.
		replay, rerr := s.replayCreate(ctx, sub, ct, actorKey, idemKey, fingerprint)
		if rerr != nil {
			return EntryDTO{}, rerr
		}
		if replay == nil {
			// The winner's record is gone already. Not retried: a second attempt
			// can lose the same race again, and a create that loops until it wins
			// is a create that can hang.
			return EntryDTO{}, ErrIdempotencyRecordVanished
		}
		act.cancel()
		return *replay, nil
	}
	if err != nil {
		return EntryDTO{}, err
	}
	if quota.MaxEntriesPerTenant > 0 {
		if count, err := s.repo.CountEntriesForTenant(ctx, sub.TenantID); err == nil {
			softWarn(ctx, "entries", count, quota.MaxEntriesPerTenant, quota.SoftThresholdPct)
		}
	}
	// Creation is an editor action; a new entry is a draft with nothing published.
	return ProjectEntry(ct, e, sub), nil
}

// mergeShallowJSON overlays overlay's top-level keys onto base and returns the
// result. Used by copy_from_source: base is the translation source's working
// payload, overlay is what the caller sent, and the merged document then runs
// through the SAME validation an ordinary create does — this function does no
// validation of its own, and does not merge nested objects, only top-level
// keys, matching "body fields override" rather than a deep patch.
func mergeShallowJSON(base, overlay json.RawMessage) (json.RawMessage, error) {
	merged := map[string]json.RawMessage{}
	if len(base) > 0 {
		if err := json.Unmarshal(base, &merged); err != nil {
			return nil, apperrors.New("CONTENT_PAYLOAD_INVALID", "source payload must be a JSON object", 500)
		}
	}
	if len(overlay) > 0 {
		var over map[string]json.RawMessage
		if err := json.Unmarshal(overlay, &over); err != nil {
			return nil, apperrors.New("CONTENT_PAYLOAD_INVALID", "payload must be a JSON object", 400)
		}
		for k, v := range over {
			merged[k] = v
		}
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, apperrors.New("CONTENT_PAYLOAD_INVALID", "payload must be a JSON object", 400)
	}
	return out, nil
}

func (s *contentService) UpdateEntry(ctx context.Context, typeName string, id uuid.UUID, payload json.RawMessage, expectedVersion int) (_ EntryDTO, err error) {
	act := s.activityWrite(ctx, domain.ActivityEntryUpdate, typeName).forEntry(id)
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentUpdate, id.String(), typeName)
	if err != nil {
		return EntryDTO{}, err
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return EntryDTO{}, err
	}
	if err := guardTypeWrite(ct, sub); err != nil {
		return EntryDTO{}, err
	}
	// Confirm the entry exists in this tenant + type before writing (404 on miss).
	existing, err := s.repo.GetEntry(ctx, sub.TenantID, ct.ID, id)
	if err != nil {
		return EntryDTO{}, err
	}
	// Confinement, and BEFORE the version check: a 409 on a row a confined caller
	// may not touch would confirm both that the id exists and what version it is
	// on. The refusals have to be ordered so the least informative one wins.
	if err := guardOwned(ct, sub, existing); err != nil {
		return EntryDTO{}, err
	}
	// Optimistic concurrency: if the client declared the version it edited
	// (If-Match), reject when the stored version has already moved on.
	if expectedVersion > 0 && existing.Version != expectedVersion {
		return EntryDTO{}, repository.ErrVersionConflict
	}
	// Field permission, BEFORE the merge. After it, the patch and the stored
	// document are one indistinguishable object and the only honest refusal left
	// would name every restricted key on the type rather than the one the caller
	// actually sent. Silently dropping the key instead — the shape this
	// deliberately is not — would report success on a write that did not happen,
	// and could resurface as a REQUIRED error naming a field the caller is not
	// allowed to send.
	if err := guardWritableKeys(ct, sub, payload); err != nil {
		return EntryDTO{}, err
	}
	// PATCH is a partial update: overlay the incoming fields onto the stored
	// payload so fields the caller didn't send are preserved, not wiped.
	// The merge BASE is pruned, not the patch: a key the schema no longer defines
	// must not survive into the merged document, or a field deleted concurrently
	// with this write would reappear and brick the entry. The caller's own patch is
	// untouched, so a client typo still earns CONTENT_FIELD_UNKNOWN.
	// Hoisted rather than inlined twice: the activity record's "what changed"
	// compares against this exact base, and a second call that drifted from this
	// one would report keys the merge never touched.
	base := pruneUndefined(existing.Payload, ct.Fields)
	merged, err := mergeEntryPayload(base, payload)
	if err != nil {
		return EntryDTO{}, apperrors.Wrap("CONTENT_PAYLOAD_INVALID", "payload must be a JSON object", 400, err)
	}
	clean, mediaRefs, relationRefs, err := s.validateAndNormalize(ctx, sub.TenantID, sub, ct, merged)
	if err != nil {
		return EntryDTO{}, err
	}
	quota, err := s.resolveQuota(ctx, sub.TenantID)
	if err != nil {
		return EntryDTO{}, err
	}
	// Guard the MERGED result: successive PATCHes must not grow a stored entry
	// past the cap even though each request body was individually under limit.
	if err := guardEntryBytes(quota, clean); err != nil {
		return EntryDTO{}, err
	}
	// Compared BEFORE the assignment below overwrites the stored payload, and
	// against the PRUNED base rather than the raw one, so the answer matches
	// what the merge actually did: a key the schema no longer defines was
	// dropped by pruneUndefined, not by this caller, and naming it here would
	// attribute a schema change to whoever happened to save next.
	//
	// The label comes from the NEW payload: the stream is read later, and a
	// title taken from the pre-edit document would describe a version of the
	// entry that no longer exists anywhere.
	act.changedKeys(domain.ChangedKeysBetween(base, clean)).describe(ct.Fields, clean)
	existing.Payload = clean
	// Recomputed from the new working payload on every save — see domain.Entry's
	// doc comment on SearchText for why this has to happen here rather than in
	// the repository. PublishedSearchText is left as the repo loaded it:
	// SetEntryPublishState is the only place published_search_text moves, and it
	// does so in SQL from the row's own search_text at publish time, not here.
	existing.SearchText = domain.ExtractSearchText(ct.Fields, clean)
	existing.UpdatedAt = time.Now().UTC()
	recordUpdateProvenance(existing, sub)
	// Status and the published snapshot are deliberately untouched: saving is not
	// publishing. Editing a live entry leaves published_payload alone, so the
	// public keeps seeing the last released version until someone publishes
	// (ADR-006 — before that, this same absence meant edits went live on write,
	// because there was only one payload to read).
	// existing.Version is the version we read; the repo guards the write on it.
	//
	// Captured BEFORE the write, from the row GetEntry loaded at the top of this
	// method: existing.Payload was overwritten above (line ~1366) but
	// existing.HasUnpublishedChanges was not — UpdateEntry is what recomputes it,
	// via RETURNING, straight back onto this same struct (see
	// PostgresContentRepository.UpdateEntry). Reading it once before that call
	// and once after is what turns "is it pending" into "did it just BECOME
	// pending", which is the transition ADR-023's debounce rests on: every save
	// while the entry stays in the queue leaves wasPending == nowPending and
	// notifies nobody a second time.
	wasPending := existing.IsPublished() && existing.HasUnpublishedChanges
	// ADR-014 Amendment: review decisions. A reviewer's decision pins
	// EntryVersion to the version they sent back; while entries.version still
	// equals that pinned value, reviewDecisionNotSentBackExpr
	// (postgres_repository.go) has already hidden this entry from the queue —
	// so despite the naive check above reading true, it was NOT actually
	// visible to a reviewer the moment before this save. Left uncorrected,
	// wasPending and nowPending would both read true across the author's
	// fix-and-resave, and notifyEntryPendingReview — gated on the FALSE->TRUE
	// transition — would never re-fire for what is, from a reviewer's point of
	// view, the entry's first reappearance since being sent back.
	//
	// Gated on the naive result so an ordinary save (no decision in play) costs
	// no extra query; a lookup failure degrades to the naive answer rather than
	// failing the save, the same soft-fail decorateWithSchedule gives a read
	// that must not block on this table.
	if wasPending {
		if dec, derr := s.repo.GetLatestEntryReviewDecision(ctx, sub.TenantID, existing.ID); derr != nil {
			log.Printf("content update entry: get latest review decision %s: %v", existing.ID, derr)
		} else if dec != nil && dec.EntryVersion == existing.Version {
			wasPending = false
		}
	}
	if err := s.repo.UpdateEntry(ctx, existing); err != nil {
		return EntryDTO{}, err
	}
	if nowPending := existing.IsPublished() && existing.HasUnpublishedChanges; !wasPending && nowPending {
		s.notifyEntryPendingReview(ctx, sub, existing)
	}
	// Links follow the WORKING payload on every write. entry_media is therefore
	// the draft's reference set; the delivery gate reads entry_media_published
	// instead, so dropping an image here does not revoke bytes the live snapshot
	// still needs (see AssetIsPublished, ADR-006).
	if err := s.repo.ReplaceEntryMedia(ctx, sub.TenantID, existing.ID, mediaRefs); err != nil {
		return EntryDTO{}, err
	}
	if err := s.repo.ReplaceEntryRelations(ctx, sub.TenantID, existing.ID, relationRefs); err != nil {
		return EntryDTO{}, err
	}
	return ProjectEntry(ct, existing, sub), nil
}

// ListTranslations returns every locale of the entry's translation group,
// including the entry itself. Resolving through the tenant-scoped repo first is
// what stops a caller naming another tenant's id.
func (s *contentService) ListTranslations(ctx context.Context, typeName string, id uuid.UUID) (_ []EntryDTO, err error) {
	act := s.activityRead(ctx, domain.ActivityEntryTranslations, typeName).forEntry(id)
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentRead, id.String(), typeName)
	if err != nil {
		return nil, err
	}
	// Before the type lookup, not after: the refusal has to be reachable without
	// touching a row, or the timing of the answer becomes the oracle the 403 was
	// chosen to avoid being. A translation group is a SET of entries, so this is a
	// collection read even though the URL names one id.
	if err := guardPreviewCollection(sub); err != nil {
		return nil, err
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return nil, err
	}
	if err := guardTypeRead(ct, sub); err != nil {
		return nil, err
	}
	src, err := s.repo.GetEntry(ctx, sub.TenantID, ct.ID, id)
	if err != nil {
		return nil, err
	}
	if err := guardOwned(ct, sub, src); err != nil {
		return nil, err
	}
	f := repository.ListEntriesFilter{
		TenantID:           sub.TenantID,
		ContentTypeID:      ct.ID,
		TranslationGroupID: src.TranslationGroupID,
		Limit:              100,
		// Confinement applies to the SIBLINGS too, not just to the entry that was
		// named. A translation group is a set of rows, and a row a confined caller
		// does not own is one they may not read — reachable through this endpoint
		// or not. Leaving it off would make the group view a way to read exactly
		// the rows GetEntry refuses, which is the shape of bypass this endpoint has
		// already had to be closed against once (see the delivery branch below).
		//
		// The cost is that a translation authored by a colleague is missing from
		// the group, so it can look incomplete rather than restricted. That is the
		// same trade the whole confinement feature makes, and it is why own_only is
		// published on the type DTO: a client that knows it is confined can say so.
		CreatedBy: confinedAuthor(ct, sub),
	}
	// A delivery credential sees only published siblings — the same override as
	// ListEntries, applied here too so the group view is not a way around it.
	if sub.PublicDelivery {
		f.Status = domain.StatusPublished
		// The SOURCE entry has to clear the same bar as GetEntry, which 404s an
		// unpublished id precisely so that "exists but forbidden" is not
		// distinguishable from "does not exist". Resolving the group off an
		// unpublished row and answering 200 with its published siblings made
		// this endpoint the oracle GetEntry refuses to be: an unpublished-but-
		// present id answered 200 while a nonexistent one answered 404. It never
		// leaked draft CONTENT — the status filter above holds — but it did leak
		// that the row is still there, which is how you tell "retracted" from
		// "deleted" for an id you already legitimately hold.
		if !src.IsPublished() {
			return nil, apperrors.ErrNotFound
		}
		// Counted like every other delivery read. Without this, the group view
		// was a metered-read bypass: GetEntry, ListEntries and ResolveMediaURL
		// all record, so quota and billing simply under-counted this path.
		s.delivery.Record(sub.TenantID)
	}
	entries, _, err := s.repo.ListEntries(ctx, f)
	if err != nil {
		return nil, err
	}
	out := make([]EntryDTO, len(entries))
	for i, e := range entries {
		out[i] = ProjectEntry(ct, e, sub)
	}
	return out, nil
}

// SetEntryStatus moves an entry between editorial states. The two directions
// are authorized by DIFFERENT verbs (ADR-014 §1, step 6): releasing takes
// content:publish, retracting keeps content:update. One method, two answers —
// which is why the verb is chosen from the argument below rather than fixed at
// the call site, and why `status` is validated before anything else runs: an
// invalid value must not reach the point where it picks an authorization verb.
//
// This is the whole of the human-is-the-gate rule as the server sees it. An
// agent credential holds content:update and not content:publish, so it writes
// the working copy unattended and cannot release it; every other layer of that
// feature (the tool list, the review dialog, the button placement) is UX built
// on top of this line and can be bypassed without touching it.
func (s *contentService) SetEntryStatus(ctx context.Context, typeName string, id uuid.UUID, status string, expectedVersion int) (_ EntryDTO, err error) {
	// The recorder opens AFTER this check, not before, because until status is
	// valid there is no action to name: publish and unpublish are two lines in
	// the stream and a nonsense third value is neither. A malformed argument is
	// the caller's own 400 and belongs in the request log, not in a record of
	// what was done to the tenant's content.
	if !domain.ValidStatus(status) {
		return EntryDTO{}, apperrors.New("CONTENT_STATUS_INVALID", "status must be draft or published", 400).
			WithDetails(map[string]any{"status": status, "allowed": domain.AllowedStatuses()})
	}
	// Publish and unpublish are separate actions in the stream, and since step 6
	// separate verbs too — "who put this live" is a different question from "who
	// took it down", and now they are also different permissions.
	//
	// The stream action and the authorization verb are derived from the same
	// `status` in the same place on purpose: a record saying `entry.publish`
	// next to a check that asked for content:update would be a lie about what
	// was authorized, and the refusal record is the one an operator reads.
	act := s.activityWrite(ctx, domain.ActivityEntryUnpublish, typeName).forEntry(id)
	action := ActionContentUpdate
	if status == domain.StatusPublished {
		act.setAction(domain.ActivityEntryPublish)
		action = ActionContentPublish
	}
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, action, id.String(), typeName)
	if err != nil {
		return EntryDTO{}, err
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return EntryDTO{}, err
	}
	if err := guardTypeWrite(ct, sub); err != nil {
		return EntryDTO{}, err
	}
	existing, err := s.repo.GetEntry(ctx, sub.TenantID, ct.ID, id)
	if err != nil {
		return EntryDTO{}, err
	}
	// Publishing is a write, so confinement applies to it — and ahead of the
	// version check, for the reason UpdateEntry orders them the same way.
	if err := guardOwned(ct, sub, existing); err != nil {
		return EntryDTO{}, err
	}
	if expectedVersion > 0 && existing.Version != expectedVersion {
		return EntryDTO{}, repository.ErrVersionConflict
	}
	// Idempotency is about the SNAPSHOT, not just the status column. This used to
	// short-circuit on `existing.Status == status`, which was right while both
	// states read one payload — but under ADR-006 it would swallow the exact case
	// the feature exists for: edit a live entry, then press publish. Status is
	// already 'published', so the call would return success having released
	// nothing. A true no-op needs the snapshot to be current too — "current"
	// meaning the content matches, not merely that the version counter does.
	act.describe(ct.Fields, existing.Payload)
	if existing.Status == status && !existing.HasUnpublishedChanges {
		// Nothing was released and nothing was taken down; see cancel().
		act.cancel()
		return ProjectEntry(ct, existing, sub), nil
	}
	now := s.clock()
	publishedAt := existing.PublishedAt
	if status == domain.StatusPublished {
		// Keep the first-release timestamp across a re-publish, so published_at
		// means "when this went live", not "when it was last edited".
		if publishedAt == nil {
			publishedAt = &now
		}
	} else {
		// Unpublishing clears the timestamp; the DB CHECK enforces the same
		// invariant, so leaving it set would fail the write anyway.
		publishedAt = nil
	}
	existing.UpdatedAt = now
	recordUpdateProvenance(existing, sub)
	// Set unconditionally; the repository's CASE decides whether it lands. Since
	// ADR-014 §5.1 a retract KEEPS the row's existing publisher (the snapshot it
	// describes survives too), and the repository reads the surviving value back
	// into this struct — so passing the retracting actor here is harmless rather
	// than merely ignored. Note this is deliberately AFTER the no-op
	// short-circuit above: republishing identical content is not a new release,
	// so the original publisher stands.
	existing.PublishedBy = actor(sub)
	if err := s.repo.SetEntryPublishState(ctx, existing, status, publishedAt); err != nil {
		return EntryDTO{}, err
	}
	return ProjectEntry(ct, existing, sub), nil
}

// mergeEntryPayload overlays a partial PATCH payload onto the stored payload
// (shallow, top-level fields — content entries are flat field maps). Both are
// JSON objects; the handler already enforces object payloads.
func mergeEntryPayload(existing, patch json.RawMessage) (json.RawMessage, error) {
	base := map[string]json.RawMessage{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &base); err != nil {
			return nil, err
		}
	}
	over := map[string]json.RawMessage{}
	if err := json.Unmarshal(patch, &over); err != nil {
		return nil, err
	}
	for k, v := range over {
		base[k] = v
	}
	return json.Marshal(base)
}

func (s *contentService) GetEntry(ctx context.Context, typeName string, id uuid.UUID, in GetEntryInput) (_ EntryDTO, err error) {
	act := s.activityRead(ctx, domain.ActivityEntryRead, typeName).forEntry(id)
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentRead, id.String(), typeName)
	if err != nil {
		return EntryDTO{}, err
	}
	// Ahead of the type lookup, for the reason ListEntries puts
	// guardPreviewCollection there: the one audience that may not use this
	// parameter at all has nothing to negotiate, and a refusal that first costs
	// a query is a refusal whose timing says whether the type exists.
	aud := audienceFor(sub)
	if err := guardPopulateAudience(aud, in.Populate); err != nil {
		return EntryDTO{}, err
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return EntryDTO{}, err
	}
	if err := guardTypeRead(ct, sub); err != nil {
		return EntryDTO{}, err
	}
	// BEFORE the entry is fetched, mirroring parseProjection's placement in
	// ListEntries: a malformed ?populate= costs one round trip rather than a
	// read plus a refusal. It discloses nothing about the id, because the answer
	// is the same 400 whether or not that entry exists.
	specs, err := s.parsePopulate(ctx, ct, sub, in.Populate)
	if err != nil {
		return EntryDTO{}, err
	}
	e, err := s.repo.GetEntry(ctx, sub.TenantID, ct.ID, id)
	if err != nil {
		return EntryDTO{}, err
	}
	// Confinement: a row this caller does not own answers 404, exactly as an
	// unpublished row does for a delivery credential below. Same reasoning, one
	// level in: the alternative distinguishes "your colleague's draft" from "no
	// such id", and that difference is enough to enumerate the collection.
	if err := guardOwned(ct, sub, e); err != nil {
		return EntryDTO{}, err
	}
	// A delivery credential fetching a draft by id gets 404, not 403 — a
	// distinguishable "exists but forbidden" would turn this endpoint into an
	// oracle for unpublished ids.
	if sub.PublicDelivery {
		// A preview credential names ONE entry, and this is the only place that
		// name is ever compared against the row being served. Without the compare
		// the credential is not narrowed at all: audienceFor answers audiencePreview
		// off the mere PRESENCE of the id, so a token minted for one draft would
		// serve the working copy of every other entry in the tenant too.
		//
		// The id match REPLACES the published gate rather than joining it — serving
		// the unpublished working copy is what this credential is for, and the match
		// is the narrower of the two conditions, not the weaker one. 404 on a
		// mismatch for the same reason the branch below 404s: a preview token is the
		// one delivery credential that legitimately leaves the platform's edge, so
		// it is the one most likely to be pointed at ids it was not minted for.
		if sub.PreviewEntryID != nil {
			if e.ID != *sub.PreviewEntryID {
				return EntryDTO{}, apperrors.ErrNotFound
			}
			s.delivery.Record(sub.TenantID)
			return ProjectEntry(ct, e, sub), nil
		}
		if !e.IsPublished() {
			return EntryDTO{}, apperrors.ErrNotFound
		}
		s.delivery.Record(sub.TenantID)
		// A page of one, through the same batching path the list uses. Sharing
		// it rather than writing a single-entry expansion is what keeps the two
		// endpoints answering the same shape — a second implementation is where
		// "the list nulls a retracted target but the single GET 404s" comes
		// from.
		rel, err := s.populateFor(ctx, specs, []*domain.Entry{e}, sub)
		if err != nil {
			return EntryDTO{}, err
		}
		return ProjectEntry(ct, e, sub).withRelated(rel[e.ID]), nil
	}
	// Admin audience only, and it costs one indexed lookup on a read that
	// already did two. An editor looking at an entry has to be able to see that
	// it is going live on Monday — without it, the only way to find out is to
	// call a second endpoint you have to know exists, and "I didn't know it was
	// scheduled" is precisely how a scheduled publish surprises someone.
	//
	// withSchedule refuses any audience but admin on its own, so the ordering
	// here is not what keeps it off a delivery response; see it and MarshalJSON.
	//
	// The admin expansion goes through the SAME page-of-one call the delivery
	// branch above makes (ADR-006 Amendment 7). Two spellings of "expand this
	// entry's relations" is how a single GET and a list of one start disagreeing
	// about what a deleted target renders as.
	rel, err := s.populateFor(ctx, specs, []*domain.Entry{e}, sub)
	if err != nil {
		return EntryDTO{}, err
	}
	dto := s.decorateWithSchedule(ctx, sub, ProjectEntry(ct, e, sub).withRelated(rel[e.ID]), e.ID)
	return s.decorateWithReviewDecision(ctx, sub, dto, e.ID), nil
}

// EntryReferencedBy answers "what points at this entry" (ADR-024, 2.8a) — the
// reverse of a relation field, read from entry_relations /
// entry_relations_published rather than scanned from every other entry's
// payload. Authorized and existence-checked exactly like GetEntry (ActionRead,
// same guardTypeRead/guardOwned pair): a caller who could not read this entry
// directly must not learn who references it either, since the reference list
// alone would confirm the id exists and describe rows the direct GET would
// have refused.
func (s *contentService) EntryReferencedBy(ctx context.Context, typeName string, id uuid.UUID, limit, offset int) (ReferencedByDTO, error) {
	sub, err := s.authorize(ctx, ActionContentRead, id.String(), typeName)
	if err != nil {
		return ReferencedByDTO{}, err
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return ReferencedByDTO{}, err
	}
	if err := guardTypeRead(ct, sub); err != nil {
		return ReferencedByDTO{}, err
	}
	e, err := s.repo.GetEntry(ctx, sub.TenantID, ct.ID, id)
	if err != nil {
		return ReferencedByDTO{}, err
	}
	if err := guardOwned(ct, sub, e); err != nil {
		return ReferencedByDTO{}, err
	}
	limit, offset = clampReferencedByPage(limit, offset)
	res, err := s.repo.EntryReferencedBy(ctx, sub.TenantID, id, limit, offset)
	if err != nil {
		return ReferencedByDTO{}, err
	}
	typesByName, err := s.contentTypesByName(ctx, sub.TenantID)
	if err != nil {
		return ReferencedByDTO{}, err
	}
	return projectReferencedBy(res, sub, typesByName), nil
}

// contentTypesByName is the tenant's whole type roster keyed by name, fetched
// ONCE per reverse-reference request and handed to readableReferencedByRows
// so it can role-check every row without a query per row. A referenced-by
// page's rows can span any number of types (a referrer is never necessarily
// the type the caller asked about), so per-row lookups would mean one
// GetContentTypeByName call per DISTINCT type on the page; ListContentTypes
// is already the single query ListContentTypes' own handler makes for the
// same tenant, reused here instead of duplicated.
func (s *contentService) contentTypesByName(ctx context.Context, tenantID string) (map[string]*domain.ContentType, error) {
	cts, err := s.repo.ListContentTypes(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*domain.ContentType, len(cts))
	for _, ct := range cts {
		out[ct.Name] = ct
	}
	return out, nil
}

func (s *contentService) DeleteEntry(ctx context.Context, typeName string, id uuid.UUID) (err error) {
	act := s.activityWrite(ctx, domain.ActivityEntryDelete, typeName).forEntry(id)
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentDelete, id.String(), typeName)
	if err != nil {
		return err
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return err
	}
	if err := guardTypeWrite(ct, sub); err != nil {
		return err
	}
	// Confinement costs this path a READ it did not previously do, and only for a
	// confined caller. There is no way around it: ownership is a property of the
	// row, and a DELETE that carried the predicate in its WHERE would report
	// success on a row it did not match — the "affected 0" that reads as done.
	// The read is now unconditional, where it used to happen only for a confined
	// caller. Delete is the one action whose activity line CANNOT be enriched
	// afterwards — the row it names will not exist to be looked up — so a stream
	// entry saying "deleted 6f2a…" is permanently unreadable. One read per
	// delete buys the label; deletes are rare, and the alternative is a record
	// nobody can use for the action that most needs one.
	e, err := s.repo.GetEntry(ctx, sub.TenantID, ct.ID, id)
	if err != nil {
		return err
	}
	act.describe(ct.Fields, e.Payload)
	if err := guardOwned(ct, sub, e); err != nil {
		return err
	}
	return s.repo.DeleteEntry(ctx, sub.TenantID, ct.ID, id)
}

// Usage resolves the tenant's plan and live counts (TKT-R4b D8). Read action —
// any tenant member can see their own usage; counts are real-time (same source
// as the hard check, so displayed usage never disagrees with enforcement).
func (s *contentService) Usage(ctx context.Context) (UsageDTO, error) {
	sub, err := s.authorize(ctx, ActionContentList, "collection", "")
	if err != nil {
		return UsageDTO{}, err
	}
	// A delivery credential is refused outright, the same shape as ExportSchema
	// and for a sharpened version of the same reason. Nothing about this response
	// is content: it is the tenant's PLAN, their quota ceilings and how much of
	// them is used — commercial facts about the customer, answered to a public
	// read credential. The edge has never called this endpoint, so nothing
	// legitimate loses access.
	//
	// It was reachable before preview existed and was not, then, exposed to
	// anyone: the only holder of a delivery credential was the platform's own
	// edge (ADR-006's premise). A preview link is the first delivery credential
	// that leaves that edge by design, which is what turned a latent reach into
	// a real one — the recalculation ADR-006's last trigger condition asks for.
	// william 0802.
	if sub.PublicDelivery {
		return UsageDTO{}, apperrors.New("CONTENT_USAGE_FORBIDDEN", "usage is an account operation", 403)
	}
	quota, err := s.resolveQuota(ctx, sub.TenantID)
	if err != nil {
		return UsageDTO{}, err
	}
	types, err := s.repo.CountContentTypes(ctx, sub.TenantID)
	if err != nil {
		return UsageDTO{}, err
	}
	entries, err := s.repo.CountEntriesForTenant(ctx, sub.TenantID)
	if err != nil {
		return UsageDTO{}, err
	}
	// Today's public read volume. Best-effort: a failure here must not take
	// down the whole usage view, which is primarily about stored counts.
	reads, err := s.repo.DeliveryReadsForDay(ctx, sub.TenantID, time.Now().UTC())
	if err != nil {
		reads = 0
	}
	return UsageDTO{
		Plan:               quota.PlanName,
		SoftThresholdPct:   quota.SoftThresholdPct,
		Types:              dimension(types, quota.MaxTypesPerTenant),
		Entries:            dimension(entries, quota.MaxEntriesPerTenant),
		DeliveryReadsToday: reads,
	}, nil
}

func (s *contentService) ListEntries(ctx context.Context, typeName string, in ListEntriesInput) (_ *EntryListResult, err error) {
	act := s.activityRead(ctx, domain.ActivityEntryList, typeName)
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentList, "collection", typeName)
	if err != nil {
		return nil, err
	}
	// Ahead of the type lookup for the same reason as ListTranslations: a uniform
	// refusal that costs a query is not uniform in the only channel that matters.
	if err := guardPreviewCollection(sub); err != nil {
		return nil, err
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return nil, err
	}
	// The collection gate, before any parameter is even parsed: a caller who may
	// not read this type has nothing to negotiate about paging.
	if err := guardTypeRead(ct, sub); err != nil {
		return nil, err
	}

	// The audience drives paging mode and the refusals below, so it is derived
	// once from the credential rather than re-decided per branch.
	aud := audienceFor(sub)
	// Only preview is refused now (ADR-006 Amendment 7 granted admin the
	// parameter), but the refusal stays here beside the offset and cursor ones
	// and for the identical reason: a parameter an audience may not use is
	// answered, never dropped. A caller that silently got no `related` would
	// conclude the relations are empty.
	if err := guardPopulateAudience(aud, in.Populate); err != nil {
		return nil, err
	}
	// A delivery credential may filter, and its filters read the SNAPSHOT
	// (ADR-006 Amendment 4). Until that amendment every clause was built
	// against `payload` — the WORKING copy — so a public caller's predicate
	// would probe never-published text: whether a row comes back is itself an
	// oracle on the draft, even though the rows' Data is the published copy.
	// The fix is not to filter in Go afterwards but to bind the predicate to
	// published_payload inside the query (f.MatchPublished below), the same
	// place the status override lives and for the same reason: the edge is not
	// the layer this invariant belongs to, because a delivery JWT authenticates
	// against the Domain API directly (OD2-023 F1).
	//
	// Sort is granted to this audience too (ADR-006 Amendment 5), under the same
	// rule as the filter: the ORDER BY reads the SNAPSHOT, because ranking the
	// snapshot's rows by the working copy's values would hand the edit order
	// back out — the position of a row in a page is as much an observation as
	// its content. The cursor now carries the sort key and value, so the keyset
	// predicate can mirror an arbitrary ORDER BY; a cursor minted under a
	// DIFFERENT sort is refused rather than translated (see decodeCursor).
	if aud == audienceDelivery {
		// Delivery pages by cursor, so offset is not merely ignored — it is
		// refused. Silently dropping it would let a caller believe it had paged
		// when it had re-read page 1, and this audience has no `total` to notice
		// the discrepancy against.
		if in.Offset != 0 {
			return nil, apperrors.New("CONTENT_OFFSET_FORBIDDEN", "delivery credentials page by cursor; use next_cursor instead of offset", 403).
				WithDetails(map[string]any{"offset": in.Offset})
		}
	} else if strings.TrimSpace(in.Cursor) != "" {
		// The admin audience keeps offset paging; accepting a cursor there and
		// ignoring it would be the same silent lie in the other direction.
		return nil, apperrors.New("CONTENT_CURSOR_UNSUPPORTED", "cursor paging is delivery-only; the admin API pages by offset", 400).
			WithDetails(map[string]any{"cursor": in.Cursor})
	}

	filters := make([]repository.FieldFilter, 0, len(in.Filters))
	for _, raw := range in.Filters {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		flt, err := parseFilter(ct, sub, raw)
		if err != nil {
			return nil, err
		}
		filters = append(filters, flt)
	}
	sort, err := parseSort(ct, sub, in.Sort)
	if err != nil {
		return nil, err
	}
	// AFTER parseSort, and that order is load-bearing: the cursor is only
	// meaningful relative to an order, so it cannot be validated until the sort
	// it claims to belong to has been resolved. It also means a caller who got
	// BOTH wrong is told about the sort first — the cursor complaint would
	// otherwise name a sort the type does not even define.
	var after *repository.EntryCursor
	if aud == audienceDelivery {
		if after, err = decodeCursor(in.Cursor, sort); err != nil {
			return nil, err
		}
	}
	projection, err := parseProjection(ct, sub, in.Fields)
	if err != nil {
		return nil, err
	}
	// Same placement and same reasoning as parseProjection above: every key is
	// resolved, permission-checked and its target type loaded before a single
	// row is read, so a bad key costs one round trip rather than a page.
	specs, err := s.parsePopulate(ctx, ct, sub, in.Populate)
	if err != nil {
		return nil, err
	}

	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	// Reject an unknown status rather than silently listing everything — a
	// caller that asked for "publised" must not get drafts back.
	if in.Status != "" && !domain.ValidStatus(in.Status) {
		return nil, apperrors.New("CONTENT_STATUS_INVALID", "status must be draft or published", 400).
			WithDetails(map[string]any{"status": in.Status, "allowed": domain.AllowedStatuses()})
	}
	if in.Locale != "" && !domain.ValidLocale(in.Locale) {
		return nil, apperrors.New("CONTENT_LOCALE_INVALID", "locale must be a valid language tag", 400).
			WithDetails(map[string]any{"locale": in.Locale})
	}
	// The cap is on the RAW string, before it is split into terms: a caller
	// padding one request with hundreds of terms pays for it in query length,
	// not in a term count this check would otherwise have to enforce
	// separately. 200 is generous for a person typing and stingy for anyone
	// trying to turn `q` into a way to smuggle an unbounded WHERE clause onto
	// the query the repository builds.
	const maxSearchQueryLen = 200
	if len(in.Query) > maxSearchQueryLen {
		return nil, apperrors.New("CONTENT_SEARCH_QUERY_TOO_LONG", "q must be at most 200 characters", 422).
			WithDetails(map[string]any{"limit": maxSearchQueryLen, "length": len(in.Query)})
	}
	// Delivery credentials see published content only. Unlike offset — the one
	// parameter this audience is simply refused — status has an answer it CAN
	// be given, so the rule is
	// not a blanket refusal: asking for `published` is honoured, asking for
	// anything else is refused rather than quietly turned into `published`.
	//
	// Silently rewriting `status=draft` into `status=published` was the old
	// behaviour, and it is the same class of lie as silently dropping `offset` —
	// the caller is told nothing and reasonably concludes the drafts it asked
	// for do not exist, when in fact it was never allowed to ask.
	if aud == audienceDelivery {
		if in.Status != "" && in.Status != domain.StatusPublished {
			return nil, apperrors.New("CONTENT_STATUS_FORBIDDEN", "delivery credentials read published entries only", 403).
				WithDetails(map[string]any{"status": in.Status})
		}
		in.Status = domain.StatusPublished
		s.delivery.Record(sub.TenantID)
	}
	f := repository.ListEntriesFilter{
		TenantID:      sub.TenantID,
		ContentTypeID: ct.ID,
		Filters:       filters,
		Sort:          sort,
		Status:        in.Status,
		Locale:        in.Locale,
		Limit:         limit,
		Offset:        in.Offset,
		Query:         in.Query,
		// Confinement, as a predicate INSIDE the query rather than a pass over the
		// page afterwards. The COUNT(*) shares this WHERE, so `total` counts what
		// the caller may see; filtering in Go would leave total describing the
		// whole collection, and the difference between it and the rows returned is
		// exactly the number of entries the caller was refused. Paging would break
		// too — every page arrives short, and a full page of other people's rows
		// reads as the end of the collection.
		CreatedBy: confinedAuthor(ct, sub),
	}
	if aud == audienceDelivery {
		f.CursorPaged = true
		f.Offset = 0
		f.After = after
		// Predicates read the snapshot, never the working copy. Status was
		// forced to published above; the repository refuses this flag without
		// it, so the two cannot drift apart silently.
		f.MatchPublished = true
		// One row past the page: the cheap way to answer has_more without a
		// second query, and without COUNT(*) — which is the cost this mode
		// exists to avoid. The extra row is trimmed before it reaches the caller.
		f.Limit = limit + 1
	}
	entries, total, err := s.repo.ListEntries(ctx, f)
	if err != nil {
		return nil, err
	}

	if aud == audienceDelivery {
		hasMore := len(entries) > limit
		if hasMore {
			entries = entries[:limit]
		}
		// AFTER the extra row is trimmed. Expanding the page-probe row too would
		// spend the id budget — and a query — on an entry that is never
		// rendered, and could refuse a page with CONTENT_POPULATE_TOO_MANY over
		// relations nobody asked to see.
		related, err := s.populateFor(ctx, specs, entries, sub)
		if err != nil {
			return nil, err
		}
		items := make([]EntryDTO, len(entries))
		for i, e := range entries {
			items[i] = ProjectEntry(ct, e, sub).narrowedTo(projection).withRelated(related[e.ID])
		}
		// Admin-only field, so this is a no-op on the delivery/preview page this
		// branch renders — decorateEntriesWithSchedule bails on PublicDelivery
		// before it queries. Called uniformly with the admin page below so the
		// two branches build EntryDTO through the same steps, not two hand-kept
		// spellings of "expand this page".
		items = s.decorateEntriesWithSchedule(ctx, sub, items, entries)
		items = s.decorateEntriesWithReviewDecision(ctx, sub, items, entries)
		res := &EntryListResult{Items: items, Limit: limit, HasMore: &hasMore}
		// A next cursor is only meaningful when there IS a next page. Emitting
		// one on the last page invites a consumer to loop forever on an
		// always-empty final request.
		if hasMore && len(entries) > 0 {
			// f.MatchPublished, not the audience constant, so the payload the
			// cursor's sort value is read from is by construction the one the
			// repository's ORDER BY read.
			res.NextCursor = encodeCursor(entries[len(entries)-1], sort, f.MatchPublished)
		}
		return res, nil
	}

	// The admin page, expanded in its own view (ADR-006 Amendment 7): one query
	// for the whole page, exactly as above, and no `limit+1` probe row to keep
	// out of the id budget because offset paging does not fetch one.
	related, err := s.populateFor(ctx, specs, entries, sub)
	if err != nil {
		return nil, err
	}
	items := make([]EntryDTO, len(entries))
	for i, e := range entries {
		items[i] = ProjectEntry(ct, e, sub).narrowedTo(projection).withRelated(related[e.ID])
	}
	// The admin console's list view reads this field for its Scheduled badge
	// (same as the single-GET path at decorateWithSchedule above) — without it
	// the badge never renders, since the list was the only read path that never
	// called withSchedule. One batched query for the whole page, not one per
	// row; see decorateEntriesWithSchedule.
	items = s.decorateEntriesWithSchedule(ctx, sub, items, entries)
	items = s.decorateEntriesWithReviewDecision(ctx, sub, items, entries)
	return &EntryListResult{Items: items, Total: &total, Limit: limit, Offset: &in.Offset}, nil
}

// validateAndNormalize unmarshals the raw payload, validates it against the
// type's fields, checks relation targets exist in the same tenant, and returns
// the canonical bytes to store.
// validateAndNormalize returns the canonical payload plus the media assets it
// references, so the caller can keep entry_media in step with the payload in the
// same write. Returning the ids (rather than re-parsing later) means the links
// can never disagree with what was validated.
func (s *contentService) validateAndNormalize(ctx context.Context, tenantID string, sub authn.Subject, ct *domain.ContentType, payload json.RawMessage) (json.RawMessage, []uuid.UUID, []repository.EntryRelationRef, error) {
	var data map[string]any
	if len(payload) == 0 {
		data = map[string]any{}
	} else if err := json.Unmarshal(payload, &data); err != nil {
		return nil, nil, nil, apperrors.Wrap("CONTENT_PAYLOAD_INVALID", "payload must be a JSON object", 400, err)
	}
	if err := validatePayload(ct.Fields, data); err != nil {
		return nil, nil, nil, err
	}
	relationRefs, err := s.checkRelations(ctx, tenantID, sub, ct, data)
	if err != nil {
		return nil, nil, nil, err
	}
	mediaRefs, err := s.collectMediaRefs(ctx, tenantID, ct, data)
	if err != nil {
		return nil, nil, nil, err
	}
	out, err := json.Marshal(data)
	if err != nil {
		return nil, nil, nil, err
	}
	return out, mediaRefs, relationRefs, nil
}

// checkRelations verifies every present relation value points at a live entry of
// the referenced type within the same tenant. The PoC stores the related UUID
// as-is (no nested resolve).
// collectMediaRefs resolves every `file` field to a media asset in the same
// tenant and returns the referenced ids. An entry may only point at an asset
// whose bytes actually landed — a reservation is not a file.
func (s *contentService) collectMediaRefs(ctx context.Context, tenantID string, ct *domain.ContentType, data map[string]any) ([]uuid.UUID, error) {
	return s.mediaRefsIn(ctx, tenantID, ct.Fields, data)
}

// mediaRefsIn is collectMediaRefs over one field list and one object. It
// calls itself for the items of a component field (ADR-020 §4): a file or
// richtext sub-field is the same reference held one level down, checked
// against the same tenant and linked through the same entry_media rows.
func (s *contentService) mediaRefsIn(ctx context.Context, tenantID string, fields []domain.Field, data map[string]any) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	for _, f := range fields {
		switch f.Type {
		case domain.FieldTypeComponent:
			for i, item := range componentItems(f, data[f.Key]) {
				subFields, subItem := prefixed(f.Key, f.ComponentFields, item)
				sub, err := s.mediaRefsIn(ctx, tenantID, subFields, subItem)
				if err != nil {
					return nil, withItem(err, f, i)
				}
				ids = append(ids, sub...)
			}
		case domain.FieldTypeDynamicZone:
			// Every item, against the sub-fields of the component IT names —
			// which is the only difference from the component arm above. A zone
			// is a place entries hold media, so it must be enumerated here or
			// the entry_media rows would be missing links the payload has, and
			// the relink pass (mediaPaths) would then delete them as orphans.
			for i, item := range zoneItems(data[f.Key]) {
				subFields, subItem := prefixed(f.Key, f.ZoneFields[zoneItemComponent(item)], zoneItemBody(item))
				sub, err := s.mediaRefsIn(ctx, tenantID, subFields, subItem)
				if err != nil {
					return nil, withItem(err, f, i)
				}
				ids = append(ids, sub...)
			}
		case domain.FieldTypeFile:
			v, ok := data[f.Key]
			if !ok || v == nil {
				continue
			}
			raw, _ := v.(string) // type already checked by validatePayload
			if raw == "" {
				continue
			}
			id, err := uuid.Parse(raw)
			if err != nil {
				return nil, apperrors.New("CONTENT_MEDIA_REF_INVALID", "file field must be a media asset id", 422).
					WithDetails(map[string]any{"field": f.Key, "value": raw})
			}
			if err := s.requireUploadedAsset(ctx, tenantID, f.Key, id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		case domain.FieldTypeRichText:
			// Image blocks are media references exactly as a `file` field is —
			// checked against the same tenant, held to the same "bytes actually
			// landed" bar, and linked through entry_media so the payload and the
			// link table cannot disagree. ValidateRichText already ran (shape and
			// uuid syntax), and CollectRichTextMediaIDs dedupes, so each ASSET
			// costs one lookup no matter how many blocks name it.
			v, ok := data[f.Key]
			if !ok || v == nil {
				continue
			}
			for _, id := range domain.CollectRichTextMediaIDs(v) {
				if err := s.requireUploadedAsset(ctx, tenantID, f.Key, id); err != nil {
					return nil, err
				}
				ids = append(ids, id)
			}
		}
	}
	return ids, nil
}

// requireUploadedAsset resolves one media reference within the tenant and
// insists its bytes actually landed — a reservation is not a file.
// Tenant-scoped lookup: naming another tenant's asset resolves to 404, so the
// reference cannot cross the isolation boundary.
func (s *contentService) requireUploadedAsset(ctx context.Context, tenantID, fieldKey string, id uuid.UUID) error {
	a, err := s.repo.GetMediaAsset(ctx, tenantID, id)
	if err != nil {
		return apperrors.New("CONTENT_MEDIA_NOT_FOUND", "referenced media asset not found", 422).
			WithDetails(map[string]any{"field": fieldKey, "asset_id": id.String()})
	}
	if !a.IsUploaded() {
		return apperrors.New("CONTENT_MEDIA_NOT_UPLOADED", "referenced media asset has no uploaded bytes", 422).
			WithDetails(map[string]any{"field": fieldKey, "asset_id": id.String()})
	}
	return nil
}

// checkRelations resolves every relation value to a live entry in the same
// tenant.
//
// It takes the SUBJECT because a relation is a read: EntryExists answers "is
// there a row with this id in that type", and pointed at a collection the caller
// may not read it is an existence oracle around guardTypeRead. Refusing the
// relation TYPE closes it.
//
// Confinement deliberately does NOT apply here. Referring to an entry is not
// reading it — no payload crosses — and requiring ownership would stop a confined
// editor from tagging their article with a category somebody else created, which
// is the normal use of a relation. What remains is that an editor can confirm a
// specific uuid exists among a colleague's rows; that costs them the whole id
// first, and closing it would cost the feature.
func (s *contentService) checkRelations(ctx context.Context, tenantID string, sub authn.Subject, ct *domain.ContentType, data map[string]any) ([]repository.EntryRelationRef, error) {
	// One resolution per related type, not one per value. Two relation fields
	// pointing at the same type — the ordinary shape, `author` and `reviewer`
	// both naming `person` — used to load that type (and all its fields) twice,
	// and a multi-valued field would have loaded it once per element.
	types := map[string]*domain.ContentType{}
	resolve := func(f domain.Field) (*domain.ContentType, error) {
		if t, ok := types[f.RelationEntity]; ok {
			return t, nil
		}
		t, err := s.repo.GetContentTypeByName(ctx, tenantID, f.RelationEntity)
		if err != nil {
			return nil, apperrors.New("CONTENT_RELATION_TYPE_UNKNOWN", "related content type not found", 422).
				WithDetails(map[string]any{"field": f.Key, "relation_entity": f.RelationEntity})
		}
		types[f.RelationEntity] = t
		return t, nil
	}
	var refs []repository.EntryRelationRef
	if err := s.checkRelationsIn(ctx, tenantID, sub, ct.Fields, data, resolve, &refs); err != nil {
		return nil, err
	}
	return refs, nil
}

// checkRelationsIn is checkRelations over one field list and one object,
// recursing into component items (ADR-020 §4) with the resolver shared, so a
// related type is still loaded once per write however many items name it.
//
// out accumulates one (fieldPath, targetID) pair per value this pass confirms
// exists — the same set ReplaceEntryRelations (ADR-024) then writes as
// entry_relations rows, so the write-time index can never name a target this
// check refused. fieldPath is prefixed exactly the way mediaRefsIn/prefixed
// already prefix a component or zone sub-field's key, so a row this loop
// appends and a row the field's own top-level check appends are distinguishable
// by field_path alone.
func (s *contentService) checkRelationsIn(ctx context.Context, tenantID string, sub authn.Subject, fields []domain.Field, data map[string]any, resolve func(domain.Field) (*domain.ContentType, error), out *[]repository.EntryRelationRef) error {
	for _, f := range fields {
		if f.Type == domain.FieldTypeComponent {
			for i, item := range componentItems(f, data[f.Key]) {
				subFields, subItem := prefixed(f.Key, f.ComponentFields, item)
				if err := s.checkRelationsIn(ctx, tenantID, sub, subFields, subItem, resolve, out); err != nil {
					return withItem(err, f, i)
				}
			}
			continue
		}
		if f.Type == domain.FieldTypeDynamicZone {
			for i, item := range zoneItems(data[f.Key]) {
				subFields, subItem := prefixed(f.Key, f.ZoneFields[zoneItemComponent(item)], zoneItemBody(item))
				if err := s.checkRelationsIn(ctx, tenantID, sub, subFields, subItem, resolve, out); err != nil {
					return withItem(err, f, i)
				}
			}
			continue
		}
		if f.Type != domain.FieldTypeRelation {
			continue
		}
		v, ok := data[f.Key]
		if !ok || v == nil {
			continue
		}
		// Dispatch on CARDINALITY, then check every element by the same rule —
		// the same split validateValue makes, for the same reason. Reading the
		// value as a bare string was correct only for the scalar shape: a
		// multi-valued relation arrived as []any, the assertion yielded "", and
		// the write was refused as an invalid UUID. `multiple` is legal on
		// relation (domain.AllowedMultipleTypes), so that made a declarable
		// field permanently unwritable rather than merely unchecked.
		raws, err := relationValues(f, v)
		if err != nil {
			return err
		}
		// Parsing precedes resolving the related type, and that order is load-
		// bearing: a malformed uuid against a relation_entity that no longer
		// exists answered CONTENT_RELATION_INVALID before this function grew a
		// second cardinality, and it still does. Resolving first would have
		// quietly promoted the type error over the value error.
		ids := make([]uuid.UUID, len(raws))
		for i, raw := range raws {
			id, err := uuid.Parse(raw)
			if err != nil {
				return indexedIfMulti(errRelationInvalid(f.Key, raw), f, i)
			}
			ids[i] = id
		}
		relType, err := resolve(f)
		if err != nil {
			return err
		}
		if err := guardTypeRead(relType, sub); err != nil {
			return err
		}
		for i, id := range ids {
			exists, err := s.repo.EntryExists(ctx, tenantID, relType.ID, id)
			if err != nil {
				return err
			}
			if !exists {
				return indexedIfMulti(errRelationNotFound(f.Key, raws[i]), f, i)
			}
			*out = append(*out, repository.EntryRelationRef{FieldPath: f.Key, TargetID: id})
		}
	}
	return nil
}

// relationValues flattens a relation field's value to the ids it names: one for
// a scalar field, len(array) for a multi-valued one. The elements are already
// known to be strings — validatePayload ran validateScalar over each of them
// before this point — so a non-string here is a broken invariant, not a caller
// error, and it is reported as the type mismatch it is rather than silently
// skipped.
func relationValues(f domain.Field, v any) ([]string, error) {
	if !f.Multiple {
		raw, ok := v.(string)
		if !ok {
			return nil, errFieldTypeMismatch(f.Key, f.Type)
		}
		return []string{raw}, nil
	}
	xs, ok := v.([]any)
	if !ok {
		return nil, errFieldTypeMismatch(f.Key, f.Type)
	}
	out := make([]string, 0, len(xs))
	for i, x := range xs {
		raw, ok := x.(string)
		if !ok {
			return nil, withIndex(errFieldTypeMismatch(f.Key, f.Type), i)
		}
		out = append(out, raw)
	}
	return out, nil
}

// indexedIfMulti attaches the element index only where one exists. A scalar
// field carrying `"index": 0` would read as "the first of several" for a field
// that can only ever hold one.
func indexedIfMulti(err error, f domain.Field, i int) error {
	if !f.Multiple {
		return err
	}
	return withIndex(err, i)
}

// supportedOps is the ONE answer to "which filter operators does this field
// accept", and both surfaces that publish it read it from here: FieldDTO.
// Supported on GET /types, and the `supported` detail on the 400 a bad filter
// gets back (ADR-013 §6).
//
// It is not repository.OpsForCardinality on its own, which is what §6 assumed
// when it called this a cheap wrapper over a pure function. Cardinality is only
// half the rule — rich text refuses EVERY operator regardless of it (see
// parseFilter below for why), so a DTO built on cardinality alone would have
// advertised eight operators on a field the service answers 400 for. Two
// half-rules in two places is how a published list starts lying.
//
// Never nil: a JSON `null` here would mean "this server does not know", and the
// caller that has to tell that from "no operator is legal" will guess wrong.
func supportedOps(f domain.Field) []repository.Op {
	if f.Type == domain.FieldTypeRichText || f.Type == domain.FieldTypeComponent || f.Type == domain.FieldTypeDynamicZone {
		return []repository.Op{}
	}
	return repository.OpsForCardinality(f.Multiple)
}

// parseFilter turns "key:op:value" into a resolved, type-aware FieldFilter. The
// key must be a defined field and the op must be whitelisted — both are rejected
// here, before any SQL is built.
// parseFilter takes the subject because a filter is a READ of the values it
// compares against. Without the permission check below, a field hidden from the
// response is still recoverable one bit per request: `?filter=salary:gte:100000`
// answers a question about a value the caller was just refused, and `contains`
// narrows a string a character at a time. Stripping the projection while leaving
// the query surface open is not a partial defence, it is a slower one.
func parseFilter(ct *domain.ContentType, sub authn.Subject, raw string) (repository.FieldFilter, error) {
	parts := strings.SplitN(raw, ":", 3)
	if len(parts) != 3 {
		return repository.FieldFilter{}, apperrors.New("CONTENT_FILTER_MALFORMED", "filter must be key:op:value", 400).
			WithDetails(map[string]any{"filter": raw})
	}
	key, opRaw, value := parts[0], parts[1], parts[2]
	field, ok := ct.FieldByKey(key)
	if !ok {
		return repository.FieldFilter{}, apperrors.New("CONTENT_FILTER_FIELD_UNKNOWN", "filter field is not defined", 400).
			WithDetails(map[string]any{"field": key})
	}
	if !canReadField(field, sub) {
		return repository.FieldFilter{}, errFieldQueryForbidden(key, "filter")
	}
	op := repository.Op(opRaw)
	if !repository.ValidOp(op) {
		return repository.FieldFilter{}, apperrors.New("CONTENT_FILTER_OP_INVALID", "unsupported filter operator", 400).
			WithDetails(map[string]any{"op": opRaw})
	}
	// Cardinality gate, and it belongs HERE rather than in the SQL builder: the
	// scalar comparison operators emit `(payload ->> key)::numeric`, which raises
	// SQLSTATE 22P02 against an array — a 500 on a page the operator never
	// touched, from data they cannot see. Refusing before any SQL is built turns
	// that into a 400 that names the problem.
	//
	// `contains` is the one that most needs stopping, because it LOOKS like it
	// works: `(payload ->> 'tags') ILIKE '%ai%'` is true for ["ai-native"] and
	// true for ["mai"]. A false friend beats a hard error at producing wrong
	// answers nobody investigates.
	// Rich text refuses EVERY operator, and the gate sits beside the cardinality
	// one because it exists for the same two reasons: the comparison operators
	// cast `(payload ->> key)` and meet a JSON document instead of a value, and
	// `contains` ILIKEs the document's RAW TEXT — `body:contains:strong` matches
	// every entry with bold text in it, a false friend that returns confidently
	// wrong rows rather than an error. This refusal is UNCHANGED by ADR-021 —
	// `contains`'s behaviour (or lack of it, here) stays exactly what it always
	// was, for backward compatibility. Querying INSIDE rich text (and every
	// other searchable field, across the whole entry) is what the list
	// endpoint's `q` parameter is for instead: it reads entries.search_text —
	// prose extracted from rich text at write time by domain.ExtractSearchText,
	// not the raw block JSON `contains` would have ILIKEd — so a query for a
	// word inside a paragraph finds it without also matching an unrelated
	// entry that merely happens to use the same HTML-ish block type name. See
	// ListEntriesInput.Query and repository.ListEntriesFilter.Query.
	//
	// A component value is the same refusal for the same reason (ADR-020 §3):
	// an inline object, or an array of them, has no operator that means
	// anything, and a dotted path into it is an unknown field to parseFilter
	// already. Nothing under a component ever reaches the SQL builder.
	//
	// A dynamic zone is refused on the same line, and it is the stronger case:
	// its items are HETEROGENEOUS, so even a hypothetical dotted path would
	// name a sub-field that only some items have. "Which pages have a hero
	// whose title contains X" is a real question; it needs its own syntax and
	// its own index, not an operator quietly reinterpreted.
	if field.Type == domain.FieldTypeRichText || field.Type == domain.FieldTypeComponent ||
		field.Type == domain.FieldTypeDynamicZone {
		return repository.FieldFilter{}, apperrors.New("CONTENT_FILTER_OP_UNSUPPORTED_FOR_FIELD", "operator is not supported for this field", 400).
			WithDetails(map[string]any{
				"field": key, "op": opRaw, "type": field.Type,
				"supported": supportedOps(field),
			})
	}
	if !repository.OpAllowedFor(op, field.Multiple) {
		return repository.FieldFilter{}, apperrors.New("CONTENT_FILTER_OP_UNSUPPORTED_FOR_FIELD", "operator is not supported for this field", 400).
			WithDetails(map[string]any{
				"field": key, "op": opRaw, "multiple": field.Multiple,
				"supported": supportedOps(field),
			})
	}
	// Single value only. All-of already composes for free — buildWhere joins
	// clauses with AND and the handler accepts repeated ?filter= — so
	// `tags:has:ai&tags:has:ml` is all-of today. Any-of needs its own name,
	// because CSV in this grammar already means any-of (see OpIn), and it must
	// be built as OR'd @> clauses: jsonb_path_ops does not support ?| , so the
	// natural spelling would seq scan.
	if (op == repository.OpHas || op == repository.OpNhas) && strings.Contains(value, ",") {
		return repository.FieldFilter{}, apperrors.New("CONTENT_FILTER_VALUE_INVALID", "has/nhas take a single value", 400).
			WithDetails(map[string]any{"field": key, "op": opRaw, "value": value})
	}
	return repository.FieldFilter{Field: field, Op: op, Value: value}, nil
}

// parseProjection resolves ?fields= into the payload keys to keep (ADR-013 §7).
// nil means no narrowing — the whole payload, which is what every caller written
// before this got and still gets.
//
// It takes the subject and refuses an unreadable field with the SAME named 403
// filter and sort use, rather than quietly dropping the key. Silently returning
// a payload missing a key the caller asked for is the failure mode this whole
// parameter exists to avoid: an agent that asked for `salary` and got an object
// without it cannot tell "you may not read that" from "this entry has no value
// there", and will conclude the latter about every row in the tenant. The
// definition is not secret (the same credential can read it from GET /types);
// the VALUE is, and the 403 discloses neither.
//
// Ordering note: this runs BEFORE any row is fetched, so a bad key costs one
// round trip rather than a full page of work.
func parseProjection(ct *domain.ContentType, sub authn.Subject, raw []string) ([]string, error) {
	keys := make([]string, 0, len(raw))
	for _, k := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			// An empty ?fields= is how a client's URL builder spells "no
			// selection", not a request for zero fields — nobody wants an entry
			// with no payload. Treated as absent, and it is the only reading
			// under which the parameter can be built by string concatenation.
			continue
		}
		field, ok := ct.FieldByKey(k)
		if !ok {
			return nil, apperrors.New("CONTENT_FIELDS_FIELD_UNKNOWN", "projected field is not defined", 400).
				WithDetails(map[string]any{"field": k})
		}
		if !canReadField(field, sub) {
			return nil, errFieldQueryForbidden(k, "fields")
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, nil
	}
	return keys, nil
}

// parseSort takes the subject for the same reason parseFilter does, and the leak
// it closes is the wider of the two: an ORDER BY over a hidden field ranks every
// row by that value, so one page of results discloses the ordering of a column
// the caller cannot see — and paging through discloses the rest.
func parseSort(ct *domain.ContentType, sub authn.Subject, raw string) (*repository.SortSpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.SplitN(raw, ":", 2)
	key := parts[0]
	dir := "asc"
	if len(parts) == 2 {
		dir = strings.ToLower(strings.TrimSpace(parts[1]))
	}
	field, ok := ct.FieldByKey(key)
	if !ok {
		return nil, apperrors.New("CONTENT_SORT_FIELD_UNKNOWN", "sort field is not defined", 400).
			WithDetails(map[string]any{"field": key})
	}
	if dir != "asc" && dir != "desc" {
		return nil, apperrors.New("CONTENT_SORT_DIR_INVALID", "sort direction must be asc or desc", 400).
			WithDetails(map[string]any{"dir": dir})
	}
	if !canReadField(field, sub) {
		return nil, errFieldQueryForbidden(key, "sort")
	}
	// Refused rather than answered. `payload ->> key` on an array yields the
	// array's text rendering, which sorts stably and deterministically by `[`,
	// then the first element's spelling — an order nobody asked for that looks
	// correct in a UI, which is worse than an error. It also drives admin offset
	// paging, so the caller would page through a meaningless key. Sorting by
	// first element or by length are real features; both need explicit syntax.
	if field.Multiple {
		return nil, apperrors.New("CONTENT_SORT_FIELD_UNSORTABLE", "cannot sort by a multi-valued field", 400).
			WithDetails(map[string]any{"field": key, "reason": "multi-valued"})
	}
	// Same refusal as the multi-valued one, same reasoning: `payload ->> key`
	// on a block document yields its JSON text, which orders every entry by
	// `[{"` and then by whichever block type sorts first alphabetically — an
	// order that looks correct in a UI and means nothing.
	if field.Type == domain.FieldTypeRichText {
		return nil, apperrors.New("CONTENT_SORT_FIELD_UNSORTABLE", "cannot sort by a rich text field", 400).
			WithDetails(map[string]any{"field": key, "reason": "richtext"})
	}
	// And a component value, for the same reason (ADR-020 §3).
	if field.Type == domain.FieldTypeComponent {
		return nil, apperrors.New("CONTENT_SORT_FIELD_UNSORTABLE", "cannot sort by a component field", 400).
			WithDetails(map[string]any{"field": key, "reason": "component"})
	}
	// A dynamic zone is a LIST, so the multi-valued refusal above would already
	// cover it if zones carried Multiple — they do not (a list of lists is not
	// a shape this system has), so it needs its own line. Same reason, own
	// wording, so the `reason` detail still tells the caller what it hit.
	if field.Type == domain.FieldTypeDynamicZone {
		return nil, apperrors.New("CONTENT_SORT_FIELD_UNSORTABLE", "cannot sort by a dynamic zone field", 400).
			WithDetails(map[string]any{"field": key, "reason": "dynamiczone"})
	}
	return &repository.SortSpec{Field: field, Desc: dir == "desc"}, nil
}
