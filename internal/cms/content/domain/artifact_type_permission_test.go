package domain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A type's DATA-level permission in the artifact. The properties asserted here
// are the two the format exists for: an unchanged schema re-exports
// byte-identically, and a change that widens access is VISIBLE in the plan.

// An undeclared type must not grow three empty arrays. pgx scans `{}` into a
// non-nil empty slice, so without the normalisation the first re-export after
// migration 000027 would be a diff of pure noise on every type a tenant has —
// and false diffs train people to ignore real ones.
func TestArtifactType_UndeclaredPermissionsAreAbsent(t *testing.T) {
	art := NewArtifact([]*ContentType{{
		Name:         "article",
		ReadRoles:    []string{},
		WriteRoles:   []string{},
		OwnOnlyRoles: []string{},
		Fields:       []Field{{Key: "title", Type: FieldTypeString, EnumValues: []string{}}},
	}})
	b, err := MarshalArtifact(art)
	require.NoError(t, err)

	for _, key := range []string{"read_roles", "write_roles", "own_only_roles"} {
		assert.NotContains(t, string(b), key,
			"an empty list means unrestricted and must render as an absent key")
	}
}

func TestArtifactType_DeclaredPermissionsSurviveTheRoundTrip(t *testing.T) {
	art := NewArtifact([]*ContentType{{
		Name:         "ledger",
		ReadRoles:    []string{"admin", "owner"},
		WriteRoles:   []string{"owner"},
		OwnOnlyRoles: []string{"editor"},
		Fields:       []Field{{Key: "memo", Type: FieldTypeString}},
	}})
	require.Len(t, art.Types, 1)
	assert.Equal(t, []string{"admin", "owner"}, art.Types[0].ReadRoles)
	assert.Equal(t, []string{"owner"}, art.Types[0].WriteRoles)
	assert.Equal(t, []string{"editor"}, art.Types[0].OwnOnlyRoles)

	b, err := MarshalArtifact(art)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"own_only_roles"`)

	// NewArtifactFromTypes is the re-ordering pass applied to an artifact
	// assembled from somewhere other than the repository. It must not be lossy —
	// a helper that silently drops permissions is a loaded gun for its next
	// caller.
	again := NewArtifactFromTypes(art)
	assert.Equal(t, art, again, "the canonical-ordering pass must carry every permission through")
}

// A permission change is reported SEPARATELY, per list, with the before and
// after. A reviewer reading a plan is asking "does this file widen access?", and
// "the type changed" does not answer it.
func TestDiffSchemas_ReportsEachTypePermissionListSeparately(t *testing.T) {
	from := Artifact{Types: []ArtifactType{{Name: "ledger", Fields: []ArtifactField{{Key: "memo", Type: FieldTypeString}}}}}
	to := Artifact{Types: []ArtifactType{{
		Name:       "ledger",
		ReadRoles:  []string{"owner"},
		WriteRoles: []string{"owner"},
		Fields:     []ArtifactField{{Key: "memo", Type: FieldTypeString}},
	}}}

	changes := DiffSchemas(from, to)
	var details []string
	for _, c := range changes {
		require.Equal(t, OpUpdateType, c.Op)
		require.Equal(t, GradeAdditive, c.Grade, "a permission change invalidates no stored value")
		details = append(details, c.Detail)
	}
	require.Len(t, details, 2, "one line per list, not one line for 'permissions changed'")
	assert.Contains(t, strings.Join(details, "\n"), "read_roles unrestricted → [owner]")
	assert.Contains(t, strings.Join(details, "\n"), "write_roles unrestricted → [owner]")
}

// Tightening confinement is GUARDED and names the refusal a database can raise;
// relaxing it is additive. Both halves are reported — a plan silent about a
// revoke would let a file quietly un-confine a role.
func TestDiffSchemas_ConfinementIsGuardedOnlyWhenItTightens(t *testing.T) {
	none := ArtifactType{Name: "article", Fields: []ArtifactField{{Key: "title", Type: FieldTypeString}}}
	confined := none
	confined.OwnOnlyRoles = []string{"editor"}

	tighten := DiffSchemas(Artifact{Types: []ArtifactType{none}}, Artifact{Types: []ArtifactType{confined}})
	require.Len(t, tighten, 1)
	assert.Equal(t, GradeGuarded, tighten[0].Grade)
	assert.Equal(t, "CONTENT_ENTRY_AUTHOR_MISSING", tighten[0].Code,
		"the plan must name the 409 an apply would hit, so a blocked step can be resolved")

	relax := DiffSchemas(Artifact{Types: []ArtifactType{confined}}, Artifact{Types: []ArtifactType{none}})
	require.Len(t, relax, 1)
	assert.Equal(t, GradeAdditive, relax[0].Grade, "un-confining can hide nothing new")
	assert.Empty(t, relax[0].Code)
}

// own_only_roles has the OPPOSITE polarity to the other two lists: it names who
// is CONFINED, not who is allowed. Rendering an empty one as "unrestricted"
// would make turning confinement OFF read identically to opening a read list.
func TestDiffSchemas_ConfinementRendersWithItsOwnPolarity(t *testing.T) {
	confined := ArtifactType{
		Name: "article", OwnOnlyRoles: []string{"editor"},
		Fields: []ArtifactField{{Key: "title", Type: FieldTypeString}},
	}
	none := ArtifactType{Name: "article", Fields: confined.Fields}

	changes := DiffSchemas(Artifact{Types: []ArtifactType{confined}}, Artifact{Types: []ArtifactType{none}})
	require.Len(t, changes, 1)
	assert.Contains(t, changes[0].Detail, "confined to own entries")
	assert.Contains(t, changes[0].Detail, "nobody confined")
	assert.NotContains(t, changes[0].Detail, "unrestricted",
		"an empty own_only_roles restricts nobody — saying 'unrestricted' describes the wrong thing")
}

// A brand-new type carries its permissions with it, so creating a restricted
// collection from a file is one step rather than create-then-lock.
func TestDiffSchemas_NewTypeCarriesItsPermissions(t *testing.T) {
	to := Artifact{Types: []ArtifactType{{
		Name: "ledger", ReadRoles: []string{"owner"},
		Fields: []ArtifactField{{Key: "memo", Type: FieldTypeString}},
	}}}
	changes := DiffSchemas(Artifact{}, to)
	require.Len(t, changes, 1)
	assert.Equal(t, OpCreateType, changes[0].Op)
	assert.Equal(t, GradeAdditive, changes[0].Grade)
}
