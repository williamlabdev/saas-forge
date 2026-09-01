package domain

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func ct(name string, fields ...Field) *ContentType {
	id := uuid.New()
	for i := range fields {
		fields[i].ContentTypeID = id
	}
	return &ContentType{ID: id, TenantID: "t", Name: name, Label: name + " label", Fields: fields}
}

func TestArtifactDropsWhatBelongsToOneDatabase(t *testing.T) {
	a := NewArtifact([]*ContentType{ct("post", Field{Key: "title", Type: FieldTypeString, Required: true})})
	b, err := MarshalArtifact(a)
	if err != nil {
		t.Fatal(err)
	}
	// Asserted on the BYTES, not on the struct. A struct assertion would pass
	// for a field that exists but is never serialised, and it is the file that
	// leaves the machine.
	for _, banned := range []string{"tenant", "\"id\"", "created_at", "updated_at", "content_type_id"} {
		if strings.Contains(string(b), banned) {
			t.Fatalf("artifact carries %q:\n%s", banned, b)
		}
	}
}

func TestArtifactSerialisationIsCanonical(t *testing.T) {
	// Types out of order, and a type whose field order is NOT alphabetical.
	a := NewArtifact([]*ContentType{
		ct("zebra", Field{Key: "b", Type: FieldTypeString}),
		ct("apple",
			Field{Key: "title", Type: FieldTypeString, Required: true},
			Field{Key: "body", Type: FieldTypeText},
			Field{Key: "status", Type: FieldTypeEnum, EnumValues: []string{"draft", "review", "live"}},
		),
	})

	if a.Types[0].Name != "apple" || a.Types[1].Name != "zebra" {
		t.Fatalf("types not sorted by name: %v", a.Types)
	}
	// Field order is the type's own definition order — it is what the admin
	// form renders — so sorting it would change what a user sees.
	got := []string{a.Types[0].Fields[0].Key, a.Types[0].Fields[1].Key, a.Types[0].Fields[2].Key}
	if got[0] != "title" || got[1] != "body" || got[2] != "status" {
		t.Fatalf("field order was rewritten: %v", got)
	}
	// Enum order likewise: ADR-007 makes reordering a legal edit, which makes it
	// meaningful rather than noise.
	if e := a.Types[0].Fields[2].EnumValues; e[0] != "draft" || e[2] != "live" {
		t.Fatalf("enum order was rewritten: %v", e)
	}

	first, err := MarshalArtifact(a)
	if err != nil {
		t.Fatal(err)
	}
	// Parse and re-render: the bytes must be a fixed point. This is the half of
	// the round-trip bar that needs no database — the other half (export,
	// apply, re-export) belongs to the apply suite.
	var back Artifact
	if err := json.Unmarshal(first, &back); err != nil {
		t.Fatal(err)
	}
	second, err := MarshalArtifact(back)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("not byte-identical across a parse:\n--- first\n%s\n--- second\n%s", first, second)
	}
	if !strings.HasSuffix(string(first), "}\n") {
		t.Fatal("no trailing newline — git diff needs one")
	}
}

// required and multiple must survive as explicit false. Omitting them would
// make "false" and "this writer did not know about the flag" the same bytes.
func TestArtifactWritesFalseFlagsExplicitly(t *testing.T) {
	b, _ := MarshalArtifact(NewArtifact([]*ContentType{ct("post", Field{Key: "title", Type: FieldTypeString})}))
	for _, want := range []string{`"required": false`, `"multiple": false`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %s:\n%s", want, b)
		}
	}
}

func find(t *testing.T, cs []SchemaChange, op ChangeOp, field string) SchemaChange {
	t.Helper()
	for _, c := range cs {
		if c.Op == op && c.Field == field {
			return c
		}
	}
	t.Fatalf("no %s change for field %q in %+v", op, field, cs)
	return SchemaChange{}
}

func TestDiffGradesAreTheAnswersADR007AlreadyGives(t *testing.T) {
	base := NewArtifact([]*ContentType{ct("post",
		Field{Key: "title", Type: FieldTypeString, Required: true},
		Field{Key: "status", Type: FieldTypeEnum, EnumValues: []string{"draft", "live"}},
		Field{Key: "note", Type: FieldTypeString, Required: true},
	)})

	t.Run("an unchanged artifact produces no work", func(t *testing.T) {
		if cs := DiffSchemas(base, base); len(cs) != 0 {
			t.Fatalf("idempotence broken: %+v", cs)
		}
	})

	t.Run("relaxing is additive, tightening is guarded", func(t *testing.T) {
		next := NewArtifact([]*ContentType{ct("post",
			Field{Key: "title", Type: FieldTypeString, Required: true},
			Field{Key: "status", Type: FieldTypeEnum, EnumValues: []string{"draft", "live", "archived"}},
			Field{Key: "note", Type: FieldTypeString}, // required relaxed
			Field{Key: "summary", Type: FieldTypeText, Required: true},
		)})
		cs := DiffSchemas(base, next)
		if c := find(t, cs, OpUpdateField, "note"); c.Grade != GradeAdditive {
			t.Fatalf("relaxing required should be additive, got %s", c.Grade)
		}
		if c := find(t, cs, OpUpdateField, "status"); c.Grade != GradeAdditive {
			t.Fatalf("widening an enum should be additive, got %s: %s", c.Grade, c.Detail)
		}
		// A new REQUIRED field is guarded, not additive: every existing entry
		// lacks the key, so all of them would fail validation.
		c := find(t, cs, OpAddField, "summary")
		if c.Grade != GradeGuarded || c.Code != "CONTENT_FIELD_REQUIRED_BACKFILL" {
			t.Fatalf("new required field should be guarded with a named code, got %s/%s", c.Grade, c.Code)
		}
	})

	t.Run("narrowing an enum names the 409 it will hit", func(t *testing.T) {
		next := NewArtifact([]*ContentType{ct("post",
			Field{Key: "title", Type: FieldTypeString, Required: true},
			Field{Key: "status", Type: FieldTypeEnum, EnumValues: []string{"draft"}},
			Field{Key: "note", Type: FieldTypeString, Required: true},
		)})
		c := find(t, DiffSchemas(base, next), OpUpdateField, "status")
		if c.Grade != GradeGuarded || c.Code != "CONTENT_ENUM_VALUE_IN_USE" {
			t.Fatalf("got %s/%s", c.Grade, c.Code)
		}
	})

	t.Run("the three immutable properties are refused by name", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			f    Field
			code string
		}{
			{"type", Field{Key: "title", Type: FieldTypeNumber, Required: true}, "CONTENT_FIELD_TYPE_IMMUTABLE"},
			{"multiple", Field{Key: "title", Type: FieldTypeString, Required: true, Multiple: true}, "CONTENT_FIELD_MULTIPLE_IMMUTABLE"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				next := NewArtifact([]*ContentType{ct("post", tc.f,
					Field{Key: "status", Type: FieldTypeEnum, EnumValues: []string{"draft", "live"}},
					Field{Key: "note", Type: FieldTypeString, Required: true},
				)})
				c := find(t, DiffSchemas(base, next), OpUpdateField, "title")
				if c.Grade != GradeRefused || c.Code != tc.code {
					t.Fatalf("got %s/%s, want refused/%s", c.Grade, c.Code, tc.code)
				}
				if c.Hint == "" {
					t.Fatal("a refusal with no way forward sends the caller to raw SQL")
				}
			})
		}
	})

	t.Run("an unknown field type is refused, not attempted", func(t *testing.T) {
		// `time` is the concrete case: the generator emitted it until today.
		next := NewArtifact([]*ContentType{ct("post",
			Field{Key: "title", Type: FieldTypeString, Required: true},
			Field{Key: "status", Type: FieldTypeEnum, EnumValues: []string{"draft", "live"}},
			Field{Key: "note", Type: FieldTypeString, Required: true},
			Field{Key: "slot", Type: "time"},
		)})
		c := find(t, DiffSchemas(base, next), OpAddField, "slot")
		if c.Grade != GradeRefused || c.Code != "CONTENT_FIELD_TYPE_UNSUPPORTED" {
			t.Fatalf("got %s/%s", c.Grade, c.Code)
		}
	})

	t.Run("deletions are reported but never inferred as safe", func(t *testing.T) {
		next := NewArtifact([]*ContentType{ct("post",
			Field{Key: "title", Type: FieldTypeString, Required: true},
			Field{Key: "status", Type: FieldTypeEnum, EnumValues: []string{"draft", "live"}},
		)})
		c := find(t, DiffSchemas(base, next), OpDeleteField, "note")
		if c.Grade != GradeGuarded || !strings.Contains(c.Detail, "prune") {
			t.Fatalf("a dropped field must say it needs prune: %+v", c)
		}
	})

	t.Run("a rename looks like delete-plus-add and says so", func(t *testing.T) {
		next := NewArtifact([]*ContentType{ct("post",
			Field{Key: "title", Type: FieldTypeString, Required: true},
			Field{Key: "status", Type: FieldTypeEnum, EnumValues: []string{"draft", "live"}},
			Field{Key: "remark", Type: FieldTypeString, Required: true}, // was `note`
		)})
		cs := DiffSchemas(base, next)
		for _, op := range []ChangeOp{OpAddField, OpDeleteField} {
			c := find(t, cs, op, map[ChangeOp]string{OpAddField: "remark", OpDeleteField: "note"}[op])
			if !strings.Contains(c.Hint, "rename") {
				t.Fatalf("%s carries no rename hint: %+v", op, c)
			}
		}
	})
}
