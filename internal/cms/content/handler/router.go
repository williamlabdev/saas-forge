package handler

import (
	"github.com/go-chi/chi/v5"
)

// Routes mounts the content endpoints onto the given chi router. One set of
// endpoints serves every content type — the type is selected by path
// ({name}) for schema operations and by the ?type= query for entries.
//
// Mount this from the application router (internal/platform/router.go) by
// calling contentH.Routes(r) alongside the other domain handlers.
func (h *Handler) Routes(r chi.Router) {
	r.Route("/api/v1/content", func(r chi.Router) {
		// Schema: content types + their fields.
		r.Post("/types", h.createType)
		r.Get("/types", h.listTypes)
		r.Get("/types/{name}", h.getType)
		r.Post("/types/{name}/fields", h.addField)

		// Schema mutation. Renames are their own verb rather than a property on
		// the PATCH, for the same reason publish/unpublish are: a routine label
		// edit must not be able to rewrite every stored document because someone
		// typed the wrong string into a field.
		//
		// Deleting a FIELD returns 200 with the type, because the resource that
		// changed still exists and the caller needs its new state; deleting a
		// TYPE returns 204, because there is nothing left to return.
		r.Patch("/types/{name}", h.updateType)
		r.Post("/types/{name}/rename", h.renameType)
		r.Delete("/types/{name}", h.deleteType)
		r.Patch("/types/{name}/fields/{key}", h.updateField)
		r.Post("/types/{name}/fields/{key}/rename", h.renameField)
		r.Delete("/types/{name}/fields/{key}", h.deleteField)

		// Schema as a portable artifact (ADR-008). Not under /types, because it
		// is the whole collection rather than one member of it.
		r.Get("/schema/export", h.exportSchema)
		// plan is a POST despite writing nothing: it carries a body, and a
		// schema document does not belong in a query string.
		r.Post("/schema/plan", h.planSchema)
		r.Post("/schema/apply", h.applySchema)

		// Proposals (ADR-013 §3 step 8). Under /schema because what is proposed
		// is a schema document; the collection is separate from /schema/apply
		// because a proposal outlives the request that filed it.
		//
		// Approve is a POST to a sub-path rather than a PATCH of the status:
		// approving RUNS something, and the response is the plan it applied.
		r.Post("/schema/proposals", h.proposeSchema)
		r.Get("/schema/proposals", h.listSchemaProposals)
		// "mine" is the proposer's door and the only proposal surface an agent may
		// open (ADR-013 未解項). The queue and the approver's read stay closed to
		// it — their plans are full-scope and name types outside its whitelist.
		//
		// Listed first for the reader, not for the router: chi resolves a static
		// segment ahead of a param at the same position whatever the registration
		// order, MEASURED by swapping these two lines and watching the routing
		// test stay green.
		//
		// The list is what makes the single read reachable: an id is the only
		// way into /mine/{id}, and before this route the only place a proposer
		// ever saw one was the response to the POST that filed it.
		r.Get("/schema/proposals/mine", h.listOwnSchemaProposals)
		r.Get("/schema/proposals/mine/{id}", h.getOwnSchemaProposal)
		r.Get("/schema/proposals/{id}", h.getSchemaProposal)
		r.Post("/schema/proposals/{id}/approve", h.approveSchemaProposal)
		r.Post("/schema/proposals/{id}/reject", h.rejectSchemaProposal)

		// Entries (generic over ?type=).
		r.Post("/entries", h.createEntry)
		r.Get("/entries", h.listEntries)
		r.Get("/entries/{id}", h.getEntry)
		r.Patch("/entries/{id}", h.updateEntry)
		r.Delete("/entries/{id}", h.deleteEntry)

		// Every language of one piece of content (one row per locale, related
		// by translation_group_id).
		r.Get("/entries/{id}/translations", h.listTranslations)

		// Who last changed each field, for the release screen's diff (ADR-014
		// §6). Its own resource rather than a field on the entry: attribution
		// costs a query the list path must not pay per row, and a field that
		// only some read paths populate would render as "unknown" on the
		// others — indistinguishable from a field nobody's write was recorded
		// for, which is the one confusion §4's three states exist to prevent.
		r.Get("/entries/{id}/attribution", h.entryAttribution)

		// A shareable link showing THIS entry's working copy through the public
		// delivery edge (ADR-006). POST because it mints a credential — see the
		// handler for why that must not be a GET.
		r.Post("/entries/{id}/preview-link", h.createPreviewLink)

		// Editorial state transitions. Separate verbs, not a PATCH field —
		// publishing must be deliberate (ADR-004). Each locale publishes
		// independently, which is why status lives per row.
		r.Post("/entries/{id}/publish", h.publishEntry)
		r.Post("/entries/{id}/unpublish", h.unpublishEntry)

		// Media. Bytes never pass through here — the platform signs short-lived
		// URLs and the client talks to object storage directly (ADR-005).
		r.Post("/media", h.createMediaUpload)
		r.Post("/media/{id}/complete", h.completeMediaUpload)
		r.Get("/media/{id}", h.getMediaAsset)
		// Client-DECLARED metadata only (filename / alt text / dimensions). A
		// PATCH here can never change what bytes are stored or who may read
		// them, which is why it carries no If-Match and sits apart from the
		// upload verbs above.
		r.Patch("/media/{id}", h.updateMediaAsset)
		r.Get("/media/{id}/url", h.resolveMediaURL)
		r.Delete("/media/{id}", h.deleteMediaAsset)

		// Per-tenant plan usage vs limits (TKT-R4b).
		r.Get("/usage", h.usage)

		// The activity record (ADR-014 §3): who did what to which thing, and
		// whether it worked. Read-only by design — the table is append-only and
		// nothing outside the service writes to it, so there is no POST here and
		// no endpoint that could edit a line after the fact.
		r.Get("/activity", h.listActivity)

		// The release queue (ADR-014 §2): everything waiting on a person, across
		// every content type. It sits beside /activity rather than under
		// /entries because it belongs to no single type — which is also why an
		// agent credential cannot reach it; see ListPendingReview.
		r.Get("/pending-review", h.listPendingReview)

		// Webhooks (ADR-011): where this tenant's content events are announced.
		// Registration answers with the signing secret exactly once; there is
		// no GET-one and no PATCH — rotation is delete-and-register.
		r.Post("/webhooks", h.createWebhook)
		r.Get("/webhooks", h.listWebhooks)
		r.Delete("/webhooks/{id}", h.deleteWebhook)
	})
}
