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

		// Reusable components (ADR-020): named sub-field lists a type field of
		// type "component" embeds. Same verbs as /types, same consent rule on
		// delete; values live inside entries, so there is no /components/{name}/
		// entries and never will be.
		r.Post("/components", h.createComponent)
		r.Get("/components", h.listComponents)
		// Built-in component templates (ADR-020 Amendment 2), listed here for
		// the reader rather than for the router: chi resolves a static segment
		// ahead of a param at the same position whatever the registration
		// order (the same fact /schema/proposals/mine relies on above), which
		// is what makes GET /components/templates reachable at all rather than
		// being swallowed by GET /components/{name}. Proven by
		// TestComponentTemplateRoutes_StaticBeatsParam, which registers these
		// two lines in the OPPOSITE order and asserts routing is unchanged.
		// The same reason is why a component can never be named "templates" —
		// see validateComponentName.
		r.Get("/components/templates", h.listComponentTemplates)
		r.Post("/components/templates/{name}", h.installComponentTemplate)
		r.Get("/components/{name}", h.getComponent)
		r.Patch("/components/{name}", h.updateComponent)
		r.Post("/components/{name}/rename", h.renameComponent)
		r.Delete("/components/{name}", h.deleteComponent)
		r.Post("/components/{name}/fields", h.addComponentField)
		r.Patch("/components/{name}/fields/{key}", h.updateComponentField)
		r.Post("/components/{name}/fields/{key}/rename", h.renameComponentField)
		r.Delete("/components/{name}/fields/{key}", h.deleteComponentField)

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

		// A reviewer sending an entry back (ADR-014 Amendment: review
		// decisions), sitting beside publish/unpublish because it takes the
		// SAME verb: content:publish. GET returns the full history; the entry's
		// CURRENT verdict, if its version still matches, rides on the entry
		// itself as review_decision.
		r.Post("/entries/{id}/review/request-changes", h.requestEntryChanges)
		r.Get("/entries/{id}/review-decisions", h.listEntryReviewDecisions)

		// The same two transitions, deferred (ADR-017). One pending schedule per
		// entry, so the resource is singular — POST files it, DELETE withdraws
		// it, GET reports the latest one in any state. Authorization follows the
		// verb being scheduled, not the verb of the HTTP request: scheduling a
		// publish needs content:publish, which is what keeps ADR-014 §1's human
		// gate from being walked around by asking for it on Friday instead.
		r.Post("/entries/{id}/schedule", h.scheduleEntry)
		r.Delete("/entries/{id}/schedule", h.cancelEntrySchedule)
		r.Get("/entries/{id}/schedule", h.getEntrySchedule)

		// What went live, release by release, and putting one back (ADR-018).
		// PLURAL and addressed by an ordinal, unlike the singular schedule
		// above: an entry has one pending intent at a time but many past
		// releases, and revision_no is what an editor can actually say out loud.
		//
		// RESTORE IS A POST ON THE ENTRY, not on the revision, and the shape is
		// the semantics: the thing that changes is the entry's working copy, and
		// the revision is an argument to that change. `POST
		// /revisions/{no}/restore` would read as an operation on the revision,
		// which is immutable and is not what moves. The number therefore travels
		// in the body — where If-Match's precondition already lives beside it —
		// so one request carries the whole of "replace this entry, from this
		// release, provided it is still at the version I read".
		//
		// It takes content:update, NOT content:publish, because it does not
		// publish: it writes the draft and leaves the live snapshot alone.
		// Releasing the restored draft is still a separate act through the
		// publish route above, which is what keeps ADR-014 §1's human gate
		// standing rather than routed around by a POST with a nicer name.
		r.Get("/entries/{id}/revisions", h.listEntryRevisions)
		r.Get("/entries/{id}/revisions/{no}", h.getEntryRevision)
		r.Get("/entries/{id}/revisions/{no}/diff", h.diffEntryRevision)
		r.Post("/entries/{id}/restore", h.restoreEntryRevision)

		// Reverse references (ADR-024, 2.8a): who points at this entry, read from
		// the maintained entry_relations index rather than scanned from every
		// other entry's payload.
		r.Get("/entries/{id}/referenced-by", h.entryReferencedBy)

		// Media. Bytes never pass through here — the platform signs short-lived
		// URLs and the client talks to object storage directly (ADR-005).
		r.Post("/media", h.createMediaUpload)
		// The admin media library (paginated, uploaded assets only). Listed
		// before /media/{id} for the reader, not the router — chi resolves the
		// static segment ahead of the param regardless of registration order —
		// but it belongs beside the other collection-shaped GET, not buried
		// after the single-resource routes below it.
		r.Get("/media", h.listMediaAssets)
		// One bounded orphan-sweep batch (003-orphan-gc): an action, not a
		// resource, hence POST beside the collection GET — the same static
		// segment convention as the comment above.
		r.Post("/media/sweep", h.sweepMediaAssets)
		r.Post("/media/{id}/complete", h.completeMediaUpload)
		r.Get("/media/{id}", h.getMediaAsset)
		// Client-DECLARED metadata only (filename / alt text / dimensions). A
		// PATCH here can never change what bytes are stored or who may read
		// them, which is why it carries no If-Match and sits apart from the
		// upload verbs above.
		r.Patch("/media/{id}", h.updateMediaAsset)
		r.Get("/media/{id}/url", h.resolveMediaURL)
		// Renditions (ADR-019): the worker makes them; this only asks it to.
		r.Post("/media/{id}/variants", h.enqueueMediaVariants)
		// Who points at this asset (ADR-024, 2.8a) — the same reverse-lookup
		// shape as /entries/{id}/referenced-by, over entry_media instead.
		r.Get("/media/{id}/referenced-by", h.mediaReferencedBy)
		// ?force=true bypasses the entry_media in-use check below (ADR-024).
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

		// Cross-type free-text search (ADR-021 §3): every entry in the tenant
		// matching `q`, across every content type. Sits beside /pending-review
		// for the same reason — it belongs to no single type, so it cannot live
		// under /entries. /entries' own `q` (listEntries, above) stays the
		// per-type answer; this is for a caller who does not know which type
		// holds what it is looking for.
		r.Get("/search", h.searchContent)

		// Locale inventory (T2 2.3): every locale in use in the tenant, and how
		// many entries carry it — optionally narrowed to one type via `type`.
		// Sits beside /search for the same reason: it belongs to no single
		// entry, so it cannot live under /entries/{id}.
		r.Get("/locales", h.listLocales)

		// Webhooks (ADR-011): where this tenant's content events are announced.
		// Registration answers with the signing secret exactly once; there is
		// no GET-one and no PATCH — rotation is delete-and-register.
		r.Post("/webhooks", h.createWebhook)
		r.Get("/webhooks", h.listWebhooks)
		r.Delete("/webhooks/{id}", h.deleteWebhook)
	})
}
