package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// previewEntry is one entry with a working copy that DIFFERS from its published
// snapshot in every field the audiences disagree about. A fixture where the two
// copies matched would pass all three tests below while proving nothing.
func previewEntry() (*domain.ContentType, *domain.Entry) {
	ct := &domain.ContentType{Name: "course"}
	actor := uuid.New()
	// The provenance pair is populated because the fields are `omitempty`: a
	// fixture that leaves them zero makes every "did this key reach the wire"
	// assertion below silently stop covering them.
	agentKind, botName := domain.ActorKindAgent, "content-bot"
	e := &domain.Entry{
		ID:                    uuid.New(),
		Status:                domain.StatusPublished,
		Payload:               json.RawMessage(`{"title":"draft title"}`),
		PublishedPayload:      json.RawMessage(`{"title":"live title"}`),
		Version:               7,
		PublishedVersion:      3,
		UpdatedAt:             time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC),
		HasUnpublishedChanges: true,
		CreatedBy:             &actor,
		UpdatedBy:             &actor,
		PublishedBy:           &actor,
		CreatedByKind:         domain.ActorKindHuman,
		UpdatedByKind:         &agentKind,
		UpdatedByAgent:        &botName,
	}
	return ct, e
}

func previewSubject(entryID uuid.UUID) authn.Subject {
	return authn.Subject{
		UserID:     uuid.New(),
		TenantID:   "t1",
		TenantRole: "viewer",
		// Both, always: preview is delivery narrowed to one entry, never a
		// separate plane. See authn.Subject.PreviewEntryID.
		PublicDelivery: true,
		PreviewEntryID: &entryID,
	}
}

// The point of the whole audience: preview shows the WORKING copy, delivery
// shows the snapshot, and every field describing the data follows the copy it
// describes (ADR-006's generalised rule). Asserting on marshalled JSON rather
// than struct fields because MarshalJSON is the real gate — a DTO that looked
// right in memory and wrong on the wire is precisely the OD2-023 F2 failure.
func TestPreview_ServesWorkingCopyWithItsOwnDescriptors(t *testing.T) {
	ct, e := previewEntry()

	raw, err := json.Marshal(ProjectEntry(ct, e, previewSubject(e.ID)))
	if err != nil {
		t.Fatalf("marshal preview: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal preview: %v", err)
	}

	data, _ := json.Marshal(got["data"])
	if string(data) != `{"title":"draft title"}` {
		t.Fatalf("data = %s, want the working copy — preview served the snapshot", data)
	}
	if got["version"] != float64(7) {
		t.Fatalf("version = %v, want 7 (the working copy's) — a descriptor tracking the other copy", got["version"])
	}
	// Kept, unlike delivery: here it describes the copy actually being shown, so
	// dropping it would leave "how fresh is this preview" unanswerable.
	if _, ok := got["updated_at"]; !ok {
		t.Fatal("updated_at missing: preview shows the working copy, so its mtime is the honest answer")
	}
}

// MarshalJSON is the SECOND clearing of the admin-only fields, and this is the
// only test that can fail when it breaks.
//
// The projector never sets those fields on a preview DTO — it returns before the
// admin block — so a test that goes through ProjectEntry passes whether or not
// MarshalJSON does its job. (Verified by mutation: deleting the whole preview
// case from MarshalJSON left every ProjectEntry-based test green.) The gate
// exists for DTOs that are touched between projection and response, so the test
// for it has to build that DTO directly. Same package, so it can.
func TestPreview_MarshalClearsAdminFieldsSetAfterProjection(t *testing.T) {
	actor := uuid.New()
	agentKind, botName := domain.ActorKindAgent, "content-bot"
	dto := EntryDTO{
		ID:                    uuid.New(),
		Type:                  "course",
		Data:                  json.RawMessage(`{"title":"draft title"}`),
		Version:               7,
		HasUnpublishedChanges: true,
		CreatedBy:             &actor,
		UpdatedBy:             &actor,
		PublishedBy:           &actor,
		// The ADR-014 §4 pair belongs in this list for the same reason the three
		// above do, and it is worth naming what it adds: the agent id is a
		// tenant's private automation topology, so leaking it to a preview link
		// tells an outside reviewer which bots that tenant runs.
		CreatedByKind:  domain.ActorKindHuman,
		UpdatedByKind:  &agentKind,
		UpdatedByAgent: &botName,
		// The ADR-014 §6 diff material. published_data is the LIVE text, which a
		// reviewer holding a preview link is being shown the unreleased version
		// of on purpose — handing them the released one alongside turns a preview
		// into an editorial diff. has_hidden_changes is editing state of the same
		// class as has_unpublished_changes.
		PublishedData:    json.RawMessage(`{"title":"live title"}`),
		HasHiddenChanges: true,
		aud:              audiencePreview,
	}

	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{
		"has_unpublished_changes", "created_by", "updated_by", "published_by",
		"created_by_kind", "created_by_agent", "updated_by_kind", "updated_by_agent",
		"published_data", "has_hidden_changes",
	} {
		if v, present := got[k]; present {
			t.Errorf("%s survived marshalling (%v): the backstop against post-projection mutation is gone", k, v)
		}
	}
	// The working copy itself must still be there — a backstop that also blanked
	// the data would "pass" this test while serving an empty preview.
	if data, _ := json.Marshal(got["data"]); string(data) != `{"title":"draft title"}` {
		t.Fatalf("data = %s, want the working copy", data)
	}
}

// The PROJECTOR's half: preview returns before the admin block, so those fields
// are never set in the first place.
//
// This asserts on the struct, NOT on marshalled JSON, and that is the only way
// it can fail. Verified by mutation: deleting the preview branch from
// ProjectEntry — which drops preview into the admin block — leaves every
// JSON-level test in this file green, because MarshalJSON clears the same four
// fields on the way out. The two layers are genuinely redundant today. They stop
// being redundant the moment someone adds an admin-only field to the projector
// and not to MarshalJSON's list, or the reverse, which is exactly the pair of
// mistakes ADR-006 says to expect (OD2-023 F2 was one of them).
func TestPreview_ProjectorDoesNotSetAdminFields(t *testing.T) {
	ct, e := previewEntry()

	dto := ProjectEntry(ct, e, previewSubject(e.ID))

	if dto.HasUnpublishedChanges {
		t.Error("projector set has_unpublished_changes on a preview DTO — it fell through to the admin block")
	}
	if dto.CreatedBy != nil || dto.UpdatedBy != nil || dto.PublishedBy != nil {
		t.Error("projector set actor ids on a preview DTO — it fell through to the admin block")
	}
	if dto.CreatedByKind != "" || dto.CreatedByAgent != nil || dto.UpdatedByKind != nil || dto.UpdatedByAgent != nil {
		t.Error("projector set write provenance on a preview DTO — it fell through to the admin block")
	}
	if len(dto.PublishedData) != 0 || dto.HasHiddenChanges {
		t.Error("projector set diff material on a preview DTO — it fell through to the admin block")
	}
	// Still the working copy: a preview branch that returned an empty DTO would
	// satisfy every assertion above.
	if string(dto.Data) != `{"title":"draft title"}` {
		t.Fatalf("data = %s, want the working copy", dto.Data)
	}
}

// A preview link leaves the platform. Editing state and internal user ids are
// not the reviewer's to see. This is the end-to-end statement of that property
// on the wire; the two tests above are what pin each layer that enforces it.
func TestPreview_WithholdsEditingStateAndActors(t *testing.T) {
	ct, e := previewEntry()

	raw, err := json.Marshal(ProjectEntry(ct, e, previewSubject(e.ID)))
	if err != nil {
		t.Fatalf("marshal preview: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal preview: %v", err)
	}

	for _, k := range []string{
		"has_unpublished_changes", "created_by", "updated_by", "published_by",
		"created_by_kind", "created_by_agent", "updated_by_kind", "updated_by_agent",
	} {
		if v, present := got[k]; present {
			t.Errorf("%s present (%v): a preview link goes to people outside the platform", k, v)
		}
	}
}

// audienceFor nests the preview test INSIDE PublicDelivery. A PreviewEntryID on
// a subject that is not a delivery credential is a malformed subject, not a
// preview — and reading it as one would hand the working copy to whoever set the
// field. It must fall through to the admin audience, where the caller's own
// permissions decide as they always did.
func TestPreview_EntryIDWithoutDeliveryIsNotPreview(t *testing.T) {
	ct, e := previewEntry()
	id := e.ID
	sub := authn.Subject{
		UserID:     uuid.New(),
		TenantID:   "t1",
		TenantRole: "owner",
		// No PublicDelivery — this is an ordinary admin subject that somehow
		// acquired a preview scope.
		PreviewEntryID: &id,
	}

	if aud := audienceFor(sub); aud != audienceAdmin {
		t.Fatalf("audienceFor = %d, want audienceAdmin (%d): a preview scope must not by itself select the preview audience", aud, audienceAdmin)
	}
	// And the resulting DTO is a full admin view, not a half-preview.
	raw, err := json.Marshal(ProjectEntry(ct, e, sub))
	if err != nil {
		t.Fatalf("marshal admin: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal admin: %v", err)
	}
	if _, ok := got["has_unpublished_changes"]; !ok {
		t.Fatal("admin audience lost has_unpublished_changes — the fall-through did not land on admin")
	}
}

// The delivery audience must be untouched by preview's arrival: same entry, no
// preview scope, still the snapshot and still without the admin-only fields.
// Without this, a regression that made preview the default for every delivery
// credential would pass the two tests above.
func TestPreview_DeliveryAudienceUnchanged(t *testing.T) {
	ct, e := previewEntry()
	sub := authn.Subject{UserID: uuid.New(), TenantID: "t1", TenantRole: "viewer", PublicDelivery: true}

	raw, err := json.Marshal(ProjectEntry(ct, e, sub))
	if err != nil {
		t.Fatalf("marshal delivery: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal delivery: %v", err)
	}

	data, _ := json.Marshal(got["data"])
	if string(data) != `{"title":"live title"}` {
		t.Fatalf("data = %s, want the published snapshot", data)
	}
	if got["version"] != float64(3) {
		t.Fatalf("version = %v, want 3 (published_version)", got["version"])
	}
	if _, ok := got["updated_at"]; ok {
		t.Fatal("updated_at present on delivery: Amendment 1a removed it")
	}
}

// --- preview credential at the read paths -----------------------------------
//
// Everything above tests the PROJECTION: given a preview subject, which copy
// does a DTO render. That is one layer. These test the SCOPE: which rows the
// credential may reach at all. A projection that is perfect on the entry the
// token names is worth nothing if the token also reaches every other entry,
// and the projection tests cannot see that — they are handed the row.

// ctxPreview mints a preview credential for one entry. Role "editor" on purpose,
// mirroring ctxDelivery: the scope has to hold on the credential alone, not
// because the role happened to be read-only.
func ctxPreview(tenant string, entryID uuid.UUID) context.Context {
	return authn.WithSubject(context.Background(), authn.Subject{
		UserID:         uuid.New(),
		TenantID:       tenant,
		TenantRole:     "editor",
		PublicDelivery: true,
		PreviewEntryID: &entryID,
	})
}

// The feature itself: the entry the token names is served from its WORKING
// copy, and the fact that it has never been published does not refuse it. A
// delivery credential 404s this same row — that contrast is the test.
func TestPreview_ServesTheUnpublishedEntryItNames(t *testing.T) {
	svc, _ := newSvc()
	admin := ctxTenant("t1")
	e := seedEntry(t, svc, admin)

	got, err := svc.GetEntry(ctxPreview("t1", e.ID), "order", e.ID)
	if err != nil {
		t.Fatalf("preview get: %v — a preview token must reach the draft it was minted for", err)
	}
	if string(got.Data) != `{"title":"a"}` {
		t.Fatalf("data = %s, want the working copy", got.Data)
	}

	if _, err := svc.GetEntry(ctxDelivery("t1"), "order", e.ID); err != apperrors.ErrNotFound {
		t.Fatalf("plain delivery on the same draft: %v, want ErrNotFound", err)
	}
}

// The scope check. audienceFor answers audiencePreview off the PRESENCE of the
// id, never a comparison — so if GetEntry does not compare, a token minted for
// one draft serves the working copy of every draft in the tenant. Remove the
// `e.ID != *sub.PreviewEntryID` branch and this is the test that turns red.
func TestPreview_TokenForOneEntryCannotReadAnother(t *testing.T) {
	svc, _ := newSvc()
	admin := ctxTenant("t1")
	named := seedEntry(t, svc, admin)
	other, err := svc.CreateEntry(admin, "order", json.RawMessage(`{"title":"someone else's draft"}`))
	if err != nil {
		t.Fatalf("create second entry: %v", err)
	}

	// 404, not 403: a distinguishable refusal would confirm the id exists, and a
	// preview token is the delivery credential most likely to be probed.
	if _, err := svc.GetEntry(ctxPreview("t1", named.ID), "order", other.ID); err != apperrors.ErrNotFound {
		t.Fatalf("preview reached an entry it was not minted for: %v, want ErrNotFound", err)
	}
}

// A preview token is the one delivery credential that leaves the platform's own
// edge, so the collection paths — which all call delivery.Record — must refuse
// it rather than let an outsider burn the tenant's metered quota.
func TestPreview_CollectionReadsRefused(t *testing.T) {
	svc, _ := newSvc()
	admin := ctxTenant("t1")
	e := seedEntry(t, svc, admin)
	ctx := ctxPreview("t1", e.ID)

	cases := map[string]func() error{
		"ListEntries":      func() error { _, err := svc.ListEntries(ctx, "order", ListEntriesInput{}); return err },
		"ListTranslations": func() error { _, err := svc.ListTranslations(ctx, "order", e.ID); return err },
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			err := call()
			appErr, ok := err.(*apperrors.AppError)
			if !ok || appErr.Code != "CONTENT_PREVIEW_SCOPE_EXCEEDED" {
				t.Fatalf("got %v, want CONTENT_PREVIEW_SCOPE_EXCEEDED", err)
			}
			if appErr.HTTPStatus != 403 {
				t.Fatalf("status = %d, want 403", appErr.HTTPStatus)
			}
		})
	}

	// The same two paths still work for a plain delivery credential — otherwise
	// the refusal above could be a blanket break rather than a preview-only one.
	if _, err := svc.ListEntries(ctxDelivery("t1"), "order", ListEntriesInput{}); err != nil {
		t.Fatalf("plain delivery list broke: %v", err)
	}
}
