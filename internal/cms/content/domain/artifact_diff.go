package domain

import "fmt"

// Diffing two artifacts. A pure function over two documents — no database, no
// tenant — because it has three callers and only one of them has a connection:
// a CLI comparing two files, the plan endpoint comparing a file against a live
// export, and the tests.
//
// Every change carries a GRADE, and the grades are not new policy invented
// here. They are the three answers ADR-007 already gives to "what happens when
// you ask for this":
//
//   - additive — always applies.
//   - guarded — applies only if the stored data allows, which only a database
//     can answer. Each one names the 409 that will fire if it does not.
//   - refused — structurally impossible; it can never apply, whatever the data.
//
// A grade is a property of the PAIR, not of the operation: `required: true` is
// additive on a new field and guarded on an existing one.

type ChangeGrade string

const (
	GradeAdditive ChangeGrade = "additive"
	GradeGuarded  ChangeGrade = "guarded"
	GradeRefused  ChangeGrade = "refused"
)

type ChangeOp string

const (
	OpCreateType  ChangeOp = "create_type"
	OpDeleteType  ChangeOp = "delete_type"
	OpUpdateType  ChangeOp = "update_type"
	OpAddField    ChangeOp = "add_field"
	OpDeleteField ChangeOp = "delete_field"
	OpUpdateField ChangeOp = "update_field"

	// Component ops (ADR-008 Amendment 2). Same grades, same guards; the
	// subject is a component instead of a type, named in SchemaChange.Component.
	OpCreateComponent      ChangeOp = "create_component"
	OpUpdateComponent      ChangeOp = "update_component"
	OpDeleteComponent      ChangeOp = "delete_component"
	OpAddComponentField    ChangeOp = "add_component_field"
	OpUpdateComponentField ChangeOp = "update_component_field"
	OpDeleteComponentField ChangeOp = "delete_component_field"
)

// SchemaChange is one step an apply would take, or refuse to take.
type SchemaChange struct {
	Op    ChangeOp    `json:"op"`
	Type  string      `json:"type,omitempty"`
	Field string      `json:"field,omitempty"`
	Grade ChangeGrade `json:"grade"`
	// Component is the subject of a component op; empty for type ops. Exactly
	// one of Type / Component is set.
	Component string `json:"component,omitempty"`
	// Detail says what changes, in the words a person would use.
	Detail string `json:"detail"`
	// Code is the error a guarded change fails with, or the refusal a refused
	// change already carries. Empty for additive changes, which have neither.
	Code string `json:"code,omitempty"`
	// Hint is advice for a caller who is probably trying to do something the
	// artifact format cannot express. Today there is exactly one: a rename.
	Hint string `json:"hint,omitempty"`
	// RemovedValues carries the enum members this change drops. Structured
	// rather than left for a reader to recover from Detail: the planner needs
	// the surviving set to ask the database whether any entry still holds one,
	// and reparsing a human sentence to get it would break the first time
	// someone improved the wording.
	RemovedValues []string `json:"removed_values,omitempty"`
	// Constraints carries the TARGET constraint set of a guarded constraint
	// change, for the same reason RemovedValues exists: the planner has to ask
	// the database whether any stored value fails the new rule, and the rule
	// has to travel structured, not be recovered from Detail.
	Constraints *FieldConstraints `json:"constraints,omitempty"`
	// RemovedComponents carries the component names a dynamic zone's
	// allowed-list drops (ADR-020 Amendment 1). A SEPARATE field rather than
	// reuse of RemovedValues, even though both are "names this change removes":
	// the planner interprets RemovedValues as enum MEMBERS and asks the
	// database a question about enum columns with them. One slice serving two
	// questions is how a planner starts counting the wrong rows.
	RemovedComponents []string `json:"removed_components,omitempty"`
}

// DiffSchemas reports what it would take to turn `from` into `to`.
//
// Deletions are reported, never assumed. A type or field present in `from` and
// absent from `to` means "this document does not describe it" — an apply acts
// on those only when explicitly told to prune. The asymmetry matters because
// artifacts get run from scripts: a file someone trimmed by hand would
// otherwise be a data-destruction command.
func DiffSchemas(from, to Artifact) []SchemaChange {
	var out []SchemaChange
	fromTypes := indexTypes(from)
	toTypes := indexTypes(to)
	fromComps := indexComponents(from)
	toComps := indexComponents(to)

	// Components FIRST: a type field may embed one, so the component and its
	// sub-fields have to exist before the field that references it is added.
	// Component DELETIONS come last, after the type fields that referenced
	// them are gone — see the tail of this function.
	for _, c := range to.Components {
		prev, existed := fromComps[c.Name]
		if !existed {
			out = append(out, createComponentChanges(c)...)
			continue
		}
		out = append(out, diffComponent(prev, c)...)
	}

	for _, t := range to.Types {
		prev, existed := fromTypes[t.Name]
		if !existed {
			out = append(out, SchemaChange{
				Op: OpCreateType, Type: t.Name, Grade: GradeAdditive,
				Detail: fmt.Sprintf("new content type with %d field(s)", len(t.Fields)),
			})
			// The fields arrive WITH the type, so they are not listed as work —
			// listing them would double-count it. But they are still graded,
			// because a field the runtime cannot store makes the whole creation
			// impossible, and a plan that omitted it would promise an apply that
			// then failed inside a verb. Only the refusals are emitted: a plan
			// must be able to say "this will not work" before anything runs.
			for _, f := range t.Fields {
				if c := addFieldChange(t.Name, f, toComps); c.Grade == GradeRefused {
					out = append(out, c)
				}
			}
			continue
		}
		out = append(out, diffType(prev, t, toComps)...)
	}

	for _, t := range from.Types {
		if _, kept := toTypes[t.Name]; !kept {
			out = append(out, SchemaChange{
				Op: OpDeleteType, Type: t.Name, Grade: GradeGuarded,
				Detail: "type is absent from the artifact; removed only with prune",
				Code:   "CONTENT_TYPE_HAS_ENTRIES|CONTENT_TYPE_REFERENCED",
			})
		}
	}
	for _, c := range from.Components {
		if _, kept := toComps[c.Name]; !kept {
			out = append(out, SchemaChange{
				Op: OpDeleteComponent, Component: c.Name, Grade: GradeGuarded,
				Detail: "component is absent from the artifact; removed only with prune, and only once nothing references it",
				Code:   "CONTENT_COMPONENT_IN_USE",
			})
		}
	}
	return withRenameHints(out)
}

func diffType(from, to ArtifactType, comps map[string]ArtifactComponent) []SchemaChange {
	var out []SchemaChange
	if from.Label != to.Label {
		out = append(out, SchemaChange{
			Op: OpUpdateType, Type: to.Name, Grade: GradeAdditive,
			Detail: fmt.Sprintf("label %q → %q", from.Label, to.Label),
		})
	}
	// DATA-level permission, reported one list at a time and with the before and
	// after spelled out — the same treatment the field lists get, for the same
	// reason. A reviewer reading a plan is asking "does this file widen access?",
	// and "permissions changed" does not answer it. At the TYPE level the stakes
	// are a whole collection rather than one key, so folding these into the label
	// line would hide the largest access change the format can express.
	if !sameOrder(from.ReadRoles, to.ReadRoles) {
		out = append(out, SchemaChange{
			Op: OpUpdateType, Type: to.Name, Grade: GradeAdditive,
			Detail: fmt.Sprintf("read_roles %s → %s", describeRoles(from.ReadRoles), describeRoles(to.ReadRoles)),
		})
	}
	if !sameOrder(from.WriteRoles, to.WriteRoles) {
		out = append(out, SchemaChange{
			Op: OpUpdateType, Type: to.Name, Grade: GradeAdditive,
			Detail: fmt.Sprintf("write_roles %s → %s", describeRoles(from.WriteRoles), describeRoles(to.WriteRoles)),
		})
	}
	// Confinement is the one permission change in the format that the DATABASE
	// can refuse, and only in the tightening direction. Entries with no recorded
	// author match no author, so a newly confined role finds them gone rather
	// than forbidden — which is why it is graded GUARDED and named, while
	// relaxing confinement stays additive. Both halves are reported: a plan that
	// said nothing about a revoke would let a file quietly un-confine a role.
	if !sameOrder(from.OwnOnlyRoles, to.OwnOnlyRoles) {
		c := SchemaChange{
			Op: OpUpdateType, Type: to.Name, Grade: GradeAdditive,
			Detail: fmt.Sprintf("own_only_roles %s → %s", describeConfinement(from.OwnOnlyRoles), describeConfinement(to.OwnOnlyRoles)),
		}
		if len(missingFrom(to.OwnOnlyRoles, from.OwnOnlyRoles)) > 0 {
			c.Grade, c.Code = GradeGuarded, "CONTENT_ENTRY_AUTHOR_MISSING"
			c.Detail += "; entries with no recorded author would become invisible"
		}
		out = append(out, c)
	}
	fromFields := indexFields(from.Fields)
	toFields := indexFields(to.Fields)

	for _, f := range to.Fields {
		prev, existed := fromFields[f.Key]
		if !existed {
			out = append(out, addFieldChange(to.Name, f, comps))
			continue
		}
		out = append(out, diffField(to.Name, prev, f)...)
	}
	for _, f := range from.Fields {
		if _, kept := toFields[f.Key]; !kept {
			out = append(out, SchemaChange{
				Op: OpDeleteField, Type: from.Name, Field: f.Key, Grade: GradeGuarded,
				Detail: "field is absent from the artifact; removed only with prune",
				Code:   "CONTENT_FIELD_HAS_DATA",
			})
		}
	}
	return out
}

func addFieldChange(typeName string, f ArtifactField, comps map[string]ArtifactComponent) SchemaChange {
	c := SchemaChange{
		Op: OpAddField, Type: typeName, Field: f.Key, Grade: GradeAdditive,
		Detail: fmt.Sprintf("new %s field", f.Type),
	}
	if !ValidFieldType(f.Type) {
		c.Grade, c.Code = GradeRefused, "CONTENT_FIELD_TYPE_UNSUPPORTED"
		c.Detail = fmt.Sprintf("unknown field type %q", f.Type)
		return c
	}
	// A component field must name a component the ARTIFACT declares: the
	// artifact describes a whole state, and a reference into something it does
	// not carry is a reference the apply could not honour in order (ADR-008
	// Amendment 2).
	switch {
	case f.Type == FieldTypeComponent && f.Component == "":
		c.Grade, c.Code = GradeRefused, "CONTENT_COMPONENT_REQUIRED"
		c.Detail = "a component field must name the component it embeds"
		return c
	case f.Type == FieldTypeComponent:
		if _, ok := comps[f.Component]; !ok {
			c.Grade, c.Code = GradeRefused, "CONTENT_COMPONENT_NOT_FOUND"
			c.Detail = fmt.Sprintf("component %q is not declared in the artifact", f.Component)
			return c
		}
		c.Detail = fmt.Sprintf("new component field embedding %q", f.Component)
	case f.Component != "":
		c.Grade, c.Code = GradeRefused, "CONTENT_COMPONENT_NOT_APPLICABLE"
		c.Detail = fmt.Sprintf("%s fields do not embed a component", f.Type)
		return c
	}
	// The same three questions for a zone's allowed-list, and the same reason:
	// every name has to be one the ARTIFACT declares, or the apply could not
	// honour the reference in order.
	switch {
	case f.Type == FieldTypeDynamicZone && len(f.Components) == 0:
		c.Grade, c.Code = GradeRefused, "CONTENT_ZONE_COMPONENTS_REQUIRED"
		c.Detail = "a dynamiczone field must name the components it accepts"
		return c
	case f.Type == FieldTypeDynamicZone:
		if dup := firstDuplicateName(f.Components); dup != "" {
			c.Grade, c.Code = GradeRefused, "CONTENT_ZONE_COMPONENT_DUPLICATE"
			c.Detail = fmt.Sprintf("component %q is listed twice", dup)
			return c
		}
		for _, name := range f.Components {
			if _, ok := comps[name]; !ok {
				c.Grade, c.Code = GradeRefused, "CONTENT_COMPONENT_NOT_FOUND"
				c.Detail = fmt.Sprintf("component %q is not declared in the artifact", name)
				return c
			}
		}
		c.Detail = fmt.Sprintf("new dynamiczone field accepting %v", f.Components)
	case len(f.Components) > 0:
		c.Grade, c.Code = GradeRefused, "CONTENT_ZONE_COMPONENTS_NOT_APPLICABLE"
		c.Detail = fmt.Sprintf("%s fields do not declare a component list", f.Type)
		return c
	}
	if f.Multiple && !MultipleAllowedFor(f.Type) {
		c.Grade, c.Code = GradeRefused, "CONTENT_FIELD_MULTIPLE_UNSUPPORTED"
		c.Detail = fmt.Sprintf("%s fields cannot be multi-valued", f.Type)
		return c
	}
	// The same refusals buildField raises, by the same codes, so a plan never
	// promises an add that the apply then refuses.
	if cerr := ValidateFieldConstraints(f.Type, f.Multiple, f.FieldConstraints); cerr != nil {
		c.Grade, c.Code, c.Detail = GradeRefused, cerr.Code, cerr.Detail
		return c
	}
	if !f.IsZero() {
		c.Detail += " (" + f.Describe() + ")"
	}
	// A required field added to a type that already holds entries: every one of
	// them lacks the key, so all of them would fail validation.
	if f.Required {
		c.Grade, c.Code = GradeGuarded, "CONTENT_FIELD_REQUIRED_BACKFILL"
		c.Detail = fmt.Sprintf("new REQUIRED %s field; existing entries do not carry it", f.Type)
	}
	return c
}

func diffField(typeName string, from, to ArtifactField) []SchemaChange {
	var out []SchemaChange
	refuse := func(code, detail string) {
		out = append(out, SchemaChange{
			Op: OpUpdateField, Type: typeName, Field: to.Key, Grade: GradeRefused,
			Detail: detail, Code: code,
			Hint: "add a new field, migrate the content, then delete the old one — the three steps ADR-007 leaves open",
		})
	}
	// The three immutable properties. Refused by name rather than lumped
	// together, exactly as UpdateFieldInput refuses them.
	if from.Type != to.Type {
		refuse("CONTENT_FIELD_TYPE_IMMUTABLE", fmt.Sprintf("type %s → %s", from.Type, to.Type))
	}
	if from.Multiple != to.Multiple {
		refuse("CONTENT_FIELD_MULTIPLE_IMMUTABLE", fmt.Sprintf("multiple %t → %t", from.Multiple, to.Multiple))
	}
	if from.RelationEntity != to.RelationEntity {
		refuse("CONTENT_FIELD_RELATION_IMMUTABLE", fmt.Sprintf("relation_entity %q → %q", from.RelationEntity, to.RelationEntity))
	}
	if from.Component != to.Component {
		refuse("CONTENT_FIELD_COMPONENT_IMMUTABLE", fmt.Sprintf("component %q → %q", from.Component, to.Component))
	}
	if len(out) > 0 {
		// A field that cannot be reshaped is not worth reporting label churn on.
		return out
	}

	if from.Label != to.Label {
		out = append(out, SchemaChange{
			Op: OpUpdateField, Type: typeName, Field: to.Key, Grade: GradeAdditive,
			Detail: fmt.Sprintf("label %q → %q", from.Label, to.Label),
		})
	}
	// A description is documentation, so it is graded exactly as a label is:
	// additive, invalidating no stored value. It is reported on its own line
	// rather than folded into the label's because a reviewer reads the plan to
	// see WHAT the file says differently, and a sentence changing is a
	// different edit from a name changing.
	if from.Description != to.Description {
		out = append(out, SchemaChange{
			Op: OpUpdateField, Type: typeName, Field: to.Key, Grade: GradeAdditive,
			Detail: fmt.Sprintf("description %q → %q", from.Description, to.Description),
		})
	}
	switch {
	case !from.Required && to.Required:
		out = append(out, SchemaChange{
			Op: OpUpdateField, Type: typeName, Field: to.Key, Grade: GradeGuarded,
			Detail: "required tightened; entries missing this field would be blocked",
			Code:   "CONTENT_FIELD_REQUIRED_BACKFILL",
		})
	case from.Required && !to.Required:
		out = append(out, SchemaChange{
			Op: OpUpdateField, Type: typeName, Field: to.Key, Grade: GradeAdditive,
			Detail: "required relaxed",
		})
	}
	// Permission changes are ADDITIVE in the grading sense — they invalidate no
	// stored value, so no count of entries can block them and there is nothing
	// for the database to be asked. But they are reported SEPARATELY per list,
	// and with the before and after spelled out, because the grade is about
	// applicability and a reviewer reading a plan is asking a different question:
	// "does this file widen access?" Folding them into the label line, or
	// summarising them as "permissions changed", would hide the one change on a
	// schema diff that has a security answer.
	if !sameOrder(from.ReadRoles, to.ReadRoles) {
		out = append(out, SchemaChange{
			Op: OpUpdateField, Type: typeName, Field: to.Key, Grade: GradeAdditive,
			Detail: fmt.Sprintf("read_roles %s → %s", describeRoles(from.ReadRoles), describeRoles(to.ReadRoles)),
		})
	}
	if !sameOrder(from.WriteRoles, to.WriteRoles) {
		out = append(out, SchemaChange{
			Op: OpUpdateField, Type: typeName, Field: to.Key, Grade: GradeAdditive,
			Detail: fmt.Sprintf("write_roles %s → %s", describeRoles(from.WriteRoles), describeRoles(to.WriteRoles)),
		})
	}
	// Only REMOVALS from an enum can brick stored data. Additions and reorders
	// are free, so they are graded additive and not conflated with the removal.
	if removed := missingFrom(from.EnumValues, to.EnumValues); len(removed) > 0 {
		out = append(out, SchemaChange{
			Op: OpUpdateField, Type: typeName, Field: to.Key, Grade: GradeGuarded,
			Detail:        fmt.Sprintf("enum values removed: %v", removed),
			Code:          "CONTENT_ENUM_VALUE_IN_USE",
			RemovedValues: removed,
		})
	} else if !sameOrder(from.EnumValues, to.EnumValues) {
		out = append(out, SchemaChange{
			Op: OpUpdateField, Type: typeName, Field: to.Key, Grade: GradeAdditive,
			Detail: "enum values added or reordered",
		})
	}
	// A dynamic zone's allowed-list is graded exactly as an enum is, and for
	// exactly the same reason: only REMOVALS can strand stored data (items of a
	// component the field no longer accepts), so additions and reorders are
	// free. The difference is which question the planner then asks the
	// database — "does any entry hold an ITEM of this component in this field",
	// which is why the names travel under their own key.
	if removed := missingFrom(from.Components, to.Components); len(removed) > 0 {
		out = append(out, SchemaChange{
			Op: OpUpdateField, Type: typeName, Field: to.Key, Grade: GradeGuarded,
			Detail:            fmt.Sprintf("dynamiczone components removed: %v", removed),
			Code:              "CONTENT_ZONE_COMPONENT_IN_USE",
			RemovedComponents: removed,
		})
	} else if !sameOrder(from.Components, to.Components) {
		out = append(out, SchemaChange{
			Op: OpUpdateField, Type: typeName, Field: to.Key, Grade: GradeAdditive,
			Detail: "dynamiczone components added or reordered",
		})
	}
	out = append(out, diffConstraints(typeName, from, to)...)
	return out
}

// diffConstraints grades a change to the constraint set the way `required` is
// graded: tightening is guarded by the count of stored values that would fail,
// relaxing is additive. Uniqueness is reported on its own line with its own
// code, because the question it asks the database (are there duplicates?) is a
// different question from the one the other four ask (does any value fail?),
// and the apply answers each with a different repository call.
func diffConstraints(typeName string, from, to ArtifactField) []SchemaChange {
	var out []SchemaChange
	if cerr := ValidateFieldConstraints(to.Type, to.Multiple, to.FieldConstraints); cerr != nil {
		return []SchemaChange{{
			Op: OpUpdateField, Type: typeName, Field: to.Key, Grade: GradeRefused,
			Detail: cerr.Detail, Code: cerr.Code,
		}}
	}
	switch {
	case !from.Unique && to.Unique:
		target := to.FieldConstraints
		out = append(out, SchemaChange{
			Op: OpUpdateField, Type: typeName, Field: to.Key, Grade: GradeGuarded,
			Detail:      "unique tightened; entries sharing a value would be blocked",
			Code:        "CONTENT_FIELD_UNIQUE_DUPLICATES",
			Constraints: &target,
		})
	case from.Unique && !to.Unique:
		out = append(out, SchemaChange{
			Op: OpUpdateField, Type: typeName, Field: to.Key, Grade: GradeAdditive,
			Detail: "unique relaxed",
		})
	}
	fromRules, toRules := from.FieldConstraints, to.FieldConstraints
	fromRules.Unique, toRules.Unique = false, false
	if fromRules.Equal(toRules) {
		return out
	}
	describe := func(c FieldConstraints) string {
		if c.IsZero() {
			return "none"
		}
		return c.Describe()
	}
	detail := fmt.Sprintf("constraints %s → %s", describe(fromRules), describe(toRules))
	if toRules.Tightens(fromRules) {
		target := to.FieldConstraints
		out = append(out, SchemaChange{
			Op: OpUpdateField, Type: typeName, Field: to.Key, Grade: GradeGuarded,
			Detail:      detail + "; entries holding a value that fails would be blocked",
			Code:        "CONTENT_FIELD_CONSTRAINT_BACKFILL",
			Constraints: &target,
		})
	} else {
		out = append(out, SchemaChange{
			Op: OpUpdateField, Type: typeName, Field: to.Key, Grade: GradeAdditive,
			Detail: detail + " (relaxed)",
		})
	}
	return out
}

// firstDuplicateName returns the first name that appears twice, or "" — the
// domain-side twin of the service's firstDuplicate, kept here so the diff can
// refuse a bad allowed-list without importing the service.
func firstDuplicateName(names []string) string {
	seen := make(map[string]struct{}, len(names))
	for _, n := range names {
		if _, dup := seen[n]; dup {
			return n
		}
		seen[n] = struct{}{}
	}
	return ""
}

// withRenameHints annotates the one thing an artifact provably cannot express.
//
// A rename and a delete-plus-add are the SAME pair of states, so the diff
// cannot tell them apart and an apply must never guess — ADR-007 records that
// the delete-plus-add spelling destroys every stored value. What is left is to
// say so, on the changes a renamer would be staring at, because the alternative
// is that they conclude the tool is broken and go edit SQL.
func withRenameHints(changes []SchemaChange) []SchemaChange {
	adds, dels := map[string]bool{}, map[string]bool{}
	owner := func(c SchemaChange) string {
		if c.Component != "" {
			return "component:" + c.Component
		}
		return "type:" + c.Type
	}
	for _, c := range changes {
		switch c.Op {
		case OpAddField, OpAddComponentField:
			adds[owner(c)] = true
		case OpDeleteField, OpDeleteComponentField:
			dels[owner(c)] = true
		}
	}
	for i, c := range changes {
		isAdd := c.Op == OpAddField || c.Op == OpAddComponentField
		isDel := c.Op == OpDeleteField || c.Op == OpDeleteComponentField
		if (isAdd || isDel) && adds[owner(c)] && dels[owner(c)] {
			changes[i].Hint = "one field added and another dropped on the same type: if this is a RENAME, use the rename verb — an artifact describes states, and applying this pair would destroy the stored values"
		}
	}
	return changes
}

// describeRoles renders a permission list for a human reading a plan. The empty
// list gets a WORD rather than `[]`, because "[] → [admin]" reads as a list
// gaining a member when it is in fact a field being closed off, and the reverse
// pair is a field being opened to everyone — the change most worth not
// misreading in a plan output.
func describeRoles(roles []string) string {
	if len(roles) == 0 {
		return "unrestricted"
	}
	return fmt.Sprintf("%v", roles)
}

// describeConfinement renders own_only_roles, which has the OPPOSITE polarity to
// the other two lists: it names who is CONFINED, not who is allowed. Reusing
// describeRoles would print "unrestricted" for an empty list, which is true of
// the collection and reads as a statement about the list — so a plan turning
// confinement OFF and one turning a read list ON would render identically.
func describeConfinement(roles []string) string {
	if len(roles) == 0 {
		return "nobody confined"
	}
	return fmt.Sprintf("%v confined to own entries", roles)
}

func indexTypes(a Artifact) map[string]ArtifactType {
	m := make(map[string]ArtifactType, len(a.Types))
	for _, t := range a.Types {
		m[t.Name] = t
	}
	return m
}

func indexFields(fields []ArtifactField) map[string]ArtifactField {
	m := make(map[string]ArtifactField, len(fields))
	for _, f := range fields {
		m[f.Key] = f
	}
	return m
}

func indexComponents(a Artifact) map[string]ArtifactComponent {
	m := make(map[string]ArtifactComponent, len(a.Components))
	for _, c := range a.Components {
		m[c.Name] = c
	}
	return m
}

func sameOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// missingFrom returns the members of old absent from next. Mirrors the
// service-side helper of the same name; the duplication is deliberate — domain
// must not import service, and one shared copy would invert that dependency.
func missingFrom(old, next []string) []string {
	keep := make(map[string]struct{}, len(next))
	for _, v := range next {
		keep[v] = struct{}{}
	}
	var gone []string
	for _, v := range old {
		if _, ok := keep[v]; !ok {
			gone = append(gone, v)
		}
	}
	return gone
}
