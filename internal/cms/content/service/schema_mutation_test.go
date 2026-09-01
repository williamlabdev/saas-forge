package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Schema mutation, service layer: REFUSALS ONLY.
//
// Every guard asserted here fires BEFORE the corresponding repository write, so
// none of these depend on memRepo faithfully reproducing JSONB semantics — which
// it explicitly does not (see the comment above its schema-mutation block). What
// the data actually does after a guard passes is asserted in
// repository/schema_mutation_integration_test.go against real Postgres.
//
// The refusals are asserted by CODE, never by "an error came back". ADR-006
// Amendment 1b spent a whole ruling closing the generic-400 pattern: a caller
// that gets "invalid request body" cannot tell which of the keys it sent was
// refused, and a test that only checks `err != nil` cannot tell the two apart
// either — it passes just as happily against the pattern the amendment banned.

func ptrTo[T any](v T) *T { return &v }

// mustCode asserts the error is an AppError with exactly this code and status.
// Both, not either: CONTENT_FIELD_HAS_DATA rendered as a 422 would tell a client
// to change its request when the correct move is to re-send it with ?force=true.
func mustCode(t *testing.T, err error, code string, status int) *apperrors.AppError {
	t.Helper()
	require.Error(t, err)
	ae, ok := apperrors.As(err)
	require.True(t, ok, "not an AppError: %v", err)
	assert.Equal(t, code, ae.Code)
	assert.Equal(t, status, ae.HTTPStatus, "code %s carried the wrong HTTP status", ae.Code)
	return ae
}

// articleTypeInput is the fixture for this file: one required string, one free
// field, one enum. Deliberately not orderTypeInput — these tests delete and
// rename fields, and sharing a fixture with the entry tests would make every
// mutation here a change to their setup too.
func articleTypeInput() CreateTypeInput {
	return CreateTypeInput{
		Name:  "article",
		Label: "Article",
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "body", Type: domain.FieldTypeText},
			{Key: "state", Type: domain.FieldTypeEnum, EnumValues: []string{"new", "review", "live"}},
		},
	}
}

// seedArticles creates the article type and returns a ready service+ctx.
func seedArticleType(t *testing.T) (ContentService, *memRepo, context.Context) {
	t.Helper()
	svc, repo := newSvc()
	ctx := ctxTenant("t1")
	_, err := svc.CreateContentType(ctx, articleTypeInput())
	require.NoError(t, err)
	return svc, repo, ctx
}

func fieldKeys(dto ContentTypeDTO) []string {
	out := make([]string, 0, len(dto.Fields))
	for _, f := range dto.Fields {
		out = append(out, f.Key)
	}
	return out
}

// --- immutable field properties ----------------------------------------------

// The three immutable properties are refused BY NAME. A generic 400 from
// DisallowUnknownFields would technically also refuse them, and that is exactly
// what ADR-006 Am.1b ruled against: the caller cannot tell which key was the
// problem, and the refusal is invisible to the next reader of UpdateFieldInput.
func TestUpdateField_RefusesImmutablePropsByName(t *testing.T) {
	cases := []struct {
		name string
		in   UpdateFieldInput
		code string
		prop string
	}{
		{"type", UpdateFieldInput{Type: ptrTo(domain.FieldTypeNumber)}, "CONTENT_FIELD_TYPE_IMMUTABLE", "type"},
		{"multiple", UpdateFieldInput{Multiple: ptrTo(true)}, "CONTENT_FIELD_MULTIPLE_IMMUTABLE", "multiple"},
		{"relation_entity", UpdateFieldInput{RelationEntity: ptrTo("author")}, "CONTENT_FIELD_RELATION_IMMUTABLE", "relation_entity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, ctx := seedArticleType(t)
			// A legal edit rides along: the refusal must not depend on the request
			// being otherwise empty, and the legal half must not land either.
			in := tc.in
			in.Label = ptrTo("Renamed Label")

			_, err := svc.UpdateField(ctx, "article", "body", in)
			ae := mustCode(t, err, tc.code, 422)
			assert.Equal(t, "body", ae.Details["field"])
			assert.Equal(t, tc.prop, ae.Details["property"],
				"the refusal must name WHICH property was refused")
			assert.NotEmpty(t, ae.Details["hint"], "the refusal must point at the lossless alternative")

			// Nothing was written — not even the legal label change.
			got, err := svc.GetContentType(ctx, "article")
			require.NoError(t, err)
			for _, f := range got.Fields {
				if f.Key == "body" {
					assert.Equal(t, domain.FieldTypeText, f.Type, "type must be untouched")
					assert.False(t, f.Multiple)
					assert.Empty(t, f.Label, "a refused PATCH must not half-apply")
				}
			}
		})
	}
}

// Refusing `type` is not fastidiousness: buildWhere emits
// `(payload ->> key)::numeric` for a number field, so a text field flipped to
// number with any stored non-numeric value turns the admin LIST page into a 500.
// Sending the value it already has is refused too — "no-op" is a property of the
// data, not of the request, and honouring it would make the guard depend on
// whether the caller happened to read the schema first.
func TestUpdateField_RefusesTypeEvenWhenUnchanged(t *testing.T) {
	svc, _, ctx := seedArticleType(t)
	_, err := svc.UpdateField(ctx, "article", "body", UpdateFieldInput{Type: ptrTo(domain.FieldTypeText)})
	mustCode(t, err, "CONTENT_FIELD_TYPE_IMMUTABLE", 422)
}

func TestUpdateField_UnknownFieldIs404(t *testing.T) {
	svc, _, ctx := seedArticleType(t)
	_, err := svc.UpdateField(ctx, "article", "nope", UpdateFieldInput{Label: ptrTo("x")})
	mustCode(t, err, "CONTENT_FIELD_NOT_FOUND", 404)
}

// --- deleting a field --------------------------------------------------------

// Deleting a field you just mistyped into existence stays frictionless; deleting
// one holding real values makes you say so. The count is in the details because
// "4000 entries" and "1 entry" are different decisions for the operator.
func TestDeleteField_HoldingDataRequiresForce(t *testing.T) {
	svc, _, ctx := seedArticleType(t)
	for _, body := range []string{"first", "second"} {
		_, err := svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{"title": "t", "body": body}))
		require.NoError(t, err)
	}
	// A third entry that never held the key must not inflate the count.
	_, err := svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{"title": "t"}))
	require.NoError(t, err)

	_, err = svc.DeleteField(ctx, "article", "body", false)
	ae := mustCode(t, err, "CONTENT_FIELD_HAS_DATA", 409)
	assert.Equal(t, "body", ae.Details["field"])
	assert.Equal(t, 2, ae.Details["entries"], "the entry count must reach the caller")

	// Still defined — a refusal is not a partial delete.
	got, err := svc.GetContentType(ctx, "article")
	require.NoError(t, err)
	assert.Contains(t, fieldKeys(got), "body")

	// force=true is the consent, and it succeeds.
	after, err := svc.DeleteField(ctx, "article", "body", true)
	require.NoError(t, err)
	assert.NotContains(t, fieldKeys(after), "body")
	assert.Equal(t, []string{"title", "state"}, fieldKeys(after), "field order must survive the delete")
}

// A field holding nothing deletes without force — the friction is proportional
// to the loss, or callers learn to pass force=true reflexively.
func TestDeleteField_EmptyFieldNeedsNoForce(t *testing.T) {
	svc, _, ctx := seedArticleType(t)
	_, err := svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{"title": "t"}))
	require.NoError(t, err)

	after, err := svc.DeleteField(ctx, "article", "body", false)
	require.NoError(t, err)
	assert.NotContains(t, fieldKeys(after), "body")
}

// A value living only in the PUBLISHED snapshot still counts as data. The
// working copy no longer mentions it, but delivery is serving it right now.
func TestDeleteField_CountsThePublishedSnapshotToo(t *testing.T) {
	svc, _, ctx := seedArticleType(t)
	e, err := svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{"title": "t", "body": "live text"}))
	require.NoError(t, err)
	_, err = svc.SetEntryStatus(ctx, "article", e.ID, domain.StatusPublished, 0)
	require.NoError(t, err)
	// Edit the key out of the WORKING copy only. The snapshot keeps it.
	_, err = svc.UpdateEntry(ctx, "article", e.ID, json.RawMessage(`{"body":null}`), 0)
	require.NoError(t, err)

	_, err = svc.DeleteField(ctx, "article", "body", false)
	ae := mustCode(t, err, "CONTENT_FIELD_HAS_DATA", 409)
	assert.Equal(t, 1, ae.Details["entries"],
		"a value still in published_payload is still a value the schema must accommodate")
}

// Otherwise the API reaches a state CreateContentType refuses to produce: a type
// with no fields, whose entries can hold nothing at all.
func TestDeleteField_LastRemainingFieldIsRefused(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	_, err := svc.CreateContentType(ctx, CreateTypeInput{
		Name:   "note",
		Fields: []FieldInput{{Key: "only", Type: domain.FieldTypeString}},
	})
	require.NoError(t, err)

	// force must not buy a way past this one: the refusal is about the SCHEMA
	// being unreachable, not about data loss.
	for _, force := range []bool{false, true} {
		_, err := svc.DeleteField(ctx, "note", "only", force)
		ae := mustCode(t, err, "CONTENT_TYPE_NO_FIELDS", 422)
		assert.Equal(t, "note", ae.Details["type"])
	}
}

func TestDeleteField_UnknownFieldIs404(t *testing.T) {
	svc, _, ctx := seedArticleType(t)
	_, err := svc.DeleteField(ctx, "article", "ghost", true)
	mustCode(t, err, "CONTENT_FIELD_NOT_FOUND", 404)
}

// --- enum_values -------------------------------------------------------------

// Narrowing an enum is the bricking case: an entry still holding a removed value
// fails its NEXT PATCH, on a field the caller did not send, because
// validatePayload checks the whole document. Widening and reordering cannot
// invalidate anything, so they are always allowed — a guard that refused them
// would make the safe operation as expensive as the dangerous one.
func TestUpdateField_EnumNarrowingRefusedWhenValueInUse(t *testing.T) {
	svc, _, ctx := seedArticleType(t)
	_, err := svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{"title": "a", "state": "review"}))
	require.NoError(t, err)

	_, err = svc.UpdateField(ctx, "article", "state",
		UpdateFieldInput{EnumValues: ptrTo([]string{"new", "live"})})
	ae := mustCode(t, err, "CONTENT_ENUM_VALUE_IN_USE", 409)
	assert.Equal(t, "state", ae.Details["field"])
	assert.Equal(t, []string{"review"}, ae.Details["values"], "the refusal must name the values still in use")
	assert.Equal(t, 1, ae.Details["entries"])
}

// Dropping a value nobody stored is a narrowing that invalidates nothing.
func TestUpdateField_EnumNarrowingAllowedWhenValueUnused(t *testing.T) {
	svc, _, ctx := seedArticleType(t)
	_, err := svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{"title": "a", "state": "new"}))
	require.NoError(t, err)

	got, err := svc.UpdateField(ctx, "article", "state",
		UpdateFieldInput{EnumValues: ptrTo([]string{"new", "live"})})
	require.NoError(t, err)
	for _, f := range got.Fields {
		if f.Key == "state" {
			assert.Equal(t, []string{"new", "live"}, f.EnumValues)
		}
	}
}

func TestUpdateField_EnumAddingAndReorderingAreAlwaysAllowed(t *testing.T) {
	svc, _, ctx := seedArticleType(t)
	_, err := svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{"title": "a", "state": "review"}))
	require.NoError(t, err)

	t.Run("adding", func(t *testing.T) {
		got, err := svc.UpdateField(ctx, "article", "state",
			UpdateFieldInput{EnumValues: ptrTo([]string{"new", "review", "live", "archived"})})
		require.NoError(t, err)
		for _, f := range got.Fields {
			if f.Key == "state" {
				assert.Equal(t, []string{"new", "review", "live", "archived"}, f.EnumValues)
			}
		}
	})
	t.Run("reordering", func(t *testing.T) {
		// Same SET, different order. The guard counts removals, so an order-only
		// change must never consult the entries at all.
		got, err := svc.UpdateField(ctx, "article", "state",
			UpdateFieldInput{EnumValues: ptrTo([]string{"archived", "live", "review", "new"})})
		require.NoError(t, err)
		for _, f := range got.Fields {
			if f.Key == "state" {
				assert.Equal(t, []string{"archived", "live", "review", "new"}, f.EnumValues,
					"declaration order is the DTO's field order and must be stored as sent")
			}
		}
	})
}

// A multi-valued enum has to be checked ELEMENT-WISE. Comparing the stored value
// as a whole would ask "is the array ["a","b"] one of the allowed strings", which
// is false for every array and therefore silently refuses every narrowing — or,
// depending which way the comparison falls, silently allows every one.
func TestUpdateField_EnumNarrowingIsElementWiseForMultiValued(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	_, err := svc.CreateContentType(ctx, CreateTypeInput{
		Name: "post",
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "tags", Type: domain.FieldTypeEnum, Multiple: true, EnumValues: []string{"ai", "go", "sql"}},
		},
	})
	require.NoError(t, err)
	_, err = svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{"title": "a", "tags": []string{"ai", "sql"}}))
	require.NoError(t, err)

	// "sql" is the SECOND element — a check that only looked at the first would
	// pass this narrowing and brick the entry.
	_, err = svc.UpdateField(ctx, "post", "tags",
		UpdateFieldInput{EnumValues: ptrTo([]string{"ai", "go"})})
	ae := mustCode(t, err, "CONTENT_ENUM_VALUE_IN_USE", 409)
	assert.Equal(t, []string{"sql"}, ae.Details["values"])

	// Removing an element value nobody used is still fine.
	_, err = svc.UpdateField(ctx, "post", "tags",
		UpdateFieldInput{EnumValues: ptrTo([]string{"ai", "sql"})})
	require.NoError(t, err)
}

func TestUpdateField_EnumValuesRejectedOnNonEnumField(t *testing.T) {
	svc, _, ctx := seedArticleType(t)
	_, err := svc.UpdateField(ctx, "article", "body",
		UpdateFieldInput{EnumValues: ptrTo([]string{"a", "b"})})
	ae := mustCode(t, err, "CONTENT_FIELD_ENUM_NOT_APPLICABLE", 422)
	assert.Equal(t, "body", ae.Details["field"])
	assert.Equal(t, domain.FieldTypeText, ae.Details["type"])
}

func TestUpdateField_EnumValuesDuplicateRejected(t *testing.T) {
	svc, _, ctx := seedArticleType(t)
	_, err := svc.UpdateField(ctx, "article", "state",
		UpdateFieldInput{EnumValues: ptrTo([]string{"new", "review", "new"})})
	ae := mustCode(t, err, "CONTENT_FIELD_ENUM_DUPLICATE", 422)
	assert.Equal(t, "new", ae.Details["value"])
}

// An enum with no values accepts no value at all, so every entry that has one
// becomes invalid at once — the emptied case is a narrowing to nothing, and it is
// refused on its own terms rather than falling through to the in-use count.
func TestUpdateField_EnumValuesEmptiedRejected(t *testing.T) {
	svc, _, ctx := seedArticleType(t)
	_, err := svc.UpdateField(ctx, "article", "state",
		UpdateFieldInput{EnumValues: ptrTo([]string{})})
	ae := mustCode(t, err, "CONTENT_FIELD_ENUM_EMPTY", 422)
	assert.Equal(t, "state", ae.Details["field"])
}

// --- required ----------------------------------------------------------------

// Tightening `required` over rows that lack the field makes every one of them
// un-PATCHable: the next write fails CONTENT_FIELD_REQUIRED on a field the caller
// never sent. Relaxing invalidates nothing and is therefore never guarded.
func TestUpdateField_RequiredTighteningRefusedWhenEntriesLackTheField(t *testing.T) {
	svc, _, ctx := seedArticleType(t)
	_, err := svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{"title": "a", "body": "x"}))
	require.NoError(t, err)
	_, err = svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{"title": "b"}))
	require.NoError(t, err)

	_, err = svc.UpdateField(ctx, "article", "body", UpdateFieldInput{Required: ptrTo(true)})
	ae := mustCode(t, err, "CONTENT_FIELD_REQUIRED_BACKFILL", 409)
	assert.Equal(t, "body", ae.Details["field"])
	assert.Equal(t, 1, ae.Details["entries"])
}

// THE case where a fake and a database silently disagree. validatePayload treats
// `!present || v == nil` as missing, so an entry holding an explicit JSON null
// fails a `required` check exactly as an absent key does. A guard that used bare
// key existence would call this row "present", allow the tightening, and hand the
// operator a green response plus an entry that cannot be saved again.
//
// The same property is asserted against real Postgres in
// repository/schema_mutation_integration_test.go — the SQL is
// `NOT (jsonb_exists(...) AND jsonb_typeof(...) <> 'null')`, and a plain
// jsonb_exists there would pass THIS test while failing the invariant.
func TestUpdateField_ExplicitNullCountsAsMissingForRequired(t *testing.T) {
	svc, repo, ctx := seedArticleType(t)
	_, err := svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{"title": "a", "body": nil}))
	require.NoError(t, err)

	// Guard: the stored payload really does carry an explicit null, or this test
	// would prove only that an absent key is missing — which is trivially true.
	require.Len(t, repo.entries, 1)
	var stored map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(repo.entries[0].Payload, &stored))
	raw, present := stored["body"]
	require.True(t, present, "guard: the key must be PRESENT for this test to mean anything")
	require.Equal(t, "null", string(raw), "guard: the stored value must be an explicit null")

	_, err = svc.UpdateField(ctx, "article", "body", UpdateFieldInput{Required: ptrTo(true)})
	ae := mustCode(t, err, "CONTENT_FIELD_REQUIRED_BACKFILL", 409)
	assert.Equal(t, 1, ae.Details["entries"])
}

func TestUpdateField_RequiredTighteningAllowedWhenEveryEntryHasTheField(t *testing.T) {
	svc, _, ctx := seedArticleType(t)
	_, err := svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{"title": "a", "body": "x"}))
	require.NoError(t, err)

	got, err := svc.UpdateField(ctx, "article", "body", UpdateFieldInput{Required: ptrTo(true)})
	require.NoError(t, err)
	for _, f := range got.Fields {
		if f.Key == "body" {
			assert.True(t, f.Required)
		}
	}
}

// Relaxing is always safe — including over the very rows that would block the
// opposite direction. This is the half that must never grow a guard.
func TestUpdateField_RequiredRelaxingIsAlwaysAllowed(t *testing.T) {
	svc, _, ctx := seedArticleType(t)
	_, err := svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{"title": "a"}))
	require.NoError(t, err)

	got, err := svc.UpdateField(ctx, "article", "title", UpdateFieldInput{Required: ptrTo(false)})
	require.NoError(t, err)
	for _, f := range got.Fields {
		if f.Key == "title" {
			assert.False(t, f.Required)
		}
	}
}

// --- renaming a field --------------------------------------------------------

func TestRenameField_Refusals(t *testing.T) {
	t.Run("to a key that is already defined", func(t *testing.T) {
		svc, _, ctx := seedArticleType(t)
		_, err := svc.RenameField(ctx, "article", "body", RenameInput{Key: "title"})
		ae := mustCode(t, err, "CONTENT_FIELD_EXISTS", 409)
		assert.Equal(t, "title", ae.Details["field"])
	})
	t.Run("to an invalid key", func(t *testing.T) {
		svc, _, ctx := seedArticleType(t)
		// A key is a JSONB object key and a sort/filter target; the pattern is
		// what lets the repository bind it as a parameter safely.
		for _, bad := range []string{"", "1body", "body-2", "body key", "payload->x"} {
			_, err := svc.RenameField(ctx, "article", "body", RenameInput{Key: bad})
			mustCode(t, err, "CONTENT_FIELD_KEY_INVALID", 422)
		}
	})
	t.Run("source field does not exist", func(t *testing.T) {
		svc, _, ctx := seedArticleType(t)
		_, err := svc.RenameField(ctx, "article", "ghost", RenameInput{Key: "spirit"})
		mustCode(t, err, "CONTENT_FIELD_NOT_FOUND", 404)
	})
}

// Renaming a field to itself is a no-op, not a conflict: the target key IS taken,
// by the field doing the renaming. It must not rewrite a single document either —
// a retried request is not a reason to bump every entry's version.
func TestRenameField_ToItselfIsANoOp(t *testing.T) {
	svc, repo, ctx := seedArticleType(t)
	e, err := svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{"title": "a", "body": "x"}))
	require.NoError(t, err)
	before := repo.entries[0].Version

	got, err := svc.RenameField(ctx, "article", "body", RenameInput{Key: "body"})
	require.NoError(t, err)
	assert.Equal(t, []string{"title", "body", "state"}, fieldKeys(got))

	assert.Equal(t, before, repo.entries[0].Version, "a no-op rename must not touch any entry")
	stored, err := svc.GetEntry(ctx, "article", e.ID)
	require.NoError(t, err)
	assert.JSONEq(t, `{"title":"a","body":"x"}`, string(stored.Data))
}

// --- content type verbs -------------------------------------------------------

// DeleteContentType refuses rather than cascading: content_type_fields and
// entries both carry ON DELETE CASCADE, so one DELETE would take a tenant's
// entire collection with it. There is deliberately no force flag — deleting a
// field is re-enterable, deleting a type destroys the ids relation fields and
// external consumers hold.
func TestDeleteContentType_RefusesWhenEntriesExist(t *testing.T) {
	svc, _, ctx := seedArticleType(t)
	for i := 0; i < 3; i++ {
		_, err := svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{"title": "a"}))
		require.NoError(t, err)
	}
	err := svc.DeleteContentType(ctx, "article")
	ae := mustCode(t, err, "CONTENT_TYPE_HAS_ENTRIES", 409)
	assert.Equal(t, "article", ae.Details["type"])
	assert.Equal(t, 3, ae.Details["entries"])

	// The type survives the refusal.
	_, err = svc.GetContentType(ctx, "article")
	require.NoError(t, err)
}

// A referrer's own schema gives no hint that its writes are about to start
// failing: relation_entity names a type that would no longer exist, and
// checkRelations resolves it per write. The details must therefore name the
// referring type AND field, or the operator has nothing to go fix.
func TestDeleteContentType_RefusesWhenARelationPointsAtIt(t *testing.T) {
	svc, _, ctx := seedArticleType(t)
	_, err := svc.CreateContentType(ctx, CreateTypeInput{
		Name: "comment",
		Fields: []FieldInput{
			{Key: "text", Type: domain.FieldTypeString, Required: true},
			{Key: "about", Type: domain.FieldTypeRelation, RelationEntity: "article"},
		},
	})
	require.NoError(t, err)

	err = svc.DeleteContentType(ctx, "article")
	ae := mustCode(t, err, "CONTENT_TYPE_REFERENCED", 409)
	assert.Equal(t, "article", ae.Details["type"])
	refs, ok := ae.Details["referenced_by"].([]map[string]string)
	require.True(t, ok, "referenced_by must be structured, not a prose string: %#v", ae.Details["referenced_by"])
	assert.Equal(t, []map[string]string{{"type": "comment", "field": "about"}}, refs)
}

// The referrer guard is TENANT-SCOPED. Another tenant's relation field naming the
// same type is not a reason to refuse — type names are only unique per tenant, so
// a global check would make one tenant's schema block another's delete.
func TestDeleteContentType_IgnoresAnotherTenantsReferrer(t *testing.T) {
	svc, _, ctx := seedArticleType(t)
	other := ctxTenant("t2")
	_, err := svc.CreateContentType(other, CreateTypeInput{
		Name: "comment",
		Fields: []FieldInput{
			{Key: "about", Type: domain.FieldTypeRelation, RelationEntity: "article"},
		},
	})
	require.NoError(t, err)

	require.NoError(t, svc.DeleteContentType(ctx, "article"))
	_, err = svc.GetContentType(ctx, "article")
	assert.True(t, apperrors.Is(err, apperrors.ErrNotFound))
}

func TestDeleteContentType_CleanTypeSucceeds(t *testing.T) {
	svc, _, ctx := seedArticleType(t)
	require.NoError(t, svc.DeleteContentType(ctx, "article"))
	_, err := svc.GetContentType(ctx, "article")
	assert.True(t, apperrors.Is(err, apperrors.ErrNotFound))
}

func TestRenameContentType_Refusals(t *testing.T) {
	t.Run("to a name another type already holds", func(t *testing.T) {
		svc, _, ctx := seedArticleType(t)
		_, err := svc.CreateContentType(ctx, CreateTypeInput{
			Name: "comment", Fields: []FieldInput{{Key: "text", Type: domain.FieldTypeString}},
		})
		require.NoError(t, err)

		_, err = svc.RenameContentType(ctx, "article", RenameInput{Name: "comment"})
		ae := mustCode(t, err, "CONTENT_TYPE_EXISTS", 409)
		assert.Equal(t, "comment", ae.Details["name"])
	})
	t.Run("to an invalid name", func(t *testing.T) {
		svc, _, ctx := seedArticleType(t)
		for _, bad := range []string{"", "9lives", "my-type", "a b"} {
			_, err := svc.RenameContentType(ctx, "article", RenameInput{Name: bad})
			mustCode(t, err, "CONTENT_TYPE_NAME_INVALID", 422)
		}
	})
}

func TestRenameContentType_ToItselfIsANoOp(t *testing.T) {
	svc, _, ctx := seedArticleType(t)
	got, err := svc.RenameContentType(ctx, "article", RenameInput{Name: "article"})
	require.NoError(t, err)
	assert.Equal(t, "article", got.Name)
}

// Renaming is its OWN verb, so the PATCH surface cannot carry a name at all. This
// is the structural half of the same rule the immutable-property refusals cover:
// a routine label edit must not be able to rewrite every stored document.
func TestUpdateContentType_CannotRename(t *testing.T) {
	svc, _, ctx := seedArticleType(t)
	got, err := svc.UpdateContentType(ctx, "article", UpdateTypeInput{Label: "  Articles  "})
	require.NoError(t, err)
	assert.Equal(t, "article", got.Name, "the PATCH surface has no `name` — renaming is POST /rename")
	assert.Equal(t, "Articles", got.Label, "the label is trimmed before storage")
}

// --- authorization ------------------------------------------------------------

func ctxRole(tenant, role string) context.Context {
	return authn.WithSubject(context.Background(), authn.Subject{
		UserID:     uuid.New(),
		TenantID:   tenant,
		TenantRole: role,
	})
}

// The rest of this file runs under NewAllowAllAuthorizer (see newSvc), which is
// the right default for policy tests but says nothing about who may call these
// verbs. This one wires the REAL RBAC authorizer so the split is asserted end to
// end through the service, not only in authz's own table test: an editor keeps
// every content capability but cannot drop a collection.
func TestSchemaMutation_EditorRefusedDestructiveVerbsOnly(t *testing.T) {
	newRBAC := func(t *testing.T) (ContentService, context.Context) {
		t.Helper()
		svc := NewContentService(&memRepo{}, authz.NewRBACAuthorizer(), staticPlan(Quota{}))
		owner := ctxRole("t1", "owner")
		_, err := svc.CreateContentType(owner, articleTypeInput())
		require.NoError(t, err)
		return svc, owner
	}

	// Destructive = ActionContentSchemaWrite: it either rewrites every entry of
	// the type or cascades them away.
	destructive := map[string]func(ContentService, context.Context) error{
		"delete field": func(s ContentService, ctx context.Context) error {
			_, err := s.DeleteField(ctx, "article", "body", true)
			return err
		},
		"rename field": func(s ContentService, ctx context.Context) error {
			_, err := s.RenameField(ctx, "article", "body", RenameInput{Key: "content"})
			return err
		},
		"rename type": func(s ContentService, ctx context.Context) error {
			_, err := s.RenameContentType(ctx, "article", RenameInput{Name: "story"})
			return err
		},
		"delete type": func(s ContentService, ctx context.Context) error {
			return s.DeleteContentType(ctx, "article")
		},
	}
	// Non-destructive = ActionContentUpdate: bounded, reversible edits.
	safe := map[string]func(ContentService, context.Context) error{
		"patch field": func(s ContentService, ctx context.Context) error {
			_, err := s.UpdateField(ctx, "article", "body", UpdateFieldInput{Label: ptrTo("Body")})
			return err
		},
		"patch type label": func(s ContentService, ctx context.Context) error {
			_, err := s.UpdateContentType(ctx, "article", UpdateTypeInput{Label: "Articles"})
			return err
		},
		"add field": func(s ContentService, ctx context.Context) error {
			_, err := s.AddField(ctx, "article", FieldInput{Key: "extra", Type: domain.FieldTypeString})
			return err
		},
	}

	for name, call := range destructive {
		t.Run("editor refused: "+name, func(t *testing.T) {
			svc, _ := newRBAC(t)
			err := call(svc, ctxRole("t1", "editor"))
			require.ErrorIs(t, err, apperrors.ErrForbidden)
		})
		t.Run("viewer refused: "+name, func(t *testing.T) {
			svc, _ := newRBAC(t)
			require.ErrorIs(t, call(svc, ctxRole("t1", "viewer")), apperrors.ErrForbidden)
		})
		for _, role := range []string{"owner", "admin"} {
			t.Run(role+" allowed: "+name, func(t *testing.T) {
				svc, _ := newRBAC(t)
				require.NoError(t, call(svc, ctxRole("t1", role)))
			})
		}
	}
	for name, call := range safe {
		t.Run("editor allowed: "+name, func(t *testing.T) {
			svc, _ := newRBAC(t)
			require.NoError(t, call(svc, ctxRole("t1", "editor")))
		})
	}
}

// --- pruneUndefined: the delete/write race ------------------------------------

// pruneUndefined drops keys the schema no longer defines from the MERGE BASE of
// an update. Without it, a stored row holding an orphan key is un-PATCHable:
// validatePayload checks the WHOLE document, so the write fails
// CONTENT_FIELD_UNKNOWN naming a field the caller never sent, and — because
// SetEntryStatus does not re-validate — that same row can still be published.
//
// Note on the race in pruneUndefined's own doc comment: for a row that actually
// held the key, DeleteField bumps `version`, so a write built on a pre-delete
// read is already rejected by the optimistic lock before validation is reached.
// What pruneUndefined therefore buys is the RESIDUAL drift — a restored backup, a
// hand-repaired row, an import that ran around the service — which is the
// broader claim its comment makes, and it is what this test reproduces.
func TestUpdateEntry_PrunesAnOrphanKeyFromTheMergeBase(t *testing.T) {
	svc, repo := newSvc()
	ctx := ctxTenant("t1")
	_, err := svc.CreateContentType(ctx, articleTypeInput())
	require.NoError(t, err)
	created, err := svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{"title": "a", "body": "doomed"}))
	require.NoError(t, err)

	_, err = svc.DeleteField(ctx, "article", "body", true)
	require.NoError(t, err)

	// Drift: the stored document reacquires a key the schema no longer defines,
	// the way a restored backup or a hand-run UPDATE would leave it. Written
	// straight into the store, not through the service, because the service is
	// precisely what refuses to produce this state.
	require.Len(t, repo.entries, 1)
	repo.entries[0].Payload = json.RawMessage(`{"title":"a","body":"orphan"}`)

	// A PATCH touching an entirely different field must still succeed: the orphan
	// is pruned out of the merge base instead of being validated and rejected.
	updated, err := svc.UpdateEntry(ctx, "article", created.ID, mustJSON(t, map[string]any{"title": "b"}), 0)
	require.NoError(t, err, "an orphan key in the merge base must not brick the entry")
	var got map[string]any
	require.NoError(t, json.Unmarshal(updated.Data, &got))
	assert.Equal(t, "b", got["title"])
	assert.NotContains(t, got, "body", "the undefined key must not survive the merge")

	// The caller's OWN patch is never pruned — a client typo is still a 422, or
	// pruning would silently swallow every misspelled field name.
	_, err = svc.UpdateEntry(ctx, "article", created.ID, mustJSON(t, map[string]any{"body": "resurrect"}), 0)
	mustCode(t, err, "CONTENT_FIELD_UNKNOWN", 422)
}
