package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

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

func (h *Handler) approveSchemaProposal(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.Error(w, apperrors.New("CONTENT_PROPOSAL_ID_INVALID", "invalid proposal id", http.StatusBadRequest))
		return
	}
	plan, err := h.svc.ApproveSchemaProposal(r.Context(), id)
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
		Sort:    q.Get("sort"),
		Status:  q.Get("status"), // "" = all states (admin default)
		Locale:  q.Get("locale"), // "" = all locales (admin default)
		Limit:   limit,
		Offset:  offset,
		Cursor:  q.Get("cursor"), // delivery audience only; opaque round-trip token
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
	dto, err := h.svc.GetEntry(r.Context(), typeName, id)
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
	url, expires, err := h.svc.ResolveMediaURL(r.Context(), id)
	if err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusOK, map[string]any{"url": url, "expires_at": expires})
}

func (h *Handler) deleteMediaAsset(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		response.Error(w, err)
		return
	}
	if err := h.svc.DeleteMediaAsset(r.Context(), id); err != nil {
		response.Error(w, err)
		return
	}
	response.JSON(w, http.StatusNoContent, nil)
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
