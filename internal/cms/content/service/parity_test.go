package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/cms/content/contract"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// TestFieldTypeParity asserts the content domain's legal field_type set is
// exactly the EntityField.type enum in the vendored admin-app.schema.json.
// The frontend schema is the single source of truth; this test reads it (rather
// than hand-copying the list) so the two can never silently diverge.
func TestFieldTypeParity(t *testing.T) {
	var schema struct {
		Defs struct {
			EntityField struct {
				Properties struct {
					Type struct {
						Enum []string `json:"enum"`
					} `json:"type"`
				} `json:"properties"`
			} `json:"EntityField"`
		} `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(contract.AdminAppSchemaJSON, &schema))

	schemaEnum := schema.Defs.EntityField.Properties.Type.Enum
	require.NotEmpty(t, schemaEnum, "EntityField.type enum missing from vendored schema")

	assert.ElementsMatch(t, schemaEnum, domain.AllowedFieldTypes(),
		"content field_type set is out of sync with admin-app.schema.json EntityField.type")
}

// TestFieldRoleContractParity asserts the tenant-role set the CMS decides field
// permission against is exactly the enum the vendored admin-app.schema.json
// declares for EntityField.read_roles and .write_roles.
//
// The set now lives in four places — memberships' CHECK (000012),
// content_type_fields' CHECK (000026), domain.AllowedFieldRoles(), and the
// generator's TenantRole literal — and a fourth copy is only defensible with
// this test, which is the reason the generator's own comment points at it.
//
// The two halves are checked separately rather than "read_roles, and assume
// write_roles matches". They are independent properties in the schema, so a
// hand-edit or a generator change can move one and not the other, and the
// asymmetric result — a role you may be granted read with but never write —
// would look like a permission bug rather than a contract one.
func TestFieldRoleContractParity(t *testing.T) {
	var schema struct {
		Defs struct {
			EntityField struct {
				Properties map[string]struct {
					Items struct {
						Enum []string `json:"enum"`
					} `json:"items"`
				} `json:"properties"`
			} `json:"EntityField"`
		} `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(contract.AdminAppSchemaJSON, &schema))

	for _, prop := range []string{"read_roles", "write_roles"} {
		p, ok := schema.Defs.EntityField.Properties[prop]
		require.True(t, ok, "EntityField.%s missing from the vendored schema — the contract does not describe field permission at all", prop)
		require.NotEmpty(t, p.Items.Enum, "EntityField.%s has no role enum; a bare string array would let a typo'd role through the contract", prop)

		assert.ElementsMatch(t, p.Items.Enum, domain.AllowedFieldRoles(),
			"domain.AllowedFieldRoles() is out of sync with EntityField.%s in admin-app.schema.json", prop)
	}
}

// TestTypeRoleContractParity is the collection-level half: EntitySchema's three
// permission lists must carry the same role enum the CMS decides against.
//
// It is a SEPARATE function from the field one rather than a loop over both
// definitions, because the failure it guards is precisely that the two drift
// apart. A single test asserting "some definition has the roles" would pass with
// EntitySchema missing its lists entirely — which is the state this repo was in
// between 88e0c35 and the type-level work, and is exactly the drift the resync
// exists to prevent. A resync without a parity test is decoration.
//
// All three lists are checked, including own_only_roles, whose polarity is the
// opposite of the other two. A role that can be named as CONFINED but not as
// ALLOWED (or the reverse) would be a contract that cannot express the schema
// the CMS accepts.
func TestTypeRoleContractParity(t *testing.T) {
	var schema struct {
		Defs struct {
			EntitySchema struct {
				Properties map[string]struct {
					Items struct {
						Enum []string `json:"enum"`
					} `json:"items"`
				} `json:"properties"`
			} `json:"EntitySchema"`
		} `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(contract.AdminAppSchemaJSON, &schema))

	for _, prop := range []string{"read_roles", "write_roles", "own_only_roles"} {
		p, ok := schema.Defs.EntitySchema.Properties[prop]
		require.True(t, ok,
			"EntitySchema.%s missing from the vendored schema — the contract describes field permission but not the collection it lives in", prop)
		require.NotEmpty(t, p.Items.Enum,
			"EntitySchema.%s has no role enum; a bare string array would let a typo'd role through the contract", prop)

		assert.ElementsMatch(t, p.Items.Enum, domain.AllowedFieldRoles(),
			"domain.AllowedFieldRoles() is out of sync with EntitySchema.%s in admin-app.schema.json", prop)
	}
}

// TestFieldTypeSQLParity asserts the domain's legal field_type set is exactly the
// content_type_fields_field_type_check constraint — as of 000028, which REPLACED
// the constraint 000024 introduced (adding 'richtext'). This test must always
// read the LATEST migration that touches the constraint; reading a superseded
// one would pin Go against a CHECK the database no longer enforces.
//
// 000024's own header says "the parity test reads this constraint the way
// TestEntryStatusParity reads 000016's" — and until this function existed that
// sentence described nothing. Drift matters in both directions and neither shows
// up as an obvious failure: a constraint NARROWER than Go turns a legal AddField
// into a driver error (a 500 for a request the service already approved), and one
// WIDER than Go lets a row reach validateScalar's fail-closed default arm, where
// every value in the field is refused with no hint why.
//
// The integration test in repository/schema_mutation_integration_test.go inserts
// each AllowedFieldTypes() value against a live database, which covers the
// narrower direction — but only when Docker is up. This one runs everywhere and
// covers both.
func TestFieldTypeSQLParity(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "cannot locate test file")
	path := filepath.Join(filepath.Dir(file), "..", "migrations", "000028_content_field_type_richtext.up.sql")

	sql, err := os.ReadFile(path) //nolint:gosec // fixed test-local path
	require.NoError(t, err, "migration 000028 must exist — it defines the field_type contract")

	// CHECK (field_type IN ('string', 'text', ...))
	re := regexp.MustCompile(`(?is)CHECK\s*\(\s*field_type\s+IN\s*\(([^)]*)\)`)
	m := re.FindSubmatch(sql)
	require.NotNil(t, m, "content_type_fields_field_type_check not found in migration 000028")

	var fromSQL []string
	for _, lit := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(string(m[1]), -1) {
		fromSQL = append(fromSQL, lit[1])
	}
	require.NotEmpty(t, fromSQL, "no field_type literals parsed out of the CHECK constraint")

	assert.ElementsMatch(t, fromSQL, domain.AllowedFieldTypes(),
		"domain.AllowedFieldTypes() is out of sync with content_type_fields_field_type_check")
}

// TestEntryStatusParity asserts the domain's legal status set is exactly the
// entries_status_check CHECK constraint in migration 000016. The migration is
// the source of truth (the DB rejects anything else regardless of what Go
// believes), so this reads the SQL rather than hand-copying the list — same
// reasoning as TestFieldTypeParity above.
func TestEntryStatusParity(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "cannot locate test file")
	path := filepath.Join(filepath.Dir(file), "..", "migrations", "000016_content_entry_status.up.sql")

	sql, err := os.ReadFile(path) //nolint:gosec // fixed test-local path
	require.NoError(t, err, "migration 000016 must exist — it defines the status contract")

	// CHECK (status IN ('draft', 'published'))
	re := regexp.MustCompile(`(?is)CHECK\s*\(\s*status\s+IN\s*\(([^)]*)\)`)
	m := re.FindSubmatch(sql)
	require.NotNil(t, m, "entries_status_check CHECK constraint not found in migration 000016")

	var fromSQL []string
	for _, lit := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(string(m[1]), -1) {
		fromSQL = append(fromSQL, lit[1])
	}
	require.NotEmpty(t, fromSQL, "no status literals parsed out of the CHECK constraint")

	assert.ElementsMatch(t, fromSQL, domain.AllowedStatuses(),
		"domain.AllowedStatuses() is out of sync with the entries_status_check constraint")
}

// TestMediaMetadataLimitsParity asserts the Go constants that produce a 422 are
// the same numbers the CHECK constraints in migration 000022 enforce.
//
// Drift here has a specific and ugly shape rather than being merely untidy: if
// Go's ceiling is the HIGHER of the two, a value passes validation, reaches
// Postgres, violates the constraint, and comes back as a driver error — a 500
// for what is unambiguously a client mistake, with no field named. So the
// migration is read as the source of truth (the DB refuses the write regardless
// of what Go believes) rather than the numbers being hand-copied, exactly as
// TestEntryStatusParity above does for the status set.
func TestMediaMetadataLimitsParity(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "cannot locate test file")
	path := filepath.Join(filepath.Dir(file), "..", "migrations", "000022_content_media_metadata.up.sql")

	sql, err := os.ReadFile(path) //nolint:gosec // fixed test-local path
	require.NoError(t, err, "migration 000022 must exist — it defines the media metadata contract")

	one := func(what, pattern string) int {
		m := regexp.MustCompile(pattern).FindSubmatch(sql)
		require.NotNil(t, m, "%s not found in migration 000022 (pattern %s)", what, pattern)
		n, err := strconv.Atoi(string(m[1]))
		require.NoError(t, err)
		return n
	}

	// char_length(filename) BETWEEN 1 AND 255
	assert.Equal(t, one("filename length limit", `(?is)char_length\(filename\)\s+BETWEEN\s+1\s+AND\s+(\d+)`),
		domain.MaxFilenameLen, "domain.MaxFilenameLen is out of sync with media_assets_filename_check")

	// char_length(alt_text) <= 1000
	assert.Equal(t, one("alt_text length limit", `(?is)char_length\(alt_text\)\s*<=\s*(\d+)`),
		domain.MaxAltTextLen, "domain.MaxAltTextLen is out of sync with media_assets_alt_text_check")

	// width_px BETWEEN 1 AND 65535 — and the height half must carry the same
	// ceiling, or one dimension is validated against a limit the other is not.
	assert.Equal(t, one("width ceiling", `(?is)width_px\s+BETWEEN\s+1\s+AND\s+(\d+)`),
		domain.MaxImageDimension, "domain.MaxImageDimension is out of sync with media_assets_dimensions_check")
	assert.Equal(t, one("height ceiling", `(?is)height_px\s+BETWEEN\s+1\s+AND\s+(\d+)`),
		domain.MaxImageDimension, "the height half of media_assets_dimensions_check disagrees with the width half")
}
