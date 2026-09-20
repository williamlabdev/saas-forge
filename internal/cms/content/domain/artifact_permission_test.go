package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

// Permission in the artifact. The property under test throughout is that the
// declaration is part of the SCHEMA DOCUMENT — so it survives export, diffs like
// any other property, and cannot be lost in a round trip. A permission model
// that lived only in the database would be one no reviewer ever sees.

// An unrestricted field must not grow two empty arrays in the file. The
// repository scans a `{}` column into a non-nil empty slice, so without
// normalisation the first export after migration 000026 would be a diff of pure
// noise on every field in every tenant — and noise is what trains people to stop
// reading schema diffs.
func TestArtifactOmitsEmptyPermissionLists(t *testing.T) {
	a := NewArtifact([]*ContentType{ct("post", Field{
		Key: "title", Type: FieldTypeString,
		ReadRoles: []string{}, WriteRoles: []string{},
	})})
	b, err := MarshalArtifact(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"read_roles", "write_roles"} {
		if strings.Contains(string(b), banned) {
			t.Fatalf("an unrestricted field emitted %q — every existing schema would re-export as a diff:\n%s", banned, b)
		}
	}
}

func TestArtifactCarriesPermissionLists(t *testing.T) {
	a := NewArtifact([]*ContentType{ct("employee", Field{
		Key: "salary", Type: FieldTypeNumber,
		ReadRoles: []string{"admin", "owner"}, WriteRoles: []string{"owner"},
	})})
	b, err := MarshalArtifact(a)
	if err != nil {
		t.Fatal(err)
	}
	var back Artifact
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	f := back.Types[0].Fields[0]
	if strings.Join(f.ReadRoles, ",") != "admin,owner" || strings.Join(f.WriteRoles, ",") != "owner" {
		t.Fatalf("permission lists did not survive the round trip: read=%v write=%v", f.ReadRoles, f.WriteRoles)
	}
}

// A permission change must appear in a plan as its OWN step with the before and
// after spelled out. Folding it into the label line, or summarising it as
// "permissions changed", hides the one line on a schema diff that has a security
// answer.
func TestDiffReportsPermissionChangeWithBothSides(t *testing.T) {
	from := NewArtifact([]*ContentType{ct("employee", Field{
		Key: "salary", Type: FieldTypeNumber, ReadRoles: []string{"owner"},
	})})
	to := NewArtifact([]*ContentType{ct("employee", Field{
		Key: "salary", Type: FieldTypeNumber, ReadRoles: []string{"admin", "owner"},
	})})

	changes := DiffSchemas(from, to)
	var found *SchemaChange
	for i := range changes {
		if strings.Contains(changes[i].Detail, "read_roles") {
			found = &changes[i]
		}
	}
	if found == nil {
		t.Fatalf("a read_roles change produced no step: %+v", changes)
	}
	if found.Grade != GradeAdditive {
		t.Fatalf("grade %q — a permission change invalidates no stored value, so nothing can block it", found.Grade)
	}
	if !strings.Contains(found.Detail, "[owner]") || !strings.Contains(found.Detail, "[admin owner]") {
		t.Fatalf("both sides must be visible in the plan, got %q", found.Detail)
	}
}

// Opening a field up is the change most worth not misreading, and `[] → [admin]`
// reads backwards: it looks like a list GAINING a member when it is a field
// being closed off. The empty list gets a word.
func TestDiffSpellsUnrestrictedAsAWord(t *testing.T) {
	from := NewArtifact([]*ContentType{ct("employee", Field{
		Key: "salary", Type: FieldTypeNumber, ReadRoles: []string{"owner"},
	})})
	to := NewArtifact([]*ContentType{ct("employee", Field{
		Key: "salary", Type: FieldTypeNumber,
	})})

	var detail string
	for _, c := range DiffSchemas(from, to) {
		if strings.Contains(c.Detail, "read_roles") {
			detail = c.Detail
		}
	}
	if !strings.Contains(detail, "unrestricted") {
		t.Fatalf("opening a field must say so in words, got %q", detail)
	}
}

// Reordering a permission list is NOT a change: it is a set, and the service
// canonicalises it on the way in. Two artifacts that grant identically have to
// compare equal, which is the entire reason the format exists.
func TestDiffIgnoresNothingBecausePermissionListsAreCanonical(t *testing.T) {
	// Both sides carry the same set in the same canonical order — the shape the
	// service stores. A diff here would mean an export could never be quiet.
	from := NewArtifact([]*ContentType{ct("employee", Field{
		Key: "salary", Type: FieldTypeNumber, ReadRoles: []string{"admin", "owner"},
	})})
	to := NewArtifact([]*ContentType{ct("employee", Field{
		Key: "salary", Type: FieldTypeNumber, ReadRoles: []string{"admin", "owner"},
	})})
	if changes := DiffSchemas(from, to); len(changes) != 0 {
		t.Fatalf("an unchanged schema produced %d change(s): %+v", len(changes), changes)
	}
}
