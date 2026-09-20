package domain

import (
	"bytes"
	"encoding/json"
	"sort"
)

// A schema artifact is a tenant's content types as a portable document: the
// same information GET /types returns, minus everything that belongs to one
// database rather than to the schema.
//
// It carries NO tenant id, NO row ids and NO timestamps, and each omission is
// load-bearing.
//
//   - A tenant in the file is a loaded gun. The file outlives anyone's memory
//     of which tenant it came from, so the target must come from the request's
//     X-Tenant-Id at the moment a person types it. Applying one artifact to ten
//     tenants is a normal use, not a misuse.
//   - ids and timestamps are facts about a database, not about a schema. Carry
//     them and the same schema exported from two machines never compares equal
//     — and comparing equal is the one capability this format exists to give.
//   - There is no checksum. A self-referential hash makes the file impossible
//     to hand-edit, and hand-editing a schema document is exactly why it is
//     worth having in git. Provenance lives outside the file, as G7's SYNC_STAMP
//     already does; an `exported_at` inside would make every re-export a false
//     diff, and false diffs train people to ignore real ones.
//
// See ADR-008.
const (
	ArtifactVersion1  = "1"
	KindContentSchema = "content-schema"
)

type Artifact struct {
	ArtifactVersion string `json:"artifact_version"`
	Kind            string `json:"kind"`
	// Components precede Types on the wire because a type may reference one and
	// the reader (and the plan) resolves them in document order (ADR-008
	// Amendment 2). omitempty keeps the export of a tenant that has none
	// byte-identical with what it was before components existed.
	Components []ArtifactComponent `json:"components,omitempty"`
	Types      []ArtifactType      `json:"types"`
}

// ArtifactComponent is one reusable component (ADR-020): a named, ordered
// list of sub-fields that a type field of type "component" embeds by name.
type ArtifactComponent struct {
	Name   string          `json:"name"`
	Label  string          `json:"label,omitempty"`
	Fields []ArtifactField `json:"fields"`
}

// ArtifactType carries the type's own DATA-level permission alongside its
// fields. It belongs in the file for the reason the whole format exists: who may
// read a collection is the change on a schema diff that has a security answer,
// and a permission that lived outside the artifact would be invisible to the
// review the artifact is for.
//
// All three lists are omitempty, matching the field lists and for the same
// reason: empty MEANS unrestricted, so an absent key and an empty array say the
// same thing, and omitting them keeps every schema written before 000027
// byte-identical to its re-export.
//
// readable/writable/own_only — the per-CALLER answers on ContentTypeDTO — are
// deliberately absent. They are facts about a request, not about a schema; an
// artifact that carried them would export differently for each person who ran
// the export, which is the one property this format cannot lose.
type ArtifactType struct {
	Name         string          `json:"name"`
	Label        string          `json:"label,omitempty"`
	ReadRoles    []string        `json:"read_roles,omitempty"`
	WriteRoles   []string        `json:"write_roles,omitempty"`
	OwnOnlyRoles []string        `json:"own_only_roles,omitempty"`
	Fields       []ArtifactField `json:"fields"`
}

// ArtifactField mirrors FieldDTO minus nothing — the wire shape a caller
// already knows. required and multiple are always written: on a bool, omitempty
// makes `false` indistinguishable from "this writer did not know about the
// flag", and a reader that has to guess will guess wrong.
//
// read_roles and write_roles are omitempty, and that asymmetry with the two
// bools is deliberate rather than sloppy: empty MEANS unrestricted (see
// field_permission.go), so an absent key and an empty array say the same thing,
// and omitting it keeps every schema written before permissions existed
// byte-identical to its re-export. A `false` bool, by contrast, is a real state
// distinct from "this writer did not know about the flag".
type ArtifactField struct {
	Key            string   `json:"key"`
	Type           string   `json:"type"`
	Label          string   `json:"label,omitempty"`
	Required       bool     `json:"required"`
	Multiple       bool     `json:"multiple"`
	EnumValues     []string `json:"enum_values,omitempty"`
	ReadRoles      []string `json:"read_roles,omitempty"`
	WriteRoles     []string `json:"write_roles,omitempty"`
	RelationEntity string   `json:"relation_entity,omitempty"`
	// Component names the reusable component a "component" field embeds
	// (ADR-020). Empty for every other type.
	Component string `json:"component,omitempty"`
	// Components is the allowed-list of a "dynamiczone" field (ADR-020
	// Amendment 1): which components its items may be. Empty for every other
	// type, and omitempty for the reason the role lists are — absent and unset
	// say the same thing, so a schema written before zones existed re-exports
	// byte-identical.
	//
	// ORDER IS PRESERVED, not sorted, for the reason field order is: it is the
	// order an editor's block menu offers, which the author chose. A reorder is
	// therefore a real change the diff reports, exactly as an enum reorder is.
	Components []string `json:"components,omitempty"`
	// Description is the author's sentence about the field. omitempty for the
	// reason the role lists are: absent and empty say the same thing, so a
	// schema written before descriptions were carried re-exports
	// byte-identically.
	Description string `json:"description,omitempty"`
	// The constraint attributes are all omitempty for the reason read_roles
	// is: every one of them is opt-in, so absent and unset say the same thing,
	// and a schema written before constraints existed re-exports byte-identical.
	FieldConstraints
}

// NewArtifact projects content types into an artifact.
//
// Types are SORTED by name; fields and enum values are NOT. The asymmetry is
// the point. A tenant's type order is an accident of ORDER BY that nothing
// owns, so sorting removes it from every diff. A type's field order is part of
// its definition — AddField appends, and the DTO's order is the order the admin
// form renders — so sorting it would change what a user sees. Enum order is
// editable on purpose (ADR-007 allows reordering freely), which makes a reorder
// a real change rather than noise.
func NewArtifact(types []*ContentType) Artifact {
	return NewSchemaArtifact(types, nil)
}

// NewSchemaArtifact is NewArtifact with the tenant's components as well. Both
// lists are sorted by name so the export is deterministic (ADR-008 §2).
func NewSchemaArtifact(types []*ContentType, components []*Component) Artifact {
	out := Artifact{ArtifactVersion: ArtifactVersion1, Kind: KindContentSchema, Types: make([]ArtifactType, 0, len(types))}
	for _, c := range components {
		ac := ArtifactComponent{Name: c.Name, Label: c.Label, Fields: make([]ArtifactField, 0, len(c.Fields))}
		for _, f := range c.Fields {
			ac.Fields = append(ac.Fields, artifactFieldOf(f))
		}
		out.Components = append(out.Components, ac)
	}
	sort.Slice(out.Components, func(i, j int) bool { return out.Components[i].Name < out.Components[j].Name })
	for _, ct := range types {
		at := ArtifactType{Name: ct.Name, Label: ct.Label, Fields: make([]ArtifactField, 0, len(ct.Fields))}
		// Nil, not empty, when a list is unset: the JSON omits it and the
		// round-trip test can compare structurally. An empty slice would
		// serialise as [] and read back as nil, which is the kind of
		// difference that makes a diff report a change nobody made.
		at.ReadRoles = copyIfAny(ct.ReadRoles)
		at.WriteRoles = copyIfAny(ct.WriteRoles)
		at.OwnOnlyRoles = copyIfAny(ct.OwnOnlyRoles)
		for _, f := range ct.Fields {
			at.Fields = append(at.Fields, artifactFieldOf(f))
		}
		out.Types = append(out.Types, at)
	}
	sort.Slice(out.Types, func(i, j int) bool { return out.Types[i].Name < out.Types[j].Name })
	return out
}

// artifactFieldOf is the one place a stored field becomes its artifact shape,
// shared by type fields and component sub-fields so the two cannot drift.
func artifactFieldOf(f Field) ArtifactField {
	af := ArtifactField{
		Key: f.Key, Type: f.Type, Label: f.Label,
		Required: f.Required, Multiple: f.Multiple,
		RelationEntity:   f.RelationEntity,
		Component:        f.ComponentName,
		Description:      f.Description,
		FieldConstraints: f.FieldConstraints,
	}
	af.EnumValues = copyIfAny(f.EnumValues)
	af.Components = copyIfAny(f.ZoneComponents)
	af.ReadRoles = copyIfAny(f.ReadRoles)
	af.WriteRoles = copyIfAny(f.WriteRoles)
	return af
}

// copyIfAny returns a detached copy of a non-empty slice and nil otherwise, so
// a nil and an empty slice both render as an absent key.
//
// The absent-key convention is what makes the format stable: the loader
// normalises the other direction, so a schema round-trips byte-identically
// whichever of the two the repository handed back. It is a function rather than
// three inline `if len(x) > 0` blocks because it now runs six times per type,
// and the failure mode of forgetting one is a permanent false diff.
func copyIfAny(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return append([]string(nil), in...)
}

// MarshalArtifact renders an artifact in its canonical form: two-space indent,
// struct field order, trailing newline, and no HTML escaping — `&` in a label
// must stay `&` or the bytes differ from what a person would write by hand.
//
// Canonical means a re-export of an unchanged schema is byte-identical to the
// file it came from. That is the acceptance bar, not a nicety: it is what makes
// `git diff` on a schema mean something.
func MarshalArtifact(a Artifact) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(a); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
