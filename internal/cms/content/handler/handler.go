package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/service"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/response"
)

// maxPayloadBytes caps an entry/request body to keep a stray large upload from
// exhausting memory. Generous for the PoC's small documents.
const maxPayloadBytes = 1 << 20 // 1 MiB

type Handler struct {
	svc service.ContentService
}

func NewHandler(svc service.ContentService) *Handler {
	return &Handler{svc: svc}
}

// --- content types ----------------------------------------------------------

func (h *Handler) createType(w http.ResponseWriter, r *http.Request) {
	var in service.CreateTypeInput
	if err := decodeJSON(r, &in); err != nil {
		response.Error(w, err)
		return
	}
	ctx := service.WithUsageWarnings(r.Context())
	dto, err := h.svc.CreateContentType(ctx, in)
	if err != nil {
		response.Error(w, err)
		return
	}
	setUsageWarningHeader(w, ctx)
	response.JSON(w, http.StatusCreated, dto)
}

func (h *Handler) addField(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var in service.FieldInput
	if err := decodeJSON(r, &in); err != nil {
		response.Error(w, err)
		return
	}
	ctx := service.WithUsageWarnings(r.Context())
	dto, err := h.svc.AddField(ctx, name, in)
	if err != nil {
		response.Error(w, err)
		return
	}
	setUsageWarningHeader(w, ctx)
	response.JSON(w, http.StatusCreated, dto)
}

// --- schema mutation ---------------------------------------------------------

func (h *Handler) updateType(w http.ResponseWriter, r *http.Request) {
	var in service.UpdateTypeInput
	if err := decodeJSON(r, &in); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.UpdateContentType(r.Context(), chi.URLParam(r, "name"), in)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) renameType(w http.ResponseWriter, r *http.Request) {
	var in service.RenameInput
	if err := decodeJSON(r, &in); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.RenameContentType(r.Context(), chi.URLParam(r, "name"), in)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) deleteType(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteContentType(r.Context(), chi.URLParam(r, "name")); err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusNoContent, nil)
}

func (h *Handler) updateField(w http.ResponseWriter, r *http.Request) {
	var in service.UpdateFieldInput
	if err := decodeJSON(r, &in); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.UpdateField(r.Context(), chi.URLParam(r, "name"), chi.URLParam(r, "key"), in)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) renameField(w http.ResponseWriter, r *http.Request) {
	var in service.RenameInput
	if err := decodeJSON(r, &in); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.RenameField(r.Context(), chi.URLParam(r, "name"), chi.URLParam(r, "key"), in)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) deleteField(w http.ResponseWriter, r *http.Request) {
	// Anything other than the literal "true" is not consent. Accepting "1" or
	// "yes" would make a typo in a script look like an approval.
	force := r.URL.Query().Get("force") == "true"
	dto, err := h.svc.DeleteField(r.Context(), chi.URLParam(r, "name"), chi.URLParam(r, "key"), force)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

// setUsageWarningHeader surfaces any soft-threshold warnings the service raised
// during this request as X-Content-Usage-Warning (TKT-R4b D3). Must be called
// before the response body is written.
func setUsageWarningHeader(w http.ResponseWriter, ctx context.Context) {
	ws := service.UsageWarningsFrom(ctx)
	if len(ws) == 0 {
		return
	}
	parts := make([]string, len(ws))
	for i, warn := range ws {
		parts[i] = warn.String()
	}
	w.Header().Set("X-Content-Usage-Warning", strings.Join(parts, ", "))
}

func (h *Handler) listTypes(w http.ResponseWriter, r *http.Request) {
	dtos, err := h.svc.ListContentTypes(r.Context())
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"items": dtos})
}

// exportSchema writes the artifact in its CANONICAL form rather than handing
// the struct to the envelope encoder. The whole point of the format is that a
// re-export of an unchanged schema is byte-identical to the file it came from,
// and that guarantee cannot survive a second serialiser with its own indent and
// escaping rules. It is therefore also unwrapped: an envelope would make the
// downloaded bytes something a caller has to unpack before they can diff it.
func (h *Handler) exportSchema(w http.ResponseWriter, r *http.Request) {
	art, err := h.svc.ExportSchema(r.Context())
	if err != nil {
		response.Error(w, err)
		return
	}
	body, err := domain.MarshalArtifact(art)
	if err != nil {
		response.Error(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="schema.artifact.json"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// planSchema and applySchema share a body: the artifact itself, with ?prune
// deciding whether destructive steps are in scope. prune is a QUERY parameter
// rather than a property of the document, because "this file is the complete
// list" is a statement about the invocation, not about the schema — the same
// document is a partial overlay in one call and an authority in another.
func (h *Handler) planSchema(w http.ResponseWriter, r *http.Request) {
	h.schemaChange(w, r, h.svc.PlanSchema)
}

func (h *Handler) applySchema(w http.ResponseWriter, r *http.Request) {
	h.schemaChange(w, r, h.svc.ApplySchema)
}

// decodeArtifact reads and validates the envelope for every endpoint that takes
// a schema document — plan, apply and propose.
//
// It is one function rather than one per endpoint because a proposal is a
// request to run an apply: an envelope rule that held for apply and not for
// propose would let a document into the queue that the apply then refuses,
// discovered by a person pressing a button rather than by the caller that sent
// it.
func (h *Handler) decodeArtifact(w http.ResponseWriter, r *http.Request) (domain.Artifact, bool) {
	var art domain.Artifact
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&art); err != nil {
		response.Error(w, apperrors.New("CONTENT_SCHEMA_ARTIFACT_INVALID", "malformed schema artifact", 400))
		return domain.Artifact{}, false
	}
	// The envelope is checked before anything else looks at the types. A
	// document whose kind says it is something else is not a schema this
	// endpoint should half-read and then reject deeper down with a message
	// about a field.
	if art.Kind != domain.KindContentSchema || art.ArtifactVersion != domain.ArtifactVersion1 {
		response.Error(w, apperrors.New("CONTENT_SCHEMA_ARTIFACT_UNSUPPORTED", "unsupported artifact kind or version", 422).
			WithDetails(map[string]any{"kind": art.Kind, "artifact_version": art.ArtifactVersion}))
		return domain.Artifact{}, false
	}
	return art, true
}

func (h *Handler) schemaChange(w http.ResponseWriter, r *http.Request,
	run func(context.Context, domain.Artifact, bool) (service.PlanResult, error)) {
	art, ok := h.decodeArtifact(w, r)
	if !ok {
		return
	}
	res, err := run(r.Context(), art, r.URL.Query().Get("prune") == "true")
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, res)
}

// --- schema proposals (ADR-013 §3 step 8) ------------------------------------

// proposeSchema takes the same body and the same ?prune as plan and apply, and
// that is not a coincidence to be tidied away: a proposal is a request to run
// an apply, so anything the apply reads must be part of what was proposed.
func (h *Handler) proposeSchema(w http.ResponseWriter, r *http.Request) {
	art, ok := h.decodeArtifact(w, r)
	if !ok {
		return
	}
	dto, err := h.svc.ProposeSchema(r.Context(), art, r.URL.Query().Get("prune") == "true")
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusCreated, dto)
}

func (h *Handler) listSchemaProposals(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.ListSchemaProposals(r.Context())
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"proposals": out})
}

func (h *Handler) getSchemaProposal(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, apperrors.New("CONTENT_PROPOSAL_ID_INVALID", "invalid proposal id", http.StatusBadRequest))
		return
	}
	dto, err := h.svc.GetSchemaProposal(r.Context(), id)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

// getOwnSchemaProposal is the proposer's read of its own row. It is a separate
// route from getSchemaProposal rather than a branch inside it, because the two
// differ in what they return, not only in who may call them: sharing the path
// would mean one response shape whose fields depend on the caller, and a client
// could not tell a field it may not see from one the server did not set.
func (h *Handler) getOwnSchemaProposal(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, apperrors.New("CONTENT_PROPOSAL_ID_INVALID", "invalid proposal id", http.StatusBadRequest))
		return
	}
	dto, err := h.svc.GetOwnSchemaProposal(r.Context(), id)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

// listOwnSchemaProposals is enveloped under "proposals", the same key
// listSchemaProposals uses, so the console's two list calls decode with one
// shape. The ROWS differ — this one carries the proposer's view — and that
// difference belongs in the row type, not in the envelope: a second key would
// make every generic list client branch on which URL it called.
func (h *Handler) listOwnSchemaProposals(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.ListOwnSchemaProposals(r.Context())
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"proposals": out})
}

// approveSchemaProposalRequest is the optional body of an approve call (缺口計畫
// 5.1). Steps stays nil for an absent body, a JSON `null` body, or a body with
// no "steps" key — every one of those is "approve everything", handled by
// ApproveSchemaProposal exactly as it was before this field existed.
type approveSchemaProposalRequest struct {
	Steps []int `json:"steps"`
}

func (h *Handler) approveSchemaProposal(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, apperrors.New("CONTENT_PROPOSAL_ID_INVALID", "invalid proposal id", http.StatusBadRequest))
		return
	}
	// decodeOptionalJSON, not decodeJSON: the body is genuinely optional here —
	// that is the whole backward-compatibility promise — but malformed JSON is
	// still refused rather than silently read as "no steps", on that helper's
	// own reasoning about a typo turning into a field nobody meant to omit.
	var in approveSchemaProposalRequest
	if err := decodeOptionalJSON(r, &in); err != nil {
		response.Error(w, err)
		return
	}
	plan, err := h.svc.ApproveSchemaProposal(r.Context(), id, in.Steps)
	if err != nil {
		response.Error(w, err)
		return
	}
	// The plan that was APPLIED, same body shape as /schema/apply: an approver
	// who pressed the button gets back what it did, not merely that it worked.
	response.JSON(w, http.StatusOK, plan)
}

func (h *Handler) rejectSchemaProposal(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, apperrors.New("CONTENT_PROPOSAL_ID_INVALID", "invalid proposal id", http.StatusBadRequest))
		return
	}
	if err := h.svc.RejectSchemaProposal(r.Context(), id); err != nil {
		response.Error(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) getType(w http.ResponseWriter, r *http.Request) {
	dto, err := h.svc.GetContentType(r.Context(), chi.URLParam(r, "name"))
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

// --- entries ----------------------------------------------------------------

func (h *Handler) createEntry(w http.ResponseWriter, r *http.Request) {
	typeName, err := requireType(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	payload, err := readPayload(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	ctx := service.WithUsageWarnings(r.Context())
	// ?locale= picks the language; ?translation_of= makes this a sibling of an
	// existing entry rather than a new piece of content.
	// Idempotency-Key is a HEADER, not a query parameter, matching POST /register
	// (the platform's other idempotent create) and the convention every HTTP
	// client already implements retries around. Absent means no promise.
	in := service.CreateLocalizedInput{
		Payload:        payload,
		Locale:         r.URL.Query().Get("locale"),
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("translation_of")); raw != "" {
		srcID, perr := uuid.Parse(raw)
		if perr != nil {
			response.Error(w, apperrors.New("CONTENT_TRANSLATION_OF_INVALID", "translation_of must be a uuid", 400))
			return
		}
		in.TranslationOf = &srcID
	}
	// ?copy_from_source=true seeds the new translation from the source entry's
	// working copy (service.CreateLocalizedInput.CopyFromSource) — a caller who
	// wants a blank sibling simply omits it, which is the existing behaviour.
	if raw := strings.TrimSpace(r.URL.Query().Get("copy_from_source")); raw != "" {
		copyFromSource, perr := strconv.ParseBool(raw)
		if perr != nil {
			response.Error(w, apperrors.New("CONTENT_COPY_FROM_SOURCE_INVALID", "copy_from_source must be a boolean", 400))
			return
		}
		in.CopyFromSource = copyFromSource
	}
	dto, err := h.svc.CreateLocalizedEntry(ctx, typeName, in)
	if err != nil {
		response.Error(w, err)
		return
	}
	setUsageWarningHeader(w, ctx)
	response.JSON(w, http.StatusCreated, dto)
}

func (h *Handler) usage(w http.ResponseWriter, r *http.Request) {
	dto, err := h.svc.Usage(r.Context())
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

// listActivity serves the tenant's activity stream, newest first.
//
// ?entry= narrows to one entry — the question §1's release screen asks when it
// attributes each changed field to whoever last touched it (step 4). An
// unparseable id is a 400 rather than a silently ignored parameter: dropping it
// would answer with the WHOLE tenant's stream to a caller who asked about one
// entry, which reads as "this entry has a lot of history".
func (h *Handler) listActivity(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	in := service.ListActivityInput{}
	if raw := q.Get("entry"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			response.Error(w, apperrors.New("CONTENT_ENTRY_ID_INVALID", "entry must be a UUID", 400).
				WithDetails(map[string]any{"entry": raw}))
			return
		}
		in.EntryID = &id
	}
	in.Limit, _ = strconv.Atoi(q.Get("limit"))
	rows, err := h.svc.ListActivity(r.Context(), in)
	if err != nil {
		response.Error(w, err)
		return
	}
	// `items`, and never a bare array: a top-level JSON array is the shape that
	// cannot grow a sibling key later without breaking every client.
	response.JSON(w, http.StatusOK, map[string]any{"items": rows})
}

// listPendingReview serves the release queue — everything across every content
// type whose working copy is not what the public sees (ADR-014 §2's top half).
//
// It takes NO filter beyond a limit, unlike listActivity's ?entry=. The queue's
// definition is "what is waiting on you", and every narrowing a caller could ask
// for is a way to make something waiting stop being visible.
func (h *Handler) listPendingReview(w http.ResponseWriter, r *http.Request) {
	in := service.ListPendingReviewInput{}
	in.Limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := h.svc.ListPendingReview(r.Context(), in)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"items": rows})
}

// searchContent is GET /api/v1/content/search — the cross-type equivalent of
// listEntries' `q`, for a caller who does not know (or does not want to name)
// which content type holds the entry they are looking for (ADR-021 §3). `q`
// is required here, unlike listEntries — a bare "show me everything, across
// every type" is what listPendingReview / the per-type list already answer
// more cheaply, so an empty `q` is refused by the service rather than treated
// as "no search".
func (h *Handler) searchContent(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	items, err := h.svc.Search(r.Context(), service.SearchInput{
		Query:  q.Get("q"),
		Locale: q.Get("locale"),
		Status: q.Get("status"),
		Limit:  limit,
	})
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"items": items})
}

// listLocales is GET /api/v1/content/locales — the inventory a console needs
// to draw a locale switcher: every locale in use in the tenant and how many
// entries carry it. `type` narrows to one content type; omitted means every
// type, matching listEntries' own `locale` convention of "" = unconfined.
func (h *Handler) listLocales(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.ListLocales(r.Context(), service.ListLocalesInput{
		Type: r.URL.Query().Get("type"),
	})
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) listEntries(w http.ResponseWriter, r *http.Request) {
	typeName, err := requireType(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	res, err := h.svc.ListEntries(r.Context(), typeName, service.ListEntriesInput{
		Filters: q["filter"], // repeated ?filter= params
		Fields:  csvParams(q["fields"]),
		// Relation expansion (ADR-006 Amendment 6, admin side Amendment 7).
		// Same wire shape as `fields` — repeated or comma-separated — because
		// they are the same kind of parameter: a list of this type's field
		// keys. The service owns which keys are legal, which audience may ask,
		// and which copy gets expanded; the router reads the parameter for
		// every caller so that a refusal is always the audience rule speaking
		// rather than the handler having forgotten to pass it on.
		Populate: csvParams(q["populate"]),
		Sort:     q.Get("sort"),
		Status:   q.Get("status"), // "" = all states (admin default)
		Locale:   q.Get("locale"), // "" = all locales (admin default)
		Limit:    limit,
		Offset:   offset,
		Cursor:   q.Get("cursor"), // delivery audience only; opaque round-trip token
		// Free-text search (ADR-021): "" leaves search out of the query
		// entirely, exactly like an absent `filter`. Legal for both admin and
		// delivery audiences — the service picks search_text or
		// published_search_text from the same audience decision it already
		// makes for every other predicate.
		Query: q.Get("q"),
	})
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, res)
}

// csvParams flattens ?fields=a,b&fields=c into one list (ADR-013 §7). Both
// spellings are accepted because both are unambiguous and mean the same thing:
// the repeated form is what ?filter= already uses, and the comma form is what a
// hand-written URL and every HTTP client's docs reach for. The service does the
// trimming and the validating — this is only the wire shape.
func csvParams(values []string) []string {
	var out []string
	for _, v := range values {
		out = append(out, strings.Split(v, ",")...)
	}
	return out
}

func (h *Handler) getEntry(w http.ResponseWriter, r *http.Request) {
	typeName, err := requireType(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.GetEntry(r.Context(), typeName, id, service.GetEntryInput{
		Populate: csvParams(r.URL.Query()["populate"]),
	})
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) updateEntry(w http.ResponseWriter, r *http.Request) {
	typeName, err := requireType(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	payload, err := readPayload(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	expectedVersion, err := parseIfMatchVersion(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.UpdateEntry(r.Context(), typeName, id, payload, expectedVersion)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) listTranslations(w http.ResponseWriter, r *http.Request) {
	typeName, err := requireType(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	items, err := h.svc.ListTranslations(r.Context(), typeName, id)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"items": items})
}

// entryAttribution serves who last changed each field of one entry (ADR-014 §6).
//
// The response is the DTO itself rather than an `items` list, because the shape
// is a map keyed by field and a key that is ABSENT is the answer "nobody's write
// to this field was recorded" — the console renders that as unknown. Wrapping it
// in a list would put the reader one loop away from that distinction.
func (h *Handler) entryAttribution(w http.ResponseWriter, r *http.Request) {
	typeName, err := requireType(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.EntryFieldAttribution(r.Context(), typeName, id)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

// createPreviewLink mints a credential that shows this entry's working copy
// through the public delivery edge (ADR-006).
//
// POST despite reading nothing: it creates a credential that did not exist
// before, and a GET would put a live bearer token into browser history, referrer
// headers and every proxy log between here and the caller. The response is
// no-store for the same reason the edge's preview response is — this body IS the
// credential.
//
// 201, not 200: the resource being reported is the link, and it is new.
func (h *Handler) createPreviewLink(w http.ResponseWriter, r *http.Request) {
	typeName, err := requireType(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	link, err := h.svc.CreatePreviewLink(r.Context(), typeName, id)
	if err != nil {
		response.Error(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	response.JSON(w, http.StatusCreated, link)
}

// --- media (ADR-005) --------------------------------------------------------

// createMediaUpload reserves an asset. The body is OPTIONAL: this endpoint
// shipped taking only ?content_type=, and every existing caller still sends
// exactly that with no body at all. Seeding the input from the query and then
// decoding the body over it means a body that omits content_type inherits the
// query value, while a body that names one wins — so the new fields become
// available without a flag day for the callers that do not want them.
func (h *Handler) createMediaUpload(w http.ResponseWriter, r *http.Request) {
	in := service.CreateMediaUploadInput{ContentType: r.URL.Query().Get("content_type")}
	if err := decodeOptionalJSON(r, &in); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.CreateMediaUpload(r.Context(), in)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusCreated, dto)
}

// updateMediaAsset patches client-declared metadata. Deliberately no If-Match:
// see service.UpdateMediaAsset. The body is required here — a PATCH with no body
// is a caller bug, not a no-op request, and the three-state decode needs a
// document to read the keys out of.
func (h *Handler) updateMediaAsset(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	var in service.UpdateMediaAssetInput
	if err := decodeJSON(r, &in); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.UpdateMediaAsset(r.Context(), id, in)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) completeMediaUpload(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.CompleteMediaUpload(r.Context(), id)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

// listMediaAssets parses paging and filter params the same way listEntries
// does; validation of `kind` (and everything else) is the service's job, not
// the handler's — see ListMediaAssets.
func (h *Handler) listMediaAssets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	res, err := h.svc.ListMediaAssets(r.Context(), service.ListMediaInput{
		Query:  q.Get("q"),
		Kind:   q.Get("kind"),
		Limit:  limit,
		Offset: offset,
		// ?orphan=true narrows to assets no entry references, draft or
		// published (ADR-024) — a media-library "safe to delete" view.
		Orphan: q.Get("orphan") == "true",
	})
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, res)
}

func (h *Handler) getMediaAsset(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.GetMediaAsset(r.Context(), id)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

// resolveMediaURL hands back a short-lived signed URL rather than the bytes:
// the API must never carry media traffic (ADR-005).
func (h *Handler) resolveMediaURL(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	// ?preset= picks a rendition (ADR-019); absent means the original. The
	// service validates the name and falls back to the original for a
	// rendition that is not ready — this handler only carries the string.
	url, expires, err := h.svc.ResolveMediaURL(r.Context(), id, r.URL.Query().Get("preset"))
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"url": url, "expires_at": expires})
}

// enqueueMediaVariants (re)queues an image's renditions. 202, not 200: the
// response carries the rows as they stand — every preset back to pending —
// and the work itself happens in the transform worker on its own clock.
func (h *Handler) enqueueMediaVariants(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.EnqueueMediaVariants(r.Context(), id)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusAccepted, dto)
}

func (h *Handler) deleteMediaAsset(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	// ?force=true skips the entry_media / entry_media_published in-use check
	// (ADR-024) — the pre-ADR-024 hard delete, kept as an explicit opt-in rather
	// than the default.
	force := r.URL.Query().Get("force") == "true"
	if err := h.svc.DeleteMediaAsset(r.Context(), id, force); err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusNoContent, nil)
}

// sweepMediaAssets runs one bounded orphan-sweep batch. The body is optional
// (decodeOptionalJSON): an empty body sweeps with the service defaults. Only
// older_than is interpreted here — a duration string — because a garbage
// string must fail loudly as a 400 rather than silently mean "default"; limit
// clamping stays in the service so every caller agrees on it.
func (h *Handler) sweepMediaAssets(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OlderThan string `json:"older_than"`
		Limit     int    `json:"limit"`
		DryRun    bool   `json:"dry_run"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		response.Error(w, err)
		return
	}
	var olderThan time.Duration
	if body.OlderThan != "" {
		d, err := time.ParseDuration(body.OlderThan)
		if err != nil {
			response.Error(w, apperrors.New("CONTENT_SWEEP_OLDER_THAN_INVALID", "older_than must be a Go duration string (e.g. \"720h\")", http.StatusBadRequest))
			return
		}
		olderThan = d
	}
	res, err := h.svc.SweepOrphans(r.Context(), service.SweepInput{
		OlderThan: olderThan,
		Limit:     body.Limit,
		DryRun:    body.DryRun,
	})
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, res)
}

// entryReferencedBy and mediaReferencedBy are GET .../referenced-by (ADR-024,
// 2.8a): the reverse of a relation field / a file or richtext media reference.
// limit/offset parse exactly like listMediaAssets' — bare strconv.Atoi, with
// the default/max clamp left to the service (clampReferencedByPage) so the two
// handlers and every future caller of the service method agree on it.

func (h *Handler) entryReferencedBy(w http.ResponseWriter, r *http.Request) {
	typeName, err := requireType(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	dto, err := h.svc.EntryReferencedBy(r.Context(), typeName, id, limit, offset)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

func (h *Handler) mediaReferencedBy(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	dto, err := h.svc.MediaReferencedBy(r.Context(), id, limit, offset)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

// publishEntry / unpublishEntry are separate verbs rather than a PATCH on a
// `status` field: editorial state is not part of the entry payload, and keeping
// it off the PATCH surface means a routine content edit can never flip an entry
// live by accident. Both honour If-Match like updateEntry.
func (h *Handler) publishEntry(w http.ResponseWriter, r *http.Request) {
	h.setStatus(w, r, domain.StatusPublished)
}

func (h *Handler) unpublishEntry(w http.ResponseWriter, r *http.Request) {
	h.setStatus(w, r, domain.StatusDraft)
}

func (h *Handler) setStatus(w http.ResponseWriter, r *http.Request, status string) {
	typeName, err := requireType(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	expectedVersion, err := parseIfMatchVersion(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.SetEntryStatus(r.Context(), typeName, id, status, expectedVersion)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

// --- scheduled publish (ADR-017) --------------------------------------------

// scheduleEntry files a publish/unpublish to happen later.
//
// It carries NO If-Match, unlike its publish/unpublish neighbours above, and
// the omission is the design rather than an oversight. If-Match asks "is the
// entry still what I read?", which is the right question when the write lands
// now; this write lands on Friday, and the answer at request time says nothing
// about the answer then. The version is checked anyway — the service pins it
// into the schedule and the worker re-checks it before executing — so the
// precondition an editor actually wants is enforced across the whole wait
// instead of for the length of one HTTP request.
func (h *Handler) scheduleEntry(w http.ResponseWriter, r *http.Request) {
	typeName, err := requireType(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	var in service.ScheduleEntryInput
	if err := decodeJSON(r, &in); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.ScheduleEntry(r.Context(), typeName, id, in)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusCreated, dto)
}

// cancelEntrySchedule withdraws the pending schedule. 404 when there is none —
// "nothing is scheduled" is the same answer whether the entry never had a
// schedule or its last one already ran, and a caller who needs to tell those
// apart reads the GET below.
func (h *Handler) cancelEntrySchedule(w http.ResponseWriter, r *http.Request) {
	typeName, err := requireType(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	if err := h.svc.CancelEntrySchedule(r.Context(), typeName, id); err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusNoContent, nil)
}

// getEntrySchedule returns the entry's MOST RECENT schedule in any state, not
// only a pending one. An editor who comes back on Monday asking "did it go
// out?" is asking about a row that is no longer pending, and answering 404
// because the work is done would leave `stale` and `failed` — the two outcomes
// somebody has to act on — visible nowhere at all.
func (h *Handler) getEntrySchedule(w http.ResponseWriter, r *http.Request) {
	typeName, err := requireType(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.GetEntrySchedule(r.Context(), typeName, id)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

// --- publish revisions and restore (ADR-018) --------------------------------

// listEntryRevisions returns the entry's releases, newest first, without their
// payloads. 200 with `[]` for an entry that has never been published — that is a
// true answer about an entry that exists, and 404 would deny the entry itself.
func (h *Handler) listEntryRevisions(w http.ResponseWriter, r *http.Request) {
	typeName, id, err := typeAndID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	out, err := h.svc.ListEntryRevisions(r.Context(), typeName, id)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, out)
}

// getEntryRevision returns one release with its snapshot, masked to what this
// reader may see.
func (h *Handler) getEntryRevision(w http.ResponseWriter, r *http.Request) {
	typeName, id, err := typeAndID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	no, err := parseRevisionNo(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.GetEntryRevision(r.Context(), typeName, id, no)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

// diffEntryRevision compares the release against the CURRENT WORKING COPY — what
// pressing restore would change about the draft in front of you.
func (h *Handler) diffEntryRevision(w http.ResponseWriter, r *http.Request) {
	typeName, id, err := typeAndID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	no, err := parseRevisionNo(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.DiffEntryRevision(r.Context(), typeName, id, no)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

// restoreEntryRevision writes an old release back into the working copy.
//
// 200, not 201 and not 202: nothing was created — the entry already existed and
// still has the same id — and nothing was deferred. The body is the entry as it
// now stands plus what the restore could not carry over.
//
// IT CARRIES If-Match, unlike its schedule neighbour above and exactly like
// PATCH /entries/{id}, because this write lands NOW. "Is the entry still what I
// read?" is the right question when the answer at request time is the answer at
// write time, and a restore is the write most likely to be pressed after
// staring at a diff for a while — which is precisely the window in which a
// colleague's save arrives.
func (h *Handler) restoreEntryRevision(w http.ResponseWriter, r *http.Request) {
	typeName, id, err := typeAndID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	expectedVersion, err := parseIfMatchVersion(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	var in service.RestoreEntryInput
	if err := decodeJSON(r, &in); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.RestoreEntryRevision(r.Context(), typeName, id, in, expectedVersion)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, dto)
}

// requestEntryChanges sends an entry back to its author (ADR-014 Amendment:
// review decisions).
//
// 201, not 200: this creates a new row — the decision — distinct from the
// entry it concerns, on RestoreEntryRevision's own distinction the other way
// (that one is 200 because nothing new exists afterward; this one is 201
// because something now does).
//
// IT CARRIES If-Match, on RestoreEntryRevision's exact reasoning: a decision
// is made against a version the reviewer read moments ago, and a colleague's
// save landing in that window is precisely the case the precondition exists
// to catch.
func (h *Handler) requestEntryChanges(w http.ResponseWriter, r *http.Request) {
	typeName, id, err := typeAndID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	expectedVersion, err := parseIfMatchVersion(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	var in service.RequestChangesInput
	if err := decodeJSON(r, &in); err != nil {
		response.Error(w, err)
		return
	}
	dto, err := h.svc.RequestChanges(r.Context(), typeName, id, in, expectedVersion)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusCreated, dto)
}

// listEntryReviewDecisions returns the entry's decision history, newest
// first.
func (h *Handler) listEntryReviewDecisions(w http.ResponseWriter, r *http.Request) {
	typeName, id, err := typeAndID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	out, err := h.svc.ListEntryReviewDecisions(r.Context(), typeName, id)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, out)
}

// typeAndID is the two-line preamble every entry sub-resource opens with. Four
// new endpoints was the point at which copying it a fifth, sixth, seventh and
// eighth time stopped being the cheaper option; the existing handlers are left
// as they are rather than churned, since the pair is order-independent and
// neither call can fail in a way the other hides.
func typeAndID(r *http.Request) (string, uuid.UUID, error) {
	typeName, err := requireType(r)
	if err != nil {
		return "", uuid.Nil, err
	}
	id, err := parseID(r)
	if err != nil {
		return "", uuid.Nil, err
	}
	return typeName, id, nil
}

// parseRevisionNo reads {no} from the path.
//
// A 400 with the offending text rather than letting a non-numeric segment fall
// through to a 404: `/revisions/latest` is a client that guessed at the API, and
// telling it "no such revision" would send its author hunting for a row that was
// never the problem. Non-positive is the same mistake — ordinals start at 1 —
// and the service refuses it again for callers that do not arrive by HTTP.
func parseRevisionNo(r *http.Request) (int, error) {
	raw := chi.URLParam(r, "no")
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, service.ErrRevisionNoInvalid(raw)
	}
	return n, nil
}

// parseIfMatchVersion reads an optional optimistic-concurrency precondition from
// the If-Match header — the entry version the client last read. Absent → 0 (no
// client precondition; the store still guards the write on the read version).
// ETag-style quoting (If-Match: "3") is accepted.
func parseIfMatchVersion(r *http.Request) (int, error) {
	raw := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`)
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return 0, apperrors.New("INVALID_IF_MATCH", "If-Match must be a positive integer version", 400)
	}
	return v, nil
}

func (h *Handler) deleteEntry(w http.ResponseWriter, r *http.Request) {
	typeName, err := requireType(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	if err := h.svc.DeleteEntry(r.Context(), typeName, id); err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusNoContent, nil)
}

// --- helpers ----------------------------------------------------------------

func requireType(r *http.Request) (string, error) {
	t := r.URL.Query().Get("type")
	if t == "" {
		return "", apperrors.New("CONTENT_TYPE_REQUIRED", "missing required ?type= query parameter", 400)
	}
	return t, nil
}

func parseID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, apperrors.Wrap("INVALID_ID", "invalid id", 400, err)
	}
	return id, nil
}

// readPayload returns the raw request body as a JSON object. It enforces a size
// cap and confirms the body is a JSON object so it can be stored as JSONB.
func readPayload(r *http.Request) (json.RawMessage, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayloadBytes+1))
	if err != nil {
		return nil, apperrors.Wrap("INVALID_JSON", "could not read request body", 400, err)
	}
	if len(body) > maxPayloadBytes {
		return nil, apperrors.New("PAYLOAD_TOO_LARGE", "request body too large", 413)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, apperrors.Wrap("CONTENT_PAYLOAD_INVALID", "payload must be a JSON object", 400, err)
	}
	return json.RawMessage(body), nil
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxPayloadBytes+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return apperrors.Wrap("INVALID_JSON", "invalid request body", 400, err)
	}
	if dec.More() {
		return apperrors.Wrap("INVALID_JSON", "invalid request body", 400, errors.New("unexpected trailing JSON"))
	}
	return nil
}

// decodeOptionalJSON is decodeJSON for an endpoint where the body is genuinely
// optional, leaving dst untouched when there is none. Only an EMPTY body is
// forgiven — malformed JSON, unknown keys and trailing content are still 400,
// because "the body was unreadable" must never quietly degrade into "there was
// no body", which is how a caller's typo becomes a silently ignored field.
func decodeOptionalJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxPayloadBytes+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return apperrors.Wrap("INVALID_JSON", "invalid request body", 400, err)
	}
	if dec.More() {
		return apperrors.Wrap("INVALID_JSON", "invalid request body", 400, errors.New("unexpected trailing JSON"))
	}
	return nil
}

// --- webhooks (ADR-011) -------------------------------------------------------

func (h *Handler) createWebhook(w http.ResponseWriter, r *http.Request) {
	var in service.CreateWebhookInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		response.Error(w, apperrors.New("INVALID_BODY", "invalid request body", http.StatusBadRequest))
		return
	}
	dto, err := h.svc.CreateWebhook(r.Context(), in)
	if err != nil {
		response.Error(w, err)
		return
	}
	// 201 carries the secret — the only response that ever does.
	response.JSON(w, http.StatusCreated, dto)
}

func (h *Handler) listWebhooks(w http.ResponseWriter, r *http.Request) {
	dtos, err := h.svc.ListWebhooks(r.Context())
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"items": dtos})
}

func (h *Handler) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	if err := h.svc.DeleteWebhook(r.Context(), id); err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusNoContent, nil)
}
