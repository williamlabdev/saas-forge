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
)

// SchemaChange is one step an apply would take, or refuse to take.
type SchemaChange struct {
	Op    ChangeOp    `json:"op"`
	Type  string      `json:"type"`
	Field string      `json:"field,omitempty"`
	Grade ChangeGrade `json:"grade"`
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
				if c := addFieldChange(t.Name, f); c.Grade == GradeRefused {
					out = append(out, c)
				}
			}
			continue
		}
		out = append(out, diffType(prev, t)...)
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
	return withRenameHints(out)
}

func diffType(from, to ArtifactType) []SchemaChange {
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
	fromFields := indexFields(from)
	toFields := indexFields(to)

	for _, f := range to.Fields {
		prev, existed := fromFields[f.Key]
		if !existed {
			out = append(out, addFieldChange(to.Name, f))
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

func addFieldChange(typeName string, f ArtifactField) SchemaChange {
	c := SchemaChange{
		Op: OpAddField, Type: typeName, Field: f.Key, Grade: GradeAdditive,
		Detail: fmt.Sprintf("new %s field", f.Type),
	}
	if !ValidFieldType(f.Type) {
		c.Grade, c.Code = GradeRefused, "CONTENT_FIELD_TYPE_UNSUPPORTED"
		c.Detail = fmt.Sprintf("unknown field type %q", f.Type)
		return c
	}
	if f.Multiple && !MultipleAllowedFor(f.Type) {
		c.Grade, c.Code = GradeRefused, "CONTENT_FIELD_MULTIPLE_UNSUPPORTED"
		c.Detail = fmt.Sprintf("%s fields cannot be multi-valued", f.Type)
		return c
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
	return out
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
	for _, c := range changes {
		switch c.Op {
		case OpAddField:
			adds[c.Type] = true
		case OpDeleteField:
			dels[c.Type] = true
		}
	}
	for i, c := range changes {
		if (c.Op == OpAddField || c.Op == OpDeleteField) && adds[c.Type] && dels[c.Type] {
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

func indexFields(t ArtifactType) map[string]ArtifactField {
	m := make(map[string]ArtifactField, len(t.Fields))
	for _, f := range t.Fields {
		m[f.Key] = f
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
