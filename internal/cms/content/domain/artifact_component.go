package domain

import "fmt"

// Component diffing (ADR-008 Amendment 2). A component is diffed the way a
// type is — label, then each sub-field added / changed / removed — with two
// differences that are ADR-020's, not this file's: a sub-field is held to
// CheckComponentSubField (no nesting, no unique, no per-field roles), and the
// guards run over the ITEMS of every entry of every referring type rather than
// over one type's rows. The second difference lives in the service
// (componentBlockingCount); here only the shape of the change is decided.

// createComponentChanges is OpCreateComponent plus a refusal per sub-field the
// create verb would reject, mirroring what DiffSchemas does for a new type.
func createComponentChanges(c ArtifactComponent) []SchemaChange {
	out := []SchemaChange{{
		Op: OpCreateComponent, Component: c.Name, Grade: GradeAdditive,
		Detail: fmt.Sprintf("new component with %d sub-field(s)", len(c.Fields)),
	}}
	switch {
	case !ValidFieldKey(c.Name):
		out[0].Grade, out[0].Code = GradeRefused, "CONTENT_COMPONENT_NAME_INVALID"
		out[0].Detail = fmt.Sprintf("component name %q is not a valid key", c.Name)
	case len(c.Fields) == 0:
		out[0].Grade, out[0].Code = GradeRefused, "CONTENT_COMPONENT_NO_FIELDS"
		out[0].Detail = "a component needs at least one sub-field"
	}
	for _, f := range c.Fields {
		if ch := addComponentFieldChange(c.Name, f); ch.Grade == GradeRefused {
			out = append(out, ch)
		}
	}
	return out
}

func diffComponent(from, to ArtifactComponent) []SchemaChange {
	var out []SchemaChange
	if from.Label != to.Label {
		out = append(out, SchemaChange{
			Op: OpUpdateComponent, Component: to.Name, Grade: GradeAdditive,
			Detail: fmt.Sprintf("label %q → %q", from.Label, to.Label),
		})
	}
	fromFields := indexFields(from.Fields)
	toFields := indexFields(to.Fields)
	for _, f := range to.Fields {
		prev, existed := fromFields[f.Key]
		if !existed {
			out = append(out, addComponentFieldChange(to.Name, f))
			continue
		}
		if v := CheckComponentSubField(subFieldOf(f)); v != nil {
			out = append(out, SchemaChange{
				Op: OpUpdateComponentField, Component: to.Name, Field: f.Key, Grade: GradeRefused,
				Detail: v.Detail, Code: v.Code,
			})
			continue
		}
		for _, ch := range diffField("", prev, f) {
			ch.Op, ch.Component = OpUpdateComponentField, to.Name
			out = append(out, ch)
		}
	}
	for _, f := range from.Fields {
		if _, kept := toFields[f.Key]; !kept {
			out = append(out, SchemaChange{
				Op: OpDeleteComponentField, Component: from.Name, Field: f.Key, Grade: GradeGuarded,
				Detail: "sub-field is absent from the artifact; removed only with prune",
				Code:   "CONTENT_FIELD_HAS_DATA",
			})
		}
	}
	return out
}

// addComponentFieldChange is addFieldChange under ADR-020's sub-field rules.
// The nesting / attribute check runs FIRST so a "component inside component"
// is reported as the nesting refusal it is, not as a missing reference.
func addComponentFieldChange(component string, f ArtifactField) SchemaChange {
	if v := CheckComponentSubField(subFieldOf(f)); v != nil {
		return SchemaChange{
			Op: OpAddComponentField, Component: component, Field: f.Key, Grade: GradeRefused,
			Detail: v.Detail, Code: v.Code,
		}
	}
	c := addFieldChange("", f, nil)
	c.Op, c.Component = OpAddComponentField, component
	return c
}

// subFieldOf carries just the attributes CheckComponentSubField looks at.
func subFieldOf(f ArtifactField) Field {
	return Field{
		Key: f.Key, Type: f.Type,
		ReadRoles: f.ReadRoles, WriteRoles: f.WriteRoles,
		FieldConstraints: f.FieldConstraints,
	}
}
