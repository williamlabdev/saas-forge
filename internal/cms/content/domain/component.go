package domain

import (
	"time"

	"github.com/google/uuid"
)

// Component is a named, tenant-scoped, reusable group of fields (ADR-020).
//
// A content type field of type `component` references one by name; the
// entry stores the component's value INLINE — an object shaped by Fields, or
// an array of such objects when the referencing field is Multiple. The
// component owns nothing at rest except its definition: there is no
// component table of values, no id per item, and consequently nothing to
// filter, sort, populate or make unique on (the hard line ADR-020 draws).
//
// Fields reuse the Field struct and the content_type_fields table: a
// sub-field row has content_type_id NULL and component_id set. The
// attributes a sub-field may NOT carry — unique, read_roles, write_roles,
// and the `component` type itself — are refused by CheckComponentSubField.
type Component struct {
	ID        uuid.UUID
	TenantID  string
	Name      string
	Label     string
	Fields    []Field
	CreatedAt time.Time
	UpdatedAt time.Time
}

// FieldByKey returns the sub-field with the given key.
func (c *Component) FieldByKey(key string) (Field, bool) {
	for _, f := range c.Fields {
		if f.Key == key {
			return f, true
		}
	}
	return Field{}, false
}

// MaxComponentFields caps how many sub-fields one component may declare.
const MaxComponentFields = 30

// MaxComponentsPerTenant caps how many components one tenant may define.
const MaxComponentsPerTenant = 200

// ComponentSubFieldViolation names why a sub-field definition was refused:
// Code is the API error code, Attribute the offending attribute (empty for
// the nesting refusal, which is about the type).
type ComponentSubFieldViolation struct {
	Code      string
	Attribute string
	Detail    string
}

// CheckComponentSubField applies ADR-020 §2 to a sub-field definition that
// has already passed the ordinary field checks. It refuses nesting (a
// component inside a component) and the three attributes that only make
// sense on a top-level field: unique (nothing to index), read_roles and
// write_roles (permission is decided on the referencing field as a whole).
//
// A dynamic zone is refused under the SAME code as a component, not a code of
// its own. What is being refused is one thing — a component containing
// component-shaped values — and a zone is the more capable form of it, so a
// caller who reads the error and removes the nesting has done the right thing
// either way. A second code would make the two look like separate rules that
// could be lifted separately; they cannot.
func CheckComponentSubField(f Field) *ComponentSubFieldViolation {
	if f.Type == FieldTypeComponent || f.Type == FieldTypeDynamicZone {
		return &ComponentSubFieldViolation{
			Code:   "CONTENT_COMPONENT_NESTING_UNSUPPORTED",
			Detail: "a component cannot contain a " + f.Type + " field",
		}
	}
	refuse := func(attr string) *ComponentSubFieldViolation {
		return &ComponentSubFieldViolation{
			Code: "CONTENT_COMPONENT_FIELD_ATTRIBUTE_UNSUPPORTED", Attribute: attr,
			Detail: "component sub-fields cannot carry " + attr,
		}
	}
	if f.Unique {
		return refuse("unique")
	}
	if len(f.ReadRoles) > 0 {
		return refuse("read_roles")
	}
	if len(f.WriteRoles) > 0 {
		return refuse("write_roles")
	}
	return nil
}
