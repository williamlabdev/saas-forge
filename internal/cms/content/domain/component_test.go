package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ADR-020 §2: a sub-field is any of the ten existing types, and never a
// component; it carries no unique and no per-field roles. The rule lives in
// one function so the create verb, the add-field verb, the update verb and
// the artifact diff cannot each remember a different subset of it.
//
// Amendment 1 adds `dynamiczone` to what a sub-field may NOT be, under the
// SAME code as the component case: a zone holds component items, so a zone
// inside a component is nesting spelled differently, and a second code would
// make the caller learn two names for one refusal.
func TestCheckComponentSubField(t *testing.T) {
	t.Run("every existing type is allowed", func(t *testing.T) {
		for _, typ := range AllowedFieldTypes() {
			if typ == FieldTypeComponent || typ == FieldTypeDynamicZone {
				continue
			}
			assert.Nil(t, CheckComponentSubField(Field{Key: "k", Type: typ}), typ)
		}
	})
	t.Run("a component inside a component is refused", func(t *testing.T) {
		v := CheckComponentSubField(Field{Key: "k", Type: FieldTypeComponent})
		require.NotNil(t, v)
		assert.Equal(t, "CONTENT_COMPONENT_NESTING_UNSUPPORTED", v.Code)
	})
	t.Run("a dynamic zone inside a component is refused", func(t *testing.T) {
		v := CheckComponentSubField(Field{Key: "k", Type: FieldTypeDynamicZone})
		require.NotNil(t, v)
		assert.Equal(t, "CONTENT_COMPONENT_NESTING_UNSUPPORTED", v.Code)
	})
	for _, tc := range []struct {
		name string
		f    Field
		attr string
	}{
		{"unique", Field{Key: "k", Type: FieldTypeString, FieldConstraints: FieldConstraints{Unique: true}}, "unique"},
		{"read_roles", Field{Key: "k", Type: FieldTypeString, ReadRoles: []string{"admin"}}, "read_roles"},
		{"write_roles", Field{Key: "k", Type: FieldTypeString, WriteRoles: []string{"admin"}}, "write_roles"},
	} {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			v := CheckComponentSubField(tc.f)
			require.NotNil(t, v)
			assert.Equal(t, "CONTENT_COMPONENT_FIELD_ATTRIBUTE_UNSUPPORTED", v.Code)
			assert.Equal(t, tc.attr, v.Attribute)
		})
	}
}

func TestComponentIsAFieldTypeAndMayBeMultiple(t *testing.T) {
	assert.True(t, ValidFieldType(FieldTypeComponent))
	assert.True(t, MultipleAllowedFor(FieldTypeComponent))
	assert.Contains(t, AllowedFieldTypes(), FieldTypeComponent)
	// The two allow-lists the hard line (ADR-020 §3) is spelled through.
	assert.False(t, UniqueAllowedFor(FieldTypeComponent))
}

func TestComponentFieldByKey(t *testing.T) {
	c := Component{Fields: []Field{{Key: "a"}, {Key: "b"}}}
	f, ok := c.FieldByKey("b")
	assert.True(t, ok)
	assert.Equal(t, "b", f.Key)
	_, ok = c.FieldByKey("zzz")
	assert.False(t, ok)
}
