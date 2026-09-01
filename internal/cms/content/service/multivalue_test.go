package service

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Everything here is a REFUSAL, and that is deliberate. Each of these fires in
// buildField / validatePayload / parseFilter / parseSort — before any repository
// call — so none of it depends on the fake repo mirroring Postgres. The
// semantics of `@>` cannot be asserted against a Go fake at all and live in
// repository/multivalue_integration_test.go instead; memRepo does not even
// implement Filters (its own comments say so), which is precisely why no
// containment assertion is made here.

func codeOf(t *testing.T, err error) string {
	t.Helper()
	require.Error(t, err)
	var ae *apperrors.AppError
	require.ErrorAs(t, err, &ae)
	return ae.Code
}

func detailsOf(t *testing.T, err error) map[string]any {
	t.Helper()
	var ae *apperrors.AppError
	require.ErrorAs(t, err, &ae)
	return ae.Details
}

func tagTypeInput(fieldType string, required bool, enumValues []string) CreateTypeInput {
	return CreateTypeInput{
		Name:  "post",
		Label: "Post",
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "tags", Type: fieldType, Required: required, Multiple: true, EnumValues: enumValues},
		},
	}
}

func TestBuildField_MultipleOnlyOnSupportedTypes(t *testing.T) {
	for _, ft := range []string{
		domain.FieldTypeBoolean, domain.FieldTypeDate,
		domain.FieldTypeDateTime, domain.FieldTypeText, domain.FieldTypeFile,
	} {
		t.Run(ft, func(t *testing.T) {
			svc, _ := newSvc()
			_, err := svc.CreateContentType(ctxTenant("tenant-a"), CreateTypeInput{
				Name:   "post",
				Fields: []FieldInput{{Key: "xs", Type: ft, Multiple: true}},
			})
			assert.Equal(t, "CONTENT_FIELD_MULTIPLE_UNSUPPORTED", codeOf(t, err))
		})
	}
	for _, ft := range []string{
		domain.FieldTypeString, domain.FieldTypeEnum, domain.FieldTypeRelation,
		domain.FieldTypeNumber,
	} {
		t.Run(ft+" is allowed", func(t *testing.T) {
			svc, _ := newSvc()
			in := FieldInput{Key: "xs", Type: ft, Multiple: true}
			switch ft {
			case domain.FieldTypeEnum:
				in.EnumValues = []string{"a", "b"}
			case domain.FieldTypeRelation:
				in.RelationEntity = "post"
			}
			_, err := svc.CreateContentType(ctxTenant("tenant-a"), CreateTypeInput{Name: "post", Fields: []FieldInput{in}})
			require.NoError(t, err)
		})
	}
}

// The flag must survive the AddField path too, not just type creation — they are
// two doors to the same definition.
//
// boolean rather than number, and it is the permanence that earns the place: a
// multiset of booleans is a count, so this is the one refusal in the list that
// will never be reopened. This test named number until number was allowed, at
// which point it went green for a reason that had nothing to do with AddField.
func TestAddField_RefusesMultipleOnUnsupportedType(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)
	_, err = svc.AddField(ctx, "order", FieldInput{Key: "b", Type: domain.FieldTypeBoolean, Multiple: true})
	assert.Equal(t, "CONTENT_FIELD_MULTIPLE_UNSUPPORTED", codeOf(t, err))
}

func TestMultiValue_PayloadShape(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, tagTypeInput(domain.FieldTypeString, false, nil))
	require.NoError(t, err)

	t.Run("a scalar is refused for a multi field", func(t *testing.T) {
		_, err := svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{"title": "T", "tags": "ai"}))
		assert.Equal(t, "CONTENT_FIELD_TYPE_MISMATCH", codeOf(t, err))
	})

	t.Run("an empty array is legal when optional and round-trips as []", func(t *testing.T) {
		// cinqing's courses all carry tags: [] today, so normalising this to
		// "absent" would silently change the shape of every migrated entry.
		got, err := svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{"title": "T", "tags": []any{}}))
		require.NoError(t, err)
		assert.Contains(t, string(got.Data), `"tags":[]`)
	})

	t.Run("duplicates are refused, not deduped", func(t *testing.T) {
		_, err := svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{"title": "T", "tags": []any{"ai", "ml", "ai"}}))
		assert.Equal(t, "CONTENT_FIELD_DUPLICATE_VALUE", codeOf(t, err))
		assert.EqualValues(t, 2, detailsOf(t, err)["index"], "the error must name WHICH element repeats")
	})

	t.Run("over the element cap is refused", func(t *testing.T) {
		xs := make([]any, domain.MaxMultipleElements+1)
		for i := range xs {
			xs[i] = fmt.Sprintf("t%d", i)
		}
		_, err := svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{"title": "T", "tags": xs}))
		assert.Equal(t, "CONTENT_FIELD_TOO_MANY_VALUES", codeOf(t, err))
	})

	t.Run("exactly the cap is accepted", func(t *testing.T) {
		xs := make([]any, domain.MaxMultipleElements)
		for i := range xs {
			xs[i] = fmt.Sprintf("t%d", i)
		}
		_, err := svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{"title": "T2", "tags": xs}))
		require.NoError(t, err)
	})
}

// The mirror image, and a regression guard: before this change the scalar arms
// accepted only their own Go type, so an array fell through to `nil` — no error.
func TestMultiValue_ArrayRefusedForScalarField(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)
	_, err = svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{"title": []any{"a"}}))
	assert.Equal(t, "CONTENT_FIELD_TYPE_MISMATCH", codeOf(t, err))
}

func TestMultiValue_RequiredRejectsEmptyArray(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, tagTypeInput(domain.FieldTypeString, true, nil))
	require.NoError(t, err)
	_, err = svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{"title": "T", "tags": []any{}}))
	assert.Equal(t, "CONTENT_FIELD_REQUIRED", codeOf(t, err),
		"[] must report the same error as an absent key — callers should not need two branches for 'you gave me nothing'")
}

func TestMultiValue_EnumElementsValidatedWithIndex(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, tagTypeInput(domain.FieldTypeEnum, false, []string{"ai", "ml"}))
	require.NoError(t, err)
	_, err = svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{"title": "T", "tags": []any{"ai", "nope"}}))
	assert.Equal(t, "CONTENT_FIELD_ENUM_INVALID", codeOf(t, err))
	assert.EqualValues(t, 1, detailsOf(t, err)["index"],
		"without the index a forty-element list tells you something is wrong and not where")
}

func TestMultiValue_FilterOperatorsAreGatedByCardinality(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, tagTypeInput(domain.FieldTypeString, false, nil))
	require.NoError(t, err)

	// Scalar operators on a multi field. `contains` matters most: it would not
	// error, it would quietly match substrings ACROSS elements.
	for _, op := range []string{"eq", "neq", "contains", "in", "gt", "gte", "lt", "lte"} {
		t.Run("tags:"+op, func(t *testing.T) {
			_, err := svc.ListEntries(ctx, "post", ListEntriesInput{Filters: []string{"tags:" + op + ":ai"}})
			assert.Equal(t, "CONTENT_FILTER_OP_UNSUPPORTED_FOR_FIELD", codeOf(t, err))
		})
	}
	// And the symmetry: a caller must not stumble into containment semantics on
	// a scalar field either.
	for _, op := range []string{"has", "nhas"} {
		t.Run("title:"+op, func(t *testing.T) {
			_, err := svc.ListEntries(ctx, "post", ListEntriesInput{Filters: []string{"title:" + op + ":x"}})
			assert.Equal(t, "CONTENT_FILTER_OP_UNSUPPORTED_FOR_FIELD", codeOf(t, err))
		})
	}
	t.Run("has takes a single value", func(t *testing.T) {
		_, err := svc.ListEntries(ctx, "post", ListEntriesInput{Filters: []string{"tags:has:ai,ml"}})
		assert.Equal(t, "CONTENT_FILTER_VALUE_INVALID", codeOf(t, err),
			"any-of needs its own operator; CSV already means any-of for `in`")
	})
	t.Run("has is accepted on a multi field", func(t *testing.T) {
		_, err := svc.ListEntries(ctx, "post", ListEntriesInput{Filters: []string{"tags:has:ai"}})
		require.NoError(t, err)
	})
}

// number is the newest member of AllowedMultipleTypes and the only one whose
// elements are not strings, so the arms it exercises are different ones: the
// float64 branch of validateScalar, and a duplicate check keyed on a numeric
// value. The containment half lives in repository/multivalue_number_integration_test.go
// — memRepo does not implement Filters, so the only honest place to assert what
// `has:7` matches is against Postgres.
func TestMultiValueNumber_Write(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, CreateTypeInput{
		Name:  "quiz",
		Label: "Quiz",
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "scores", Type: domain.FieldTypeNumber, Multiple: true},
		},
	})
	require.NoError(t, err)

	t.Run("an array of numbers round-trips as numbers", func(t *testing.T) {
		got, err := svc.CreateEntry(ctx, "quiz", mustJSON(t, map[string]any{"title": "T", "scores": []any{3, 7}}))
		require.NoError(t, err)
		assert.Contains(t, string(got.Data), `"scores":[3,7]`,
			"the elements must not come back quoted — a string array would match no numeric containment query")
	})

	t.Run("a scalar is refused for a multi field", func(t *testing.T) {
		_, err := svc.CreateEntry(ctx, "quiz", mustJSON(t, map[string]any{"title": "T", "scores": 3}))
		assert.Equal(t, "CONTENT_FIELD_TYPE_MISMATCH", codeOf(t, err))
	})

	t.Run("a non-numeric element is refused, and the error names which one", func(t *testing.T) {
		_, err := svc.CreateEntry(ctx, "quiz", mustJSON(t, map[string]any{"title": "T", "scores": []any{3, "7"}}))
		assert.Equal(t, "CONTENT_FIELD_TYPE_MISMATCH", codeOf(t, err))
		assert.EqualValues(t, 1, detailsOf(t, err)["index"])
	})

	// The write-time and query-time definitions of "the same value" have to be the
	// same definition. jsonb stores numbers as `numeric`, so [4, 4.0] is one
	// element to a containment query; if the duplicate check disagreed and let the
	// pair through, `has:4` would match an entry whose second element the caller
	// believes is distinct.
	t.Run("4 and 4.0 are one element, as they are to jsonb", func(t *testing.T) {
		_, err := svc.CreateEntry(ctx, "quiz", mustJSON(t, map[string]any{"title": "T", "scores": []any{4, 4.0}}))
		assert.Equal(t, "CONTENT_FIELD_DUPLICATE_VALUE", codeOf(t, err))
		assert.EqualValues(t, 1, detailsOf(t, err)["index"])
	})

	// The gate that lets number be multiple at all. It is asserted here as well as
	// at the repository layer because they check different things: this says the
	// caller gets a 400, the integration test says what the 400 is preventing.
	t.Run("scalar comparison operators stay refused", func(t *testing.T) {
		for _, op := range []string{"eq", "neq", "gt", "gte", "lt", "lte", "in", "contains"} {
			t.Run("scores:"+op, func(t *testing.T) {
				_, err := svc.ListEntries(ctx, "quiz", ListEntriesInput{Filters: []string{"scores:" + op + ":5"}})
				assert.Equal(t, "CONTENT_FILTER_OP_UNSUPPORTED_FOR_FIELD", codeOf(t, err))
			})
		}
	})

	t.Run("sorting stays refused", func(t *testing.T) {
		_, err := svc.ListEntries(ctx, "quiz", ListEntriesInput{Sort: "scores:asc"})
		assert.Equal(t, "CONTENT_SORT_FIELD_UNSORTABLE", codeOf(t, err))
	})

	t.Run("has is accepted", func(t *testing.T) {
		_, err := svc.ListEntries(ctx, "quiz", ListEntriesInput{Filters: []string{"scores:has:7"}})
		require.NoError(t, err)
	})
}

func TestMultiValue_SortIsRefused(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, tagTypeInput(domain.FieldTypeString, false, nil))
	require.NoError(t, err)
	_, err = svc.ListEntries(ctx, "post", ListEntriesInput{Sort: "tags:asc"})
	assert.Equal(t, "CONTENT_SORT_FIELD_UNSORTABLE", codeOf(t, err),
		"text-array ordering is stable and meaningless, which looks correct in a UI — worse than an error")
}

// validateScalar's default arm is unreachable through the service — buildField
// rejects unknown field types first — so it is called directly here. It is worth
// having and worth testing anyway: nothing in the DATABASE constrains
// field_type, so a row written around the service (a migration, a repair script)
// produces exactly this input, and before the arm existed the switch fell
// through to nil and stored the value unvalidated.
func TestValidateScalar_UnknownTypeFailsClosed(t *testing.T) {
	err := validateScalar(domain.Field{Key: "x", Type: "not-a-real-type"}, "anything")
	assert.Equal(t, "CONTENT_FIELD_TYPE_MISMATCH", codeOf(t, err))
}

// The flag has to reach the client, or a form engine renders a scalar control
// over an array. `multiple` deliberately carries no omitempty.
func TestMultiValue_FlagReachesTheTypeDTO(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	dto, err := svc.CreateContentType(ctx, tagTypeInput(domain.FieldTypeString, false, nil))
	require.NoError(t, err)
	var tags, title FieldDTO
	for _, f := range dto.Fields {
		switch f.Key {
		case "tags":
			tags = f
		case "title":
			title = f
		}
	}
	assert.True(t, tags.Multiple)
	assert.False(t, title.Multiple)
	b := mustJSON(t, dto)
	assert.True(t, strings.Contains(string(b), `"multiple":true`), "the flag must be serialised, not just present in Go")
	assert.True(t, strings.Contains(string(b), `"multiple":false`), "false must be emitted, not omitted — absent would read as 'this server does not know the flag'")
}
