package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
)

// ctxUser pins the acting user, which ctxTenant deliberately does not — it mints
// a fresh uuid per call, so "who did this" is unassertable through it.
func ctxUser(tenant string, id uuid.UUID) context.Context {
	return authn.WithSubject(context.Background(), authn.Subject{
		UserID:   id,
		TenantID: tenant,
		Roles:    []string{"member"},
	})
}

func TestCreateEntry_RecordsCreatorAndEditor(t *testing.T) {
	svc, _ := newSvc()
	u1 := uuid.New()
	ctx := ctxUser("tenant-a", u1)
	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)

	created, err := svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{"title": "T1"}))
	require.NoError(t, err)

	// A new entry's last editor IS its author — the same reason created_at and
	// updated_at both get `now`.
	require.NotNil(t, created.CreatedBy)
	require.NotNil(t, created.UpdatedBy)
	assert.Equal(t, u1, *created.CreatedBy)
	assert.Equal(t, u1, *created.UpdatedBy)
}

func TestUpdateEntry_MovesUpdatedByNotCreatedBy(t *testing.T) {
	svc, _ := newSvc()
	u1, u2 := uuid.New(), uuid.New()
	ctx1 := ctxUser("tenant-a", u1)
	_, err := svc.CreateContentType(ctx1, orderTypeInput())
	require.NoError(t, err)
	created, err := svc.CreateEntry(ctx1, "order", mustJSON(t, map[string]any{"title": "T1"}))
	require.NoError(t, err)

	updated, err := svc.UpdateEntry(ctxUser("tenant-a", u2), "order", created.ID,
		mustJSON(t, map[string]any{"title": "T2"}), 0)
	require.NoError(t, err)

	require.NotNil(t, updated.CreatedBy)
	require.NotNil(t, updated.UpdatedBy)
	assert.Equal(t, u1, *updated.CreatedBy, "the author does not change when someone else edits")
	assert.Equal(t, u2, *updated.UpdatedBy)
}

// TestPublish_RecordsPublisher_RetractKeepsIt. The name is the assertion: since
// ADR-014 §5.1 a retract keeps the snapshot, so it keeps the snapshot's releaser
// too. Renamed from UnpublishClearsIt, which described the pre-§5.1 rule.
func TestPublish_RecordsPublisher_RetractKeepsIt(t *testing.T) {
	svc, _ := newSvc()
	u1, u2 := uuid.New(), uuid.New()
	ctx1 := ctxUser("tenant-a", u1)
	_, err := svc.CreateContentType(ctx1, orderTypeInput())
	require.NoError(t, err)
	created, err := svc.CreateEntry(ctx1, "order", mustJSON(t, map[string]any{"title": "T1"}))
	require.NoError(t, err)

	published, err := svc.SetEntryStatus(ctxUser("tenant-a", u2), "order", created.ID, domain.StatusPublished, 0)
	require.NoError(t, err)
	// Guard: assert it was actually set before asserting it gets cleared, or the
	// clearing assertion would pass against a field nobody ever wrote.
	require.NotNil(t, published.PublishedBy, "publisher must be recorded before we can test clearing it")
	assert.Equal(t, u2, *published.PublishedBy)

	retracted, err := svc.SetEntryStatus(ctxUser("tenant-a", u1), "order", created.ID, domain.StatusDraft, 0)
	require.NoError(t, err)
	// ADR-014 §5.1: the snapshot outlives the retract, so the column that
	// DESCRIBES the snapshot outlives it too. Clearing published_by alone would
	// leave the three snapshot columns describing different copies, which is
	// what ADR-006's standing rule forbids.
	//
	// This assertion used to be `Nil` — see the test's old name, UnpublishClearsIt
	// — and it stayed green for two steps after §5.1 shipped because memRepo was
	// nulling the column that the SQL had stopped nulling. The integration suite
	// had the right answer the whole time (authorship_integration_test.go), so
	// the two layers disagreed and nothing said so.
	require.NotNil(t, retracted.PublishedBy,
		"the snapshot survives a retract, so the actor who released it still describes something")
	assert.Equal(t, u2, *retracted.PublishedBy,
		"the releaser is whoever put it live, NOT whoever took it down")
	require.NotNil(t, retracted.UpdatedBy)
	assert.Equal(t, u1, *retracted.UpdatedBy, "an unpublish very much has an editor")
}

func TestRepublishIdenticalContent_KeepsOriginalPublisher(t *testing.T) {
	svc, _ := newSvc()
	u1, u2 := uuid.New(), uuid.New()
	ctx1 := ctxUser("tenant-a", u1)
	_, err := svc.CreateContentType(ctx1, orderTypeInput())
	require.NoError(t, err)
	created, err := svc.CreateEntry(ctx1, "order", mustJSON(t, map[string]any{"title": "T1"}))
	require.NoError(t, err)
	published, err := svc.SetEntryStatus(ctx1, "order", created.ID, domain.StatusPublished, 0)
	require.NoError(t, err)
	require.NotNil(t, published.PublishedBy)

	// Pressing publish on unchanged content short-circuits: it is not a new
	// release, so the original publisher stands.
	again, err := svc.SetEntryStatus(ctxUser("tenant-a", u2), "order", created.ID, domain.StatusPublished, 0)
	require.NoError(t, err)
	require.NotNil(t, again.PublishedBy)
	assert.Equal(t, u1, *again.PublishedBy, "republishing identical content is not a new release")
}

// TestDelivery_EntryDTOCarriesNoAuthorship is the OD2-025 proof, written as a
// key-set allowlist rather than three absence assertions: it fails on ANY future
// EntryDTO field that reaches delivery, which is the class of bug that rule
// exists to prevent, and is cheaper than one test per field.
func TestDelivery_EntryDTOCarriesNoAuthorship(t *testing.T) {
	u := uuid.New()
	agentKind, botName := domain.ActorKindAgent, "content-bot"
	published := &domain.Entry{
		ID: uuid.New(),
		// `salary` differs between the copies and is unreadable to the admin
		// subject below, which is what makes has_hidden_changes come out TRUE for
		// the admin view. That is the same load-bearing reason the provenance
		// columns are all populated: the field is `omitempty`, so a fixture that
		// leaves it false emits no key and this allowlist would go on passing
		// while delivery leaked it.
		Payload:          json.RawMessage(`{"title":"working","salary":120000}`),
		PublishedPayload: json.RawMessage(`{"title":"live","salary":100000}`),
		Version:          5,
		PublishedVersion: 4,
		Status:           domain.StatusPublished,
		Locale:           domain.DefaultLocale,
		CreatedBy:        &u,
		UpdatedBy:        &u,
		PublishedBy:      &u,
		// EVERY provenance column carries a value, and that is load-bearing rather
		// than thorough. These fields are `omitempty`, so an unset one is absent
		// from the delivery JSON no matter what the projector does with it — this
		// allowlist would have gone on passing while a leak sat in the code. It did
		// exactly that when the ADR-014 §4 pair was added: the fixture predated the
		// columns, so removing their line from MarshalJSON's delivery case changed
		// nothing here. A key-set allowlist only guards keys the fixture can emit.
		CreatedByKind:  domain.ActorKindHuman,
		CreatedByAgent: nil,
		UpdatedByKind:  &agentKind,
		UpdatedByAgent: &botName,
	}
	// One field only the owner may read — so the "admin" subject below has a
	// hidden key, and the two ADR-014 §6 fields are both populated in the admin
	// view.
	ct := &domain.ContentType{Name: "order", Fields: []domain.Field{
		{Key: "title", Type: domain.FieldTypeString},
		{Key: "salary", Type: domain.FieldTypeNumber, ReadRoles: []string{"owner"}},
	}}

	// Guard first: if the admin view does not carry them either, the delivery
	// assertion below proves nothing.
	adminKeys := topLevelKeys(t, ProjectEntry(ct, published, adminSubject()))
	for _, k := range []string{"created_by", "updated_by", "published_by", "created_by_kind", "updated_by_kind", "updated_by_agent", "published_data", "has_hidden_changes"} {
		require.Contains(t, adminKeys, k, "admin audience must carry %s or the delivery check is vacuous", k)
	}

	deliveryKeys := []string{"id", "type", "data", "version", "status", "locale", "translation_group_id", "created_at"}
	assert.ElementsMatch(t, deliveryKeys,
		topLevelKeys(t, ProjectEntry(ct, published, deliverySubject())),
		"delivery must expose exactly this key set — a new field here needs an audience decision, not a default",
	)

	// Preview shares the admin-only list with delivery except for updated_at, and
	// MarshalJSON spells the two cases out separately rather than falling through.
	// Two hand-maintained lists is exactly the arrangement where one gets a new
	// field and the other does not, so both are asserted here.
	assert.ElementsMatch(t, append(deliveryKeys, "updated_at"),
		topLevelKeys(t, ProjectEntry(ct, published, previewSubject(published.ID))),
		"a preview link goes to people outside the platform: internal writers are not theirs to see",
	)
}

// adminSubject and deliverySubject name the two credentials the projection
// distinguishes. Tests construct DTOs the way production does — via the
// projector — because a hand-built literal now carries no audience and refuses
// to marshal, which is the property under test in
// TestProjection_UnprojectedDTOsRefuseToMarshal.
func adminSubject() authn.Subject {
	return authn.Subject{UserID: uuid.New(), TenantID: "t", TenantRole: "admin"}
}

func deliverySubject() authn.Subject {
	return authn.Subject{UserID: uuid.New(), TenantID: "t", PublicDelivery: true}
}

// TestProjection_UnprojectedDTOsRefuseToMarshal pins the fail-closed default.
//
// The zero audience used to BE the admin audience, so any DTO that reached a
// wire without going through a projector served the working copy — the one
// holding unreleased edits. The zero value is now "no decision made", and no
// decision must never render.
func TestProjection_UnprojectedDTOsRefuseToMarshal(t *testing.T) {
	_, err := json.Marshal(EntryDTO{ID: uuid.New(), Type: "order", Data: json.RawMessage(`{"secret":"draft"}`)})
	require.Error(t, err, "an EntryDTO built as a literal must not render")
	assert.Contains(t, err.Error(), "no audience")

	_, err = json.Marshal(MediaAssetDTO{ID: uuid.New()})
	require.Error(t, err, "a MediaAssetDTO built as a literal must not render")
	assert.Contains(t, err.Error(), "no audience")

	// And the refusal survives being nested, which is how a DTO actually
	// reaches a response — inside a list, inside an envelope.
	_, err = json.Marshal(EntryListResult{Items: []EntryDTO{{ID: uuid.New()}}})
	require.Error(t, err, "an unprojected DTO must not be laundered by a wrapper")
}

// TestProjection_DeliveryStripsAdminFieldsAtMarshalTime is the backstop for the
// OD2-023 F2 shape: an admin-only field assigned outside the projector, on a
// DTO that has already been decided to be a delivery view. The constructor is
// not the last word — serialisation is.
func TestProjection_DeliveryStripsAdminFieldsAtMarshalTime(t *testing.T) {
	u := uuid.New()
	now := time.Now().UTC()
	dto := ProjectEntry(&domain.ContentType{Name: "order"}, &domain.Entry{
		ID:               uuid.New(),
		PublishedPayload: json.RawMessage(`{"title":"live"}`),
		Status:           domain.StatusPublished,
		Locale:           domain.DefaultLocale,
	}, deliverySubject())

	// Someone downstream sets them anyway. This is the mutation that used to
	// reach the wire.
	dto.CreatedBy, dto.UpdatedBy, dto.PublishedBy = &u, &u, &u
	dto.HasUnpublishedChanges = true
	dto.UpdatedAt = &now

	keys := topLevelKeys(t, dto)
	for _, k := range []string{"created_by", "updated_by", "published_by", "has_unpublished_changes", "updated_at"} {
		assert.NotContains(t, keys, k, "delivery must not carry %s even when it was assigned after projection", k)
	}
}

func topLevelKeys(t *testing.T, dto EntryDTO) []string {
	t.Helper()
	b, err := json.Marshal(dto)
	require.NoError(t, err)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &m))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestActor_ReturnsNilForNonHumanSubject(t *testing.T) {
	human := uuid.New()
	for _, tc := range []struct {
		name string
		sub  authn.Subject
		want *uuid.UUID
	}{
		{"delivery credential", authn.Subject{UserID: uuid.New(), PublicDelivery: true}, nil},
		{"zero uuid from dev headers", authn.Subject{UserID: uuid.Nil}, nil},
		{"a person", authn.Subject{UserID: human}, &human},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := actor(tc.sub)
			if tc.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, *tc.want, *got)
		})
	}
}
