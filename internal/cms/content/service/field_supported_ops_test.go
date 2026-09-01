package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
)

// ADR-013 §6: FieldDTO.Supported publishes the filter operators a field accepts,
// so a caller can plan a query instead of discovering the grammar by being
// refused one.
//
// The whole risk of publishing a derived list is that it drifts from the code
// that enforces it. So the parity test below does not ask supportedOps() what it
// thinks; it asks the FILTER PATH what it actually accepts, by sending one.

// probeType is one type carrying every (field type, cardinality) combination the
// CMS will accept, so the parity test covers the axes the answer depends on
// rather than one convenient field.
func probeTypeInput() CreateTypeInput {
	return CreateTypeInput{
		Name:  "probe",
		Label: "Probe",
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Label: "Title"},
			{Key: "tags", Type: domain.FieldTypeString, Label: "Tags", Multiple: true},
			{Key: "body", Type: domain.FieldTypeRichText, Label: "Body"},
			{Key: "count", Type: domain.FieldTypeNumber, Label: "Count"},
			{Key: "flag", Type: domain.FieldTypeBoolean, Label: "Flag"},
			{Key: "day", Type: domain.FieldTypeDate, Label: "Day"},
			{Key: "at", Type: domain.FieldTypeDateTime, Label: "At"},
			{Key: "status", Type: domain.FieldTypeEnum, Label: "Status", EnumValues: []string{"new", "paid"}},
			{Key: "labels", Type: domain.FieldTypeEnum, Label: "Labels", EnumValues: []string{"a", "b"}, Multiple: true},
			{Key: "doc", Type: domain.FieldTypeFile, Label: "Doc"},
		},
	}
}

// everyOperator is the whole grammar, written out. Deriving this list from
// repository.ValidOp would make the probe below blind to exactly the operator
// somebody forgot to classify — the candidate set must come from outside the
// code under test.
var everyOperator = []repository.Op{"eq", "neq", "gt", "gte", "lt", "lte", "in", "contains", "has", "nhas"}

func fieldDTO(t *testing.T, svc ContentService, ctx context.Context, typeName, key string) FieldDTO {
	t.Helper()
	dto, err := svc.GetContentType(ctx, typeName)
	require.NoError(t, err)
	for _, f := range dto.Fields {
		if f.Key == key {
			return f
		}
	}
	t.Fatalf("field %q not found on type %q", key, typeName)
	return FieldDTO{}
}

// The parity that matters: what GET /types PUBLISHES equals what the filter path
// ACCEPTS, field by field and operator by operator.
//
// The expectation is produced by sending real filters, which is a different code
// path from the projection — so this cannot go green by both sides agreeing with
// the same wrong helper. That failure mode is not hypothetical here: rich text
// refuses every operator in parseFilter while OpsForCardinality knows nothing
// about it, and §6's own instruction ("add OpsForCardinality's result") would
// have shipped eight operators on a field that answers 400 to all of them.
func TestSupportedOpsAreExactlyWhatTheFilterPathAccepts(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(ctx, probeTypeInput())
	require.NoError(t, err)

	for _, f := range probeTypeInput().Fields {
		t.Run(f.Key, func(t *testing.T) {
			var accepted []repository.Op
			for _, op := range everyOperator {
				// A value with no comma: has/nhas refuse a CSV operand, which is
				// a rule about the VALUE, and a probe that tripped it would read
				// as "operator not supported".
				_, err := svc.ListEntries(ctx, "probe", ListEntriesInput{
					Filters: []string{f.Key + ":" + string(op) + ":x"},
				})
				switch {
				case err == nil:
					accepted = append(accepted, op)
				case codeOf(t, err) == "CONTENT_FILTER_OP_UNSUPPORTED_FOR_FIELD":
					// refused for this field — the answer this test is reading
				default:
					t.Fatalf("%s:%s probe failed for an unrelated reason: %v", f.Key, op, err)
				}
			}

			published := fieldDTO(t, svc, ctx, "probe", f.Key).Supported
			assert.ElementsMatch(t, accepted, published,
				"GET /types advertises %v for %q but the filter path accepts %v", published, f.Key, accepted)
		})
	}
}

// The values themselves, written down. The test above proves the two halves
// AGREE; it would still pass if both went wrong together (a change that closed
// the filter path AND the published list would keep them equal). This one says
// what the answers actually are.
func TestSupportedOpsAreTheExpectedLists(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(ctx, probeTypeInput())
	require.NoError(t, err)

	assert.Equal(t,
		[]repository.Op{"eq", "neq", "gt", "gte", "lt", "lte", "in", "contains"},
		fieldDTO(t, svc, ctx, "probe", "title").Supported,
		"a scalar field's operators, in the order the API publishes them")

	assert.Equal(t, []repository.Op{"has", "nhas"},
		fieldDTO(t, svc, ctx, "probe", "tags").Supported,
		"a multi-valued field gets containment and nothing else — the scalar comparisons cast an array and raise 22P02")

	assert.Equal(t, []repository.Op{}, fieldDTO(t, svc, ctx, "probe", "body").Supported,
		"rich text is a block document: every operator either casts a JSON array or ILIKEs its raw text")
}

// `[]` and `null` are different claims and rich text is where they diverge: one
// says "no operator is legal", the other says "this server did not tell you".
// Asserted on the marshalled bytes, because the distinction only exists in JSON —
// on the Go struct both are a length-zero slice.
func TestSupportedOpsMarshalAsAnEmptyArrayNotNull(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(ctx, probeTypeInput())
	require.NoError(t, err)

	body, err := json.Marshal(fieldDTO(t, svc, ctx, "probe", "body"))
	require.NoError(t, err)
	assert.Contains(t, string(body), `"supported":[]`)
	assert.NotContains(t, string(body), `"supported":null`)
}

// The two surfaces that publish this list must publish the SAME list. Before
// §6 the 400's details were the only way to obtain it; now that GET /types
// carries it too, a caller can compare them — and a caller that finds them
// disagreeing has no way to tell which one to believe.
func TestTheRefusalDetailAndTheSchemaAgree(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(ctx, probeTypeInput())
	require.NoError(t, err)

	for _, key := range []string{"tags", "body"} {
		// An operator each of those fields refuses, so the 400 carries the list.
		_, err := svc.ListEntries(ctx, "probe", ListEntriesInput{Filters: []string{key + ":eq:x"}})
		require.Equal(t, "CONTENT_FILTER_OP_UNSUPPORTED_FOR_FIELD", codeOf(t, err), "field %q", key)

		fromError, ok := detailsOf(t, err)["supported"].([]repository.Op)
		require.True(t, ok, "field %q: the refusal must still carry the list it always carried", key)
		assert.Equal(t, fieldDTO(t, svc, ctx, "probe", key).Supported, fromError,
			"field %q: the schema and the refusal disagree about which operators exist", key)
	}
}
