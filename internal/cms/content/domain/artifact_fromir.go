package domain

import (
	"encoding/json"
	"fmt"
)

// Reading spec-generator's admin_app_schema.json.
//
// The IR is an INPUT DIALECT, not this format. An artifact describes one
// tenant's schema state; the IR describes a whole app's design, so a generator
// emitting artifacts would have to know about tenant boundaries and a CMS
// consuming IR directly would have to know about ui_bindings. The conversion
// therefore lives here, on the import side, as a pure function.
//
// What it cannot carry, it REPORTS. A silent loss is the only kind that
// matters: the caller can decide whether a dropped state machine is acceptable,
// but only if they are told. Same principle ADR-006 Amendment 1b established for
// refusals — nothing important happens without saying its own name.

// Dropped is one property the CMS has no home for.
type Dropped struct {
	Type   string `json:"type,omitempty"`
	Field  string `json:"field,omitempty"`
	What   string `json:"what"`
	Reason string `json:"reason"`
}

type irField struct {
	Key            string   `json:"key"`
	Type           string   `json:"type"`
	Label          string   `json:"label"`
	Required       bool     `json:"required"`
	Multiple       bool     `json:"multiple"`
	EnumValues     []string `json:"enum_values"`
	RelationEntity string   `json:"relation_entity"`
	Description    string   `json:"description"`
}

type irEntity struct {
	Name       string    `json:"name"`
	Label      string    `json:"label"`
	Fields     []irField `json:"fields"`
	StateModel *struct {
		Field       string            `json:"field"`
		Initial     string            `json:"initial"`
		States      []string          `json:"states"`
		Transitions []json.RawMessage `json:"transitions"`
	} `json:"state_model"`
}

type irDoc struct {
	Entities    []irEntity                 `json:"entities"`
	UIBindings  map[string]json.RawMessage `json:"ui_bindings"`
	AppID       string                     `json:"app_id"`
	Locale      string                     `json:"locale"`
	SchemaField string                     `json:"version"`
}

// FromIR converts an admin_app_schema document into an artifact, returning
// every property it could not carry.
//
// app_id and locale are dropped without comment, and that is the one silence
// here: the tenant comes from the request (never from a file), and locale in
// this CMS is a property of an entry rather than of a schema. Neither is a loss
// of meaning — there is nothing for a caller to decide about.
func FromIR(raw []byte) (Artifact, []Dropped, error) {
	var doc irDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Artifact{}, nil, fmt.Errorf("parse admin_app_schema: %w", err)
	}
	if len(doc.Entities) == 0 {
		return Artifact{}, nil, fmt.Errorf("admin_app_schema has no entities")
	}
	var dropped []Dropped
	art := Artifact{ArtifactVersion: ArtifactVersion1, Kind: KindContentSchema}

	for _, e := range doc.Entities {
		at := ArtifactType{Name: e.Name, Label: e.Label}
		for _, f := range e.Fields {
			at.Fields = append(at.Fields, ArtifactField{
				Key: f.Key, Type: f.Type, Label: f.Label,
				Required: f.Required, Multiple: f.Multiple,
				EnumValues: f.EnumValues, RelationEntity: f.RelationEntity,
			})
			if f.Description != "" {
				dropped = append(dropped, Dropped{
					Type: e.Name, Field: f.Key, What: "description",
					Reason: "the content model has no field description; inert documentation, nothing depends on it",
				})
			}
		}
		if e.StateModel != nil {
			// The state SET survives — it is the enum_values of the field the
			// model names, and the generator's g4 gate already pins the two
			// equal. What is lost is the TRANSITIONS: this CMS has no state
			// machine and will not refuse an illegal move. The IR has always
			// said a downstream runtime enforces those, and the CMS is not it.
			dropped = append(dropped, Dropped{
				Type: e.Name, What: "state_model.transitions",
				Reason: fmt.Sprintf("%d transition(s) on %q dropped: the CMS stores the state set but enforces no transitions",
					len(e.StateModel.Transitions), e.StateModel.Field),
			})
		}
		art.Types = append(art.Types, at)
	}

	if len(doc.UIBindings) > 0 {
		// access looks like it maps onto delivery, and it does not: public
		// visibility here is publish state plus audience projection
		// (ADR-004/006), not a property of a type or field. Mapping it to
		// anything would build a path around the publish gate.
		dropped = append(dropped, Dropped{
			What: "ui_bindings",
			Reason: fmt.Sprintf("%d page binding(s) dropped, `access` included: the CMS does not own page routing, and visibility is publish state plus audience, not a schema property",
				len(doc.UIBindings)),
		})
	}
	// Sorting and field-order rules are the artifact's, not the IR's.
	return NewArtifactFromTypes(art), dropped, nil
}

// NewArtifactFromTypes applies the canonical ordering to an artifact assembled
// from somewhere other than the repository.
func NewArtifactFromTypes(a Artifact) Artifact {
	cts := make([]*ContentType, 0, len(a.Types))
	for _, t := range a.Types {
		// Every permission list is carried through. The IR declares none of them,
		// so this changes nothing on the FromIR path that owns this function
		// today — but a re-ordering helper that silently drops permissions is a
		// loaded gun for the next caller, and "it happened to have no callers that
		// set them" is not a property anyone can see from the call site.
		ct := &ContentType{
			Name: t.Name, Label: t.Label,
			ReadRoles: t.ReadRoles, WriteRoles: t.WriteRoles, OwnOnlyRoles: t.OwnOnlyRoles,
		}
		for _, f := range t.Fields {
			ct.Fields = append(ct.Fields, Field{
				Key: f.Key, Type: f.Type, Label: f.Label,
				Required: f.Required, Multiple: f.Multiple,
				EnumValues: f.EnumValues, RelationEntity: f.RelationEntity,
				ReadRoles: f.ReadRoles, WriteRoles: f.WriteRoles,
			})
		}
		cts = append(cts, ct)
	}
	return NewArtifact(cts)
}
