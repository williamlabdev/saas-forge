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
	ArtifactVersion string         `json:"artifact_version"`
	Kind            string         `json:"kind"`
	Types           []ArtifactType `json:"types"`
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
	out := Artifact{ArtifactVersion: ArtifactVersion1, Kind: KindContentSchema, Types: make([]ArtifactType, 0, len(types))}
	for _, ct := range types {
		at := ArtifactType{Name: ct.Name, Label: ct.Label, Fields: make([]ArtifactField, 0, len(ct.Fields))}
		// Same nil-versus-empty normalisation the field lists get below, and it
		// matters for the same reason: pgx scans a `{}` column into a non-nil empty
		// slice, so without this every unrestricted type — which is all of them
		// today — would grow three empty arrays and make the first re-export after
		// 000027 a diff of pure noise.
		at.ReadRoles = copyIfAny(ct.ReadRoles)
		at.WriteRoles = copyIfAny(ct.WriteRoles)
		at.OwnOnlyRoles = copyIfAny(ct.OwnOnlyRoles)
		for _, f := range ct.Fields {
			af := ArtifactField{
				Key: f.Key, Type: f.Type, Label: f.Label,
				Required: f.Required, Multiple: f.Multiple,
				RelationEntity: f.RelationEntity,
			}
			// A nil and an empty slice both render as absent, and the loader
			// normalises the other direction — so a type with no enum values
			// round-trips byte-identically whichever one the repository handed
			// back. The permission lists get the same treatment for the same
			// reason, and it matters more there: pgx scans a `{}` column into a
			// non-nil empty slice, so without this every unrestricted field —
			// which is all of them today — would grow two empty arrays in the
			// file and make the first re-export after 000026 a diff of noise.
			af.EnumValues = copyIfAny(f.EnumValues)
			af.ReadRoles = copyIfAny(f.ReadRoles)
			af.WriteRoles = copyIfAny(f.WriteRoles)
			at.Fields = append(at.Fields, af)
		}
		out.Types = append(out.Types, at)
	}
	sort.Slice(out.Types, func(i, j int) bool { return out.Types[i].Name < out.Types[j].Name })
	return out
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
