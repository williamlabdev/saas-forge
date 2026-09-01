package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
)

// Field-level permission. Everything asserted here is a REFUSAL or an ABSENCE,
// which is the only useful shape for this feature: a permission system is
// judged by what it does not let through, and every one of these tests fails
// open if the corresponding gate is deleted.

// salaryTypeInput is a type with one restricted field and one open one, so
// every test can assert the restriction WITHOUT also proving the caller was
// simply refused everything.
func salaryTypeInput() CreateTypeInput {
	return CreateTypeInput{
		Name:  "employee",
		Label: "Employee",
		Fields: []FieldInput{
			{Key: "name", Type: domain.FieldTypeString, Required: true},
			{
				Key: "salary", Type: domain.FieldTypeNumber,
				ReadRoles:  []string{"owner", "admin"},
				WriteRoles: []string{"owner"},
			},
		},
	}
}

// seedEmployee creates the type as owner (who may write every field) and one
// entry carrying both values, then returns the entry id.
func seedEmployee(t *testing.T, svc ContentService, owner context.Context) uuid.UUID {
	t.Helper()
	_, err := svc.CreateContentType(owner, salaryTypeInput())
	require.NoError(t, err)
	e, err := svc.CreateEntry(owner, "employee", mustJSON(t, map[string]any{
		"name": "Ada", "salary": 100000,
	}))
	require.NoError(t, err)
	return e.ID
}

// --- write gate ---------------------------------------------------------------

// The refusal has to name the key the CALLER sent. Silently dropping it is the
// alternative this test exists to keep out: the caller would be told the write
// succeeded while their value went nowhere.
func TestFieldPermission_UpdateRefusesUnwritableKeyByName(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, salaryTypeInput())
	require.NoError(t, err)
	e, err := svc.CreateEntry(owner, "employee", mustJSON(t, map[string]any{"name": "Ada", "salary": 100000}))
	require.NoError(t, err)

	editor := ctxRole("t1", "editor")
	_, err = svc.UpdateEntry(editor, "employee", e.ID, mustJSON(t, map[string]any{"salary": 1}), 0)

	assert.Equal(t, "CONTENT_FIELD_WRITE_FORBIDDEN", codeOf(t, err))
	d := detailsOf(t, err)
	assert.Equal(t, "salary", d["field"], "the refusal must name the key the caller typed")
	assert.Equal(t, []string{"owner"}, d["allowed_roles"], "and say who may write it, or the caller cannot tell a field rule from a role rule")
}

// The stored value must be untouched. A 403 that had already merged is a
// permission check that only affects the response.
func TestFieldPermission_RefusedWriteStoresNothing(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	id := seedEmployee(t, svc, owner)

	e, err := svc.GetEntry(owner, "employee", id)
	require.NoError(t, err)
	before := string(e.Data)

	editor := ctxRole("t1", "editor")
	_, err = svc.UpdateEntry(editor, "employee", id, mustJSON(t, map[string]any{"salary": 1}), 0)
	require.Error(t, err)

	after, err := svc.GetEntry(owner, "employee", id)
	require.NoError(t, err)
	assert.JSONEq(t, before, string(after.Data), "a refused write must leave the document exactly as it was")
}

// A caller who may not write the restricted field can still write the ones they
// own. Without this the feature is indistinguishable from revoking the verb.
func TestFieldPermission_UnrestrictedKeysStillWritable(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	id := seedEmployee(t, svc, owner)

	editor := ctxRole("t1", "editor")
	got, err := svc.UpdateEntry(editor, "employee", id, mustJSON(t, map[string]any{"name": "Grace"}), 0)
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(mustMarshalData(t, got), &doc))
	assert.Equal(t, "Grace", doc["name"])
}

// The merge preserves a value the writer may not send — so a PATCH that touches
// only open fields does not wipe the restricted one. This is why the required
// check is create-only.
func TestFieldPermission_MergePreservesUnwritableValue(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	id := seedEmployee(t, svc, owner)

	editor := ctxRole("t1", "editor")
	_, err := svc.UpdateEntry(editor, "employee", id, mustJSON(t, map[string]any{"name": "Grace"}), 0)
	require.NoError(t, err)

	// Read back as owner, who may see it.
	got, err := svc.GetEntry(owner, "employee", id)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(mustMarshalData(t, got), &doc))
	assert.EqualValues(t, 100000, doc["salary"], "an editor's PATCH must not drop a field they cannot write")
}

// An UNDEFINED key is not a permission problem, and answering 403 for it would
// make the gate an oracle for which fields exist. It must still be the schema's
// 422.
func TestFieldPermission_UnknownKeyIsStillUnknownNotForbidden(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	id := seedEmployee(t, svc, owner)

	editor := ctxRole("t1", "editor")
	_, err := svc.UpdateEntry(editor, "employee", id, mustJSON(t, map[string]any{"nope": 1}), 0)
	assert.Equal(t, "CONTENT_FIELD_UNKNOWN", codeOf(t, err))
}

// --- create: the required dead end ---------------------------------------------

// A role that cannot write a REQUIRED field cannot create the entry at all. It
// must hear that, and not CONTENT_FIELD_REQUIRED naming a key it is not allowed
// to type.
func TestFieldPermission_CreateRefusesRequiredUnwritableField(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name: "employee",
		Fields: []FieldInput{
			{Key: "name", Type: domain.FieldTypeString},
			{Key: "salary", Type: domain.FieldTypeNumber, Required: true, WriteRoles: []string{"owner"}},
		},
	})
	require.NoError(t, err)

	editor := ctxRole("t1", "editor")
	_, err = svc.CreateEntry(editor, "employee", mustJSON(t, map[string]any{"name": "Ada"}))

	assert.Equal(t, "CONTENT_FIELD_REQUIRED_NOT_WRITABLE", codeOf(t, err))
	assert.Equal(t, "salary", detailsOf(t, err)["field"])
}

func TestFieldPermission_CreateRefusesUnwritableKey(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, salaryTypeInput())
	require.NoError(t, err)

	editor := ctxRole("t1", "editor")
	_, err = svc.CreateEntry(editor, "employee", mustJSON(t, map[string]any{"name": "Ada", "salary": 1}))
	assert.Equal(t, "CONTENT_FIELD_WRITE_FORBIDDEN", codeOf(t, err))
}

// --- read projection ------------------------------------------------------------

func TestFieldPermission_ReadStripsUnreadableField(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	id := seedEmployee(t, svc, owner)

	for _, tc := range []struct {
		role    string
		visible bool
	}{
		{"owner", true},
		{"admin", true},
		{"editor", false},
		{"viewer", false},
	} {
		t.Run(tc.role, func(t *testing.T) {
			got, err := svc.GetEntry(ctxRole("t1", tc.role), "employee", id)
			require.NoError(t, err)
			var doc map[string]any
			require.NoError(t, json.Unmarshal(mustMarshalData(t, got), &doc))

			assert.Contains(t, doc, "name", "the open field is visible to everyone with the read verb")
			if tc.visible {
				assert.Contains(t, doc, "salary")
			} else {
				assert.NotContains(t, doc, "salary", "%s is not in read_roles and must not see the value", tc.role)
			}
		})
	}
}

// The list path projects through the same constructor, and a leak there is
// worse than one on the detail path: it hands over every row at once.
func TestFieldPermission_ListStripsUnreadableField(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedEmployee(t, svc, owner)

	res, err := svc.ListEntries(ctxRole("t1", "editor"), "employee", ListEntriesInput{})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(mustMarshalData(t, res.Items[0]), &doc))
	assert.NotContains(t, doc, "salary")
}

// The delivery edge carries NO tenant role, so it matches no non-empty list.
// This is the fail-closed half of the design and the one with the worst
// consequence if it inverts: the field goes to the open internet.
func TestFieldPermission_DeliveryNeverSeesARestrictedField(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	id := seedEmployee(t, svc, owner)
	_, err := svc.SetEntryStatus(owner, "employee", id, domain.StatusPublished, 0)
	require.NoError(t, err)

	delivery := authn.WithSubject(context.Background(), authn.Subject{
		TenantID: "t1", PublicDelivery: true,
	})
	got, err := svc.GetEntry(delivery, "employee", id)
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(mustMarshalData(t, got), &doc))
	assert.Contains(t, doc, "name")
	assert.NotContains(t, doc, "salary", "a restricted field must never reach the public delivery edge")
}

// And it stays refused even if someone mints a delivery credential carrying a
// role that IS on the list. PublicDelivery only ever narrows — the same rule
// authorize() applies to write verbs.
func TestFieldPermission_DeliveryRoleOnTheListIsStillRefused(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	id := seedEmployee(t, svc, owner)
	_, err := svc.SetEntryStatus(owner, "employee", id, domain.StatusPublished, 0)
	require.NoError(t, err)

	delivery := authn.WithSubject(context.Background(), authn.Subject{
		TenantID: "t1", TenantRole: "owner", PublicDelivery: true,
	})
	got, err := svc.GetEntry(delivery, "employee", id)
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(mustMarshalData(t, got), &doc))
	assert.NotContains(t, doc, "salary",
		"PublicDelivery narrows; a role on the token must not buy back a restricted field")
}

// A type that declares no permission must come out byte-for-byte as stored. If
// the projector re-encoded unconditionally, every payload in the system would be
// rewritten by Go's marshaller on every read.
func TestFieldPermission_UnrestrictedPayloadBytesAreUntouched(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, orderTypeInput())
	require.NoError(t, err)
	e, err := svc.CreateEntry(owner, "order", json.RawMessage(`{"title":"a & b","amount":3}`))
	require.NoError(t, err)

	got, err := svc.GetEntry(owner, "order", e.ID)
	require.NoError(t, err)
	assert.Equal(t, string(e.Data), string(got.Data),
		"nothing is hidden here, so the stored bytes must pass through untouched")
}

// --- query surface --------------------------------------------------------------

// Stripping the response while leaving filter open is not a partial defence: a
// range filter recovers a hidden number one request per bit.
func TestFieldPermission_FilterOnUnreadableFieldIsRefused(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedEmployee(t, svc, owner)

	_, err := svc.ListEntries(ctxRole("t1", "editor"), "employee", ListEntriesInput{
		Filters: []string{"salary:gte:50000"},
	})
	assert.Equal(t, "CONTENT_FIELD_QUERY_FORBIDDEN", codeOf(t, err))
	assert.Equal(t, "filter", detailsOf(t, err)["clause"])
}

func TestFieldPermission_SortOnUnreadableFieldIsRefused(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedEmployee(t, svc, owner)

	_, err := svc.ListEntries(ctxRole("t1", "editor"), "employee", ListEntriesInput{Sort: "salary:desc"})
	assert.Equal(t, "CONTENT_FIELD_QUERY_FORBIDDEN", codeOf(t, err))
	assert.Equal(t, "sort", detailsOf(t, err)["clause"])
}

// A reader who MAY see the field keeps the whole query surface. Otherwise the
// gate has simply broken filtering for everyone.
func TestFieldPermission_ReadableFieldStaysQueryable(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedEmployee(t, svc, owner)

	_, err := svc.ListEntries(owner, "employee", ListEntriesInput{Sort: "salary:desc"})
	assert.NoError(t, err)
}

// --- definition-time validation ---------------------------------------------------

// A typo'd role is a list nobody matches, which silently makes the field
// unwritable by everyone. The only cheap moment to catch it is here.
func TestFieldPermission_UnknownRoleRefusedAtDefinition(t *testing.T) {
	svc, _ := newSvc()
	_, err := svc.CreateContentType(ctxRole("t1", "owner"), CreateTypeInput{
		Name:   "employee",
		Fields: []FieldInput{{Key: "salary", Type: domain.FieldTypeNumber, WriteRoles: []string{"editors"}}},
	})
	assert.Equal(t, "CONTENT_FIELD_ROLE_UNKNOWN", codeOf(t, err))
	assert.Equal(t, "editors", detailsOf(t, err)["role"])
}

func TestFieldPermission_DuplicateRoleRefused(t *testing.T) {
	svc, _ := newSvc()
	_, err := svc.CreateContentType(ctxRole("t1", "owner"), CreateTypeInput{
		Name:   "employee",
		Fields: []FieldInput{{Key: "salary", Type: domain.FieldTypeNumber, ReadRoles: []string{"admin", "admin"}}},
	})
	assert.Equal(t, "CONTENT_FIELD_ROLE_DUPLICATE", codeOf(t, err))
}

// A permission list is a SET, so two orderings must produce one stored form —
// otherwise two artifacts that grant identically would not compare equal, and
// comparing equal is the whole point of the format.
func TestFieldPermission_RoleListIsCanonicalised(t *testing.T) {
	svc, _ := newSvc()
	got, err := svc.CreateContentType(ctxRole("t1", "owner"), CreateTypeInput{
		Name: "employee",
		Fields: []FieldInput{
			{Key: "salary", Type: domain.FieldTypeNumber, ReadRoles: []string{"owner", "admin"}},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"admin", "owner"}, got.Fields[0].ReadRoles)
}

// An empty list and an omitted one are the same statement, and both mean
// unrestricted. If they diverged, "" would become a third state nobody defined.
func TestFieldPermission_EmptyListMeansUnrestricted(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name: "employee",
		Fields: []FieldInput{
			{Key: "salary", Type: domain.FieldTypeNumber, ReadRoles: []string{}, WriteRoles: []string{}},
		},
	})
	require.NoError(t, err)

	e, err := svc.CreateEntry(ctxRole("t1", "viewer"), "employee", mustJSON(t, map[string]any{"salary": 1}))
	require.NoError(t, err, "an empty list restricts nobody")
	var doc map[string]any
	require.NoError(t, json.Unmarshal(mustMarshalData(t, e), &doc))
	assert.Contains(t, doc, "salary")
}

// --- privilege escalation ----------------------------------------------------------

// UpdateField runs on content:update, which an EDITOR holds. Without the extra
// check, an editor could PATCH read_roles to [] and hand themselves a field
// restricted to admins — the endpoint would be an escalation, not a schema edit.
func TestFieldPermission_EditorCannotRewritePermissions(t *testing.T) {
	svc := NewContentService(&memRepo{}, authz.NewRBACAuthorizer(), staticPlan(Quota{}))
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, salaryTypeInput())
	require.NoError(t, err)

	empty := []string{}
	_, err = svc.UpdateField(ctxRole("t1", "editor"), "employee", "salary", UpdateFieldInput{ReadRoles: &empty})
	require.Error(t, err, "an editor must not be able to unlock a field")

	// The same editor keeps the ordinary field edits, or this has just revoked
	// the verb rather than narrowing it.
	label := "Compensation"
	_, err = svc.UpdateField(ctxRole("t1", "editor"), "employee", "salary", UpdateFieldInput{Label: &label})
	assert.NoError(t, err, "an editor still edits the parts of a field definition that are not a policy")
}

func TestFieldPermission_OwnerCanRewritePermissions(t *testing.T) {
	svc := NewContentService(&memRepo{}, authz.NewRBACAuthorizer(), staticPlan(Quota{}))
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, salaryTypeInput())
	require.NoError(t, err)

	grant := []string{"editor"}
	got, err := svc.UpdateField(owner, "employee", "salary", UpdateFieldInput{ReadRoles: &grant})
	require.NoError(t, err)
	require.Equal(t, "salary", got.Fields[1].Key)
	assert.Equal(t, []string{"editor"}, got.Fields[1].ReadRoles)
}

// A revoke has to take effect on the next read, not on the next login.
func TestFieldPermission_RevokeAppliesImmediately(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	id := seedEmployee(t, svc, owner)

	visible := func(ctx context.Context) bool {
		got, err := svc.GetEntry(ctx, "employee", id)
		require.NoError(t, err)
		var doc map[string]any
		require.NoError(t, json.Unmarshal(mustMarshalData(t, got), &doc))
		_, ok := doc["salary"]
		return ok
	}
	require.True(t, visible(owner))

	only := []string{"admin"}
	_, err := svc.UpdateField(owner, "employee", "salary", UpdateFieldInput{ReadRoles: &only})
	require.NoError(t, err)

	assert.False(t, visible(owner), "the owner just removed themselves from the list — no bypass")
}

// The absence of an owner bypass, asserted on its own so that adding one later
// fails a test whose name says what it is.
func TestFieldPermission_ThereIsNoOwnerBypass(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name: "employee",
		Fields: []FieldInput{
			{Key: "salary", Type: domain.FieldTypeNumber, WriteRoles: []string{"editor"}},
		},
	})
	require.NoError(t, err)

	_, err = svc.CreateEntry(owner, "employee", mustJSON(t, map[string]any{"salary": 1}))
	assert.Equal(t, "CONTENT_FIELD_WRITE_FORBIDDEN", codeOf(t, err),
		"the declaration is the truth; owner is not silently on every list")
}

// --- parity ------------------------------------------------------------------------

// The legal role set has three homes: memberships' CHECK (000012), this
// migration's CHECK, and Go. This reads the SQL rather than hand-copying it, the
// same way TestEntryStatusParity does — a constraint NARROWER than Go turns an
// approved AddField into a driver error (a 500 for a request the service just
// accepted), and one WIDER lets a role reach the table that no policy decision
// can be made from.
func TestFieldRoleSQLParity(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "cannot locate test file")
	path := filepath.Join(filepath.Dir(file), "..", "migrations", "000026_content_field_permissions.up.sql")

	sql, err := os.ReadFile(path) //nolint:gosec // fixed test-local path
	require.NoError(t, err, "migration 000026 must exist — it defines the field permission contract")

	// read_roles <@ ARRAY['owner', 'admin', ...]  — and the write_roles half.
	re := regexp.MustCompile(`(?is)(read_roles|write_roles)\s*<@\s*ARRAY\s*\[([^\]]*)\]`)
	found := re.FindAllStringSubmatch(string(sql), -1)
	require.Len(t, found, 2, "both halves of content_type_fields_roles_check must be present")

	for _, half := range found {
		var fromSQL []string
		for _, lit := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(half[2], -1) {
			fromSQL = append(fromSQL, lit[1])
		}
		require.NotEmpty(t, fromSQL, "no role literals parsed out of the %s half", half[1])
		assert.ElementsMatch(t, fromSQL, domain.AllowedFieldRoles(),
			"domain.AllowedFieldRoles() is out of sync with the %s half of content_type_fields_roles_check", half[1])
	}
}

// --- helpers -------------------------------------------------------------------------

// mustMarshalData renders the DTO the way the API does and hands back its `data`
// member. Reading dto.Data directly would bypass MarshalJSON — which is where
// the read gate lives, so a test that skipped it would assert nothing.
func mustMarshalData(t *testing.T, dto EntryDTO) []byte {
	t.Helper()
	b, err := json.Marshal(dto)
	require.NoError(t, err)
	var wire struct {
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(b, &wire))
	return wire.Data
}

// --- unreadable implies unwritable -----------------------------------------------

// The silent data-loss path, closed at the server. A field the caller cannot
// READ they cannot WRITE, because a client that renders a form from the schema
// and posts it back sends a value for the hole the read gate left — and PATCH
// semantics make that an overwrite.
//
// This is the case a read-only restriction USED to allow: write_roles is empty
// (unrestricted), so nothing but the read rule stands between an editor and the
// stored value.
func TestFieldPermission_UnreadableFieldIsAlsoUnwritable(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name: "payroll",
		Fields: []FieldInput{
			{Key: "name", Type: domain.FieldTypeString, Required: true},
			// READ-restricted, write UNRESTRICTED — the dangerous combination.
			{Key: "salary", Type: domain.FieldTypeNumber, ReadRoles: []string{"owner"}},
		},
	})
	require.NoError(t, err)
	e, err := svc.CreateEntry(owner, "payroll", mustJSON(t, map[string]any{"name": "Ada", "salary": 100000}))
	require.NoError(t, err)

	// Exactly what the admin app sends: every schema field, with the unreadable
	// one carrying the empty control's value (Number("") === 0).
	editor := ctxRole("t1", "editor")
	_, err = svc.UpdateEntry(editor, "payroll", e.ID, mustJSON(t, map[string]any{
		"name": "Grace", "salary": 0,
	}), 0)
	assert.Equal(t, "CONTENT_FIELD_WRITE_FORBIDDEN", codeOf(t, err))

	// And the stored value is intact — the point of the whole exercise.
	got, err := svc.GetEntry(owner, "payroll", e.ID)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(mustMarshalData(t, got), &doc))
	assert.EqualValues(t, 100000, doc["salary"], "an unreadable field must not be overwritable by a client echoing a blank")
}

// The refusal has to report who may ACTUALLY write. Reporting write_roles alone
// would answer `[]` here — "nobody may write this" — when the truth is "only the
// owner may", and a caller acting on that asks for the wrong grant.
func TestFieldPermission_RefusalReportsEffectiveWriters(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name: "payroll",
		Fields: []FieldInput{
			{Key: "salary", Type: domain.FieldTypeNumber, ReadRoles: []string{"owner"}},
		},
	})
	require.NoError(t, err)

	_, err = svc.CreateEntry(ctxRole("t1", "editor"), "payroll", mustJSON(t, map[string]any{"salary": 0}))
	assert.Equal(t, []string{"owner"}, detailsOf(t, err)["allowed_roles"],
		"write_roles is empty here; the effective constraint is the READ list")
}

func TestFieldPermission_EffectiveWriteRolesIsTheIntersection(t *testing.T) {
	for _, tc := range []struct {
		name              string
		read, write, want []string
	}{
		{"no restriction at all", nil, nil, nil},
		{"write only", nil, []string{"owner"}, []string{"owner"}},
		{"read only — reading gates writing", []string{"owner"}, nil, []string{"owner"}},
		{"both — intersection", []string{"admin", "owner"}, []string{"owner"}, []string{"owner"}},
		// Disjoint: nobody satisfies both, so nobody may write. Reported as an
		// empty list rather than refused at definition time — "frozen field" is a
		// coherent thing to have meant.
		{"disjoint — nobody", []string{"owner"}, []string{"editor"}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveWriteRoles(domain.Field{ReadRoles: tc.read, WriteRoles: tc.write})
			assert.Equal(t, tc.want, got)
		})
	}
}

// --- per-caller access on the schema response ------------------------------------

// GET /types tells the CALLER what they may do, because the caller cannot work
// it out. The admin app never learns its own tenant role, and a client that
// re-derived the rule would be a second copy of a decision that already changed
// once (reading became a precondition for writing).
func TestFieldPermission_SchemaReportsPerCallerAccess(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, salaryTypeInput())
	require.NoError(t, err)

	access := func(ctx context.Context, key string) (bool, bool) {
		t.Helper()
		got, err := svc.GetContentType(ctx, "employee")
		require.NoError(t, err)
		for _, f := range got.Fields {
			if f.Key == key {
				return f.Readable, f.Writable
			}
		}
		t.Fatalf("field %q missing from the schema response", key)
		return false, false
	}

	// salary: read owner+admin, write owner.
	r, w := access(owner, "salary")
	assert.True(t, r)
	assert.True(t, w)

	r, w = access(ctxRole("t1", "admin"), "salary")
	assert.True(t, r, "admin is on the read list")
	assert.False(t, w, "admin is not on the write list")

	r, w = access(ctxRole("t1", "editor"), "salary")
	assert.False(t, r)
	assert.False(t, w)

	// The open field stays open for everyone, or the flags are just reporting
	// "this caller is restricted" rather than anything per-field.
	r, w = access(ctxRole("t1", "editor"), "name")
	assert.True(t, r)
	assert.True(t, w)
}

// The flags must follow the unreadable-implies-unwritable rule too, or a client
// obeying them would still send a field the server refuses — which is the exact
// failed-save this whole mechanism exists to prevent.
func TestFieldPermission_SchemaAccessObeysTheReadPrecondition(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name: "payroll",
		Fields: []FieldInput{
			// READ-restricted, write unrestricted.
			{Key: "salary", Type: domain.FieldTypeNumber, ReadRoles: []string{"owner"}},
		},
	})
	require.NoError(t, err)

	got, err := svc.GetContentType(ctxRole("t1", "editor"), "payroll")
	require.NoError(t, err)
	assert.False(t, got.Fields[0].Readable)
	assert.False(t, got.Fields[0].Writable,
		"write_roles is empty, but an unreadable field is unwritable — the flag must say so")
}
