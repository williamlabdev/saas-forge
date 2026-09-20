package service

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
)

// The entry diff (ADR-014 §6) is the material a person reads before releasing
// someone else's edits. Everything here is about that reader being told the
// truth: not a change that did not happen, not a value they may not see, and
// not silence where a change they may not see did happen.

// payrollType is a collection with one field the owner alone may read — the
// example ADR-009 uses, and the shape that makes "writable to me, unreadable to
// me" constructible. `title` is deliberately unrestricted so every assertion
// below has a visible field to contrast against.
func payrollType() *domain.ContentType {
	return &domain.ContentType{
		Name: "payroll",
		Fields: []domain.Field{
			{Key: "title", Type: domain.FieldTypeString},
			{Key: "salary", Type: domain.FieldTypeNumber, ReadRoles: []string{"owner"}},
		},
	}
}

// wireOf renders the DTO the way a response does and returns the raw top-level
// JSON. Assertions run against these bytes rather than against the struct
// because the struct is not what leaves the process: a value that survives into
// the payload has already been sent whether or not a client draws it
// (verification plan item 10).
func wireOf(t *testing.T, dto EntryDTO) map[string]json.RawMessage {
	t.Helper()
	b, err := json.Marshal(dto)
	require.NoError(t, err)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &m))
	return m
}

func roleSubject(role string) authn.Subject {
	return authn.Subject{UserID: uuid.New(), TenantID: "t", TenantRole: role}
}

// TestEntryDiff_RestrictedFieldValueNeverReachesTheWire is verification plan
// item 10.
//
// Field-level read permission lives in the projector and nowhere else — the
// repository selects whole payloads and the SQL layer has no field filter — so a
// diff assembled from the two raw columns would hand the reader exactly the
// values ADR-009 refused them. The snapshot is the new column here, and it is
// the one nothing else was masking.
func TestEntryDiff_RestrictedFieldValueNeverReachesTheWire(t *testing.T) {
	e := &domain.Entry{
		ID:               uuid.New(),
		Status:           domain.StatusPublished,
		Payload:          json.RawMessage(`{"title":"working","salary":120000}`),
		PublishedPayload: json.RawMessage(`{"title":"live","salary":100000}`),
	}

	// Control group FIRST: if the owner cannot see the field either, the
	// restricted-role assertion below is satisfied by a diff that returns no
	// fields at all, and would pass against an implementation that shipped
	// nothing.
	ownerWire := wireOf(t, ProjectEntry(payrollType(), e, roleSubject("owner")))
	require.Contains(t, string(ownerWire["data"]), "120000",
		"owner must see the working copy's restricted value or the editor check below is vacuous")
	require.Contains(t, string(ownerWire["published_data"]), "100000",
		"owner must see the snapshot's restricted value or the editor check below is vacuous")

	// The editor may write this type but may not read this field.
	editorWire := wireOf(t, ProjectEntry(payrollType(), e, roleSubject("editor")))
	assert.NotContains(t, string(editorWire["published_data"]), "100000",
		"the snapshot side of a diff must go through the same mask as the working copy")
	assert.NotContains(t, string(editorWire["published_data"]), "salary",
		"masking the value but keeping the key still tells them the field exists and changed")
	// The working copy has been masked since ADR-009; asserted here so a
	// regression on either side fails in the test that owns the diff.
	assert.NotContains(t, string(editorWire["data"]), "120000")
	// And what they ARE entitled to still arrives — a mask that removed
	// everything would pass every assertion above.
	assert.Contains(t, string(editorWire["published_data"]), "live")
	assert.Contains(t, string(editorWire["data"]), "working")
}

// TestEntryDiff_HiddenChangeIsAnnouncedNotOmitted is verification plan item 11.
//
// Masking both sides is not enough by itself. With the keys simply gone, a
// change confined to them renders as "nothing changed" — a stronger claim than
// "nothing you may see changed", and a false one. The person about to press
// publish would be endorsing edits the interface told them did not exist.
func TestEntryDiff_HiddenChangeIsAnnouncedNotOmitted(t *testing.T) {
	// ONLY the restricted field differs. The visible field is byte-identical on
	// both sides, so after masking the two documents are equal and the flag is
	// the only thing left that can carry the truth.
	e := &domain.Entry{
		ID:               uuid.New(),
		Status:           domain.StatusPublished,
		Payload:          json.RawMessage(`{"title":"same","salary":120000}`),
		PublishedPayload: json.RawMessage(`{"title":"same","salary":100000}`),
	}

	editor := wireOf(t, ProjectEntry(payrollType(), e, roleSubject("editor")))
	assert.JSONEq(t, string(editor["data"]), string(editor["published_data"]),
		"precondition: after masking these two must be equal, or the flag is not what is under test")
	assert.Equal(t, "true", string(editor["has_hidden_changes"]),
		"a change confined to fields this reader may not see must be announced, not rendered as no change")

	// The owner sees the change itself, so there is nothing to announce. Without
	// this, a flag hardcoded to true would pass the assertion above.
	owner := wireOf(t, ProjectEntry(payrollType(), e, roleSubject("owner")))
	_, present := owner["has_hidden_changes"]
	assert.False(t, present,
		"nothing is hidden from the owner, so there is no unseen change to warn about")
	assert.NotEqual(t, string(owner["data"]), string(owner["published_data"]),
		"the owner's own diff must still show the field that changed")
}

// The flag answers a question about the keys, not about the document: two
// copies that differ only in a field the reader CAN see need no warning, or the
// warning is permanently lit and stops meaning anything.
func TestEntryDiff_VisibleChangeRaisesNoHiddenWarning(t *testing.T) {
	e := &domain.Entry{
		ID:               uuid.New(),
		Status:           domain.StatusPublished,
		Payload:          json.RawMessage(`{"title":"working","salary":100000}`),
		PublishedPayload: json.RawMessage(`{"title":"live","salary":100000}`),
	}
	wire := wireOf(t, ProjectEntry(payrollType(), e, roleSubject("editor")))
	_, present := wire["has_hidden_changes"]
	assert.False(t, present, "the restricted field is identical on both sides — nothing to warn about")
	assert.NotEqual(t, string(wire["data"]), string(wire["published_data"]),
		"the visible change must still be visible")
}

// TestEntryDiff_IdenticalCopiesShowNoDiff is verification plan item 4, the
// reverse assertion. Without it, an implementation that reports a difference
// unconditionally passes every other test in this file: the "changed" signal
// would be lit forever and nobody would notice.
func TestEntryDiff_IdenticalCopiesShowNoDiff(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name:   "article",
		Fields: []FieldInput{{Key: "title", Type: domain.FieldTypeString, Required: true}},
	})
	require.NoError(t, err)
	created, err := svc.CreateEntry(owner, "article", mustJSON(t, map[string]any{"title": "first"}))
	require.NoError(t, err)

	// Before publication there is no snapshot at all. That is NOT "no changes":
	// an absent published_data is what tells the console to say "not published
	// yet" instead of showing an empty diff (ADR-014 §6).
	draft := wireOf(t, created)
	_, present := draft["published_data"]
	assert.False(t, present, "a draft has no live version, and an empty diff would claim it matches one")

	published, err := svc.SetEntryStatus(owner, "article", created.ID, domain.StatusPublished, created.Version)
	require.NoError(t, err)

	wire := wireOf(t, published)
	require.Contains(t, wire, "published_data", "a published entry must carry the snapshot to diff against")
	assert.JSONEq(t, string(wire["data"]), string(wire["published_data"]),
		"nothing was edited after publishing, so the two copies must be identical and the console must show no diff")
	_, changed := wire["has_unpublished_changes"]
	assert.False(t, changed, "and the existing headline flag must agree with the diff material")

	// Now edit, and the same two fields must disagree — otherwise the assertion
	// above is being satisfied by a snapshot that simply mirrors the working
	// copy.
	edited, err := svc.UpdateEntry(owner, "article", created.ID, mustJSON(t, map[string]any{"title": "second"}), published.Version)
	require.NoError(t, err)
	editedWire := wireOf(t, edited)
	assert.NotEqual(t, string(editedWire["data"]), string(editedWire["published_data"]),
		"an edit after publication is exactly what the diff exists to show")
	assert.Contains(t, string(editedWire["published_data"]), "first",
		"the snapshot must still hold the released text, not the new one")
}

// TestEntryDiff_MarshalClearsSnapshotSetAfterProjection pins the SECOND of the
// two layers that keep the snapshot off a delivery wire.
//
// Both layers exist: ProjectEntry's delivery branch returns before the admin
// block, so the field is never assigned, and MarshalJSON clears it again on the
// way out. They are redundant today, and that redundancy is why this test has
// to be white-box — verified by mutation: deleting MarshalJSON's clearing line
// leaves TestDelivery_EntryDTOCarriesNoAuthorship green, because the projector
// already never set it. A black-box test can only ever bite the first layer.
//
// The scenario it stands in for is the one MarshalJSON's own comment names:
// code between projection and response that sets a field, written by someone
// who never read ProjectEntry. That is not hypothetical here — OD2-023 F2
// shipped exactly that way.
func TestEntryDiff_MarshalClearsSnapshotSetAfterProjection(t *testing.T) {
	e := &domain.Entry{
		ID:               uuid.New(),
		Status:           domain.StatusPublished,
		Payload:          json.RawMessage(`{"title":"working"}`),
		PublishedPayload: json.RawMessage(`{"title":"live"}`),
	}
	ct := &domain.ContentType{Name: "article", Fields: []domain.Field{{Key: "title", Type: domain.FieldTypeString}}}

	dto := ProjectEntry(ct, e, deliverySubject())
	// Stand in for later code assigning them. Same package, so it can — which is
	// the only way to reach the second layer with the first one intact.
	dto.PublishedData = e.PublishedPayload
	dto.HasHiddenChanges = true

	wire := wireOf(t, dto)
	for _, k := range []string{"published_data", "has_hidden_changes"} {
		_, present := wire[k]
		assert.False(t, present, "%s survived marshalling: the delivery backstop is gone", k)
	}
	// The snapshot must still be served as `data` — a backstop that blanked
	// everything would satisfy the loop above while serving an empty document.
	assert.JSONEq(t, `{"title":"live"}`, string(wire["data"]))
}

// The masking and the caller's own ?fields= narrowing both apply to the
// snapshot, and they have to apply to BOTH sides. Narrowing only the working
// copy would render every key outside the selection as "removed by this edit".
func TestEntryDiff_ProjectionNarrowsBothSidesAlike(t *testing.T) {
	e := &domain.Entry{
		ID:               uuid.New(),
		Status:           domain.StatusPublished,
		Payload:          json.RawMessage(`{"title":"working","body":"w"}`),
		PublishedPayload: json.RawMessage(`{"title":"live","body":"p"}`),
	}
	ct := &domain.ContentType{Name: "article", Fields: []domain.Field{
		{Key: "title", Type: domain.FieldTypeString},
		{Key: "body", Type: domain.FieldTypeString},
	}}
	wire := wireOf(t, ProjectEntry(ct, e, roleSubject("owner")).narrowedTo([]string{"title"}))
	assert.JSONEq(t, `{"title":"working"}`, string(wire["data"]))
	assert.JSONEq(t, `{"title":"live"}`, string(wire["published_data"]),
		"a snapshot that kept the unselected keys would read as an edit that added them")
}
