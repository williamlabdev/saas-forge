package service

import (
	"context"
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
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Data-level permission (migration 000027). As with the field-level suite,
// almost everything asserted here is a REFUSAL or an ABSENCE: a permission
// system is judged by what it does not let through, and each of these fails
// OPEN if its gate is deleted.
//
// The confinement half additionally asserts things about `total` and about
// WHICH error is returned, because those are the two ways a row-level rule leaks
// what it is hiding even when it returns the right rows.

// ctxUser is ctxRole with a CALLER IDENTITY that the test controls. Confinement
// is resolved against created_by, so a helper that minted a fresh uuid per call
// — which ctxRole does — would make every caller a different person and every
// confinement test pass for the wrong reason.
func ctxRoleUser(tenant, role string, id uuid.UUID) context.Context {
	return authn.WithSubject(context.Background(), authn.Subject{
		UserID:     id,
		TenantID:   tenant,
		TenantRole: role,
	})
}

// restrictedTypeInput is a collection only owner/admin may read and only owner
// may write, with one unrestricted field — so a refusal in these tests is
// provably the TYPE gate rather than the field gate.
func restrictedTypeInput() CreateTypeInput {
	return CreateTypeInput{
		Name:       "ledger",
		Label:      "Ledger",
		ReadRoles:  []string{"admin", "owner"},
		WriteRoles: []string{"owner"},
		Fields: []FieldInput{
			{Key: "memo", Type: domain.FieldTypeString, Required: true},
		},
	}
}

// --- the collection gate ------------------------------------------------------

// A collection an editor may not read must refuse the LIST, by name. The type's
// existence is not the secret — its contents are — so this is a 403 that says
// which roles may read, not a 404 that sends an operator hunting for a typo.
func TestTypePermission_ListRefusesUnreadableCollectionByName(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, restrictedTypeInput())
	require.NoError(t, err)

	_, err = svc.ListEntries(ctxRole("t1", "editor"), "ledger", ListEntriesInput{})
	assert.Equal(t, "CONTENT_TYPE_READ_FORBIDDEN", codeOf(t, err))
	d := detailsOf(t, err)
	assert.Equal(t, "ledger", d["type"])
	assert.Equal(t, []string{"admin", "owner"}, d["allowed_roles"],
		"the refusal must say who may read, or the caller cannot tell which grant to ask for")
}

// Every single-entry read path goes through the same gate. Listed one by one
// because each is a separate call site, and the one that forgets is the one that
// serves the collection.
func TestTypePermission_EveryReadPathRefusesUnreadableCollection(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, restrictedTypeInput())
	require.NoError(t, err)
	e, err := svc.CreateEntry(owner, "ledger", mustJSON(t, map[string]any{"memo": "rent"}))
	require.NoError(t, err)

	editor := ctxRole("t1", "editor")
	reads := map[string]func() error{
		"list": func() error { _, err := svc.ListEntries(editor, "ledger", ListEntriesInput{}); return err },
		"get":  func() error { _, err := svc.GetEntry(editor, "ledger", e.ID); return err },
		"translations": func() error {
			_, err := svc.ListTranslations(editor, "ledger", e.ID)
			return err
		},
	}
	for name, call := range reads {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, "CONTENT_TYPE_READ_FORBIDDEN", codeOf(t, call()))
		})
	}
}

// And every write path. create / update / publish / delete are four call sites
// that each resolve the type themselves.
func TestTypePermission_EveryWritePathRefusesUnwritableCollection(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, restrictedTypeInput())
	require.NoError(t, err)
	e, err := svc.CreateEntry(owner, "ledger", mustJSON(t, map[string]any{"memo": "rent"}))
	require.NoError(t, err)

	// admin MAY read this collection and may NOT write it, which isolates the
	// write gate: a refusal here cannot be the read gate firing first.
	admin := ctxRole("t1", "admin")
	writes := map[string]func() error{
		"create": func() error {
			_, err := svc.CreateEntry(admin, "ledger", mustJSON(t, map[string]any{"memo": "x"}))
			return err
		},
		"update": func() error {
			_, err := svc.UpdateEntry(admin, "ledger", e.ID, mustJSON(t, map[string]any{"memo": "y"}), 0)
			return err
		},
		"publish": func() error {
			_, err := svc.SetEntryStatus(admin, "ledger", e.ID, domain.StatusPublished, 0)
			return err
		},
		"delete": func() error { return svc.DeleteEntry(admin, "ledger", e.ID) },
	}
	for name, call := range writes {
		t.Run(name, func(t *testing.T) {
			err := call()
			assert.Equal(t, "CONTENT_TYPE_WRITE_FORBIDDEN", codeOf(t, err))
			assert.Equal(t, "ledger", detailsOf(t, err)["type"])
		})
	}

	// The owner is on both lists and keeps working — otherwise this test would
	// pass just as well against a gate that refused everyone.
	_, err = svc.UpdateEntry(owner, "ledger", e.ID, mustJSON(t, map[string]any{"memo": "y"}), 0)
	assert.NoError(t, err)
}

// A collection the caller cannot READ they cannot WRITE — the ruling
// canWriteField carries, one level up. Here write_roles is EMPTY (unrestricted),
// so nothing but the read rule stands between an editor and the collection.
func TestTypePermission_UnreadableCollectionIsAlsoUnwritable(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name:      "ledger",
		ReadRoles: []string{"owner"}, // write UNRESTRICTED — the dangerous pair
		Fields:    []FieldInput{{Key: "memo", Type: domain.FieldTypeString}},
	})
	require.NoError(t, err)

	_, err = svc.CreateEntry(ctxRole("t1", "editor"), "ledger", mustJSON(t, map[string]any{"memo": "x"}))
	require.Equal(t, "CONTENT_TYPE_WRITE_FORBIDDEN", codeOf(t, err))

	// And it reports who may ACTUALLY write — "only the owner", not the literal
	// empty write_roles, which reads as "nobody may write this" and sends the
	// caller to ask for a grant that does not exist.
	assert.Equal(t, []string{"owner"}, detailsOf(t, err)["allowed_roles"])
}

// A delivery credential carries no tenant role, so it matches no non-empty list.
// Asserted separately from "an editor is refused" because the two are the same
// answer today for DIFFERENT reasons, and canReadType writes the delivery one
// out explicitly so it survives a delivery token that ever carries a role.
func TestTypePermission_DeliveryIsRefusedARestrictedCollection(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, restrictedTypeInput())
	require.NoError(t, err)
	e, err := svc.CreateEntry(owner, "ledger", mustJSON(t, map[string]any{"memo": "rent"}))
	require.NoError(t, err)
	_, err = svc.SetEntryStatus(owner, "ledger", e.ID, domain.StatusPublished, 0)
	require.NoError(t, err)

	// PUBLISHED, and still dark to the public: an operator who restricted the
	// collection restricted it, and publish state does not override that.
	_, err = svc.ListEntries(ctxDelivery("t1"), "ledger", ListEntriesInput{})
	assert.Equal(t, "CONTENT_TYPE_READ_FORBIDDEN", codeOf(t, err))
}

// The delivery refusal is EXPLICIT, not a lucky fall-through.
//
// Today a delivery credential carries no tenant role, so "refused because
// PublicDelivery" and "refused because it matches no list" give the same answer
// and neither can be told from the other. They stop agreeing the moment anyone
// mints a delivery token with a role on it — exactly the change nobody would
// connect to this file. So the token here DOES carry a role that is on the read
// list, and it must still be refused.
func TestTypePermission_DeliveryRoleOnTheListIsStillRefused(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, restrictedTypeInput())
	require.NoError(t, err)
	e, err := svc.CreateEntry(owner, "ledger", mustJSON(t, map[string]any{"memo": "rent"}))
	require.NoError(t, err)
	_, err = svc.SetEntryStatus(owner, "ledger", e.ID, domain.StatusPublished, 0)
	require.NoError(t, err)

	// "owner" is on read_roles — and PublicDelivery narrows regardless.
	delivery := authn.WithSubject(context.Background(), authn.Subject{
		TenantID: "t1", TenantRole: "owner", PublicDelivery: true,
	})
	_, err = svc.ListEntries(delivery, "ledger", ListEntriesInput{})
	assert.Equal(t, "CONTENT_TYPE_READ_FORBIDDEN", codeOf(t, err),
		"a role on the token must not buy a delivery credential into a restricted collection")

	_, err = svc.GetEntry(delivery, "ledger", e.ID)
	assert.Equal(t, "CONTENT_TYPE_READ_FORBIDDEN", codeOf(t, err))
}

// The absence of an owner bypass, on its own so that adding one fails a test
// whose name says what it is.
func TestTypePermission_ThereIsNoOwnerBypass(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name:       "ledger",
		WriteRoles: []string{"editor"},
		Fields:     []FieldInput{{Key: "memo", Type: domain.FieldTypeString}},
	})
	require.NoError(t, err)

	_, err = svc.CreateEntry(owner, "ledger", mustJSON(t, map[string]any{"memo": "x"}))
	assert.Equal(t, "CONTENT_TYPE_WRITE_FORBIDDEN", codeOf(t, err),
		"the declaration is the truth; owner is not silently on every list")
}

// Empty means unrestricted, and it is the default — the property that makes
// migration 000027 a no-op for every type that already exists.
func TestTypePermission_EmptyListsRestrictNobody(t *testing.T) {
	svc, _ := newSvc()
	_, err := svc.CreateContentType(ctxRole("t1", "owner"), CreateTypeInput{
		Name:   "note",
		Fields: []FieldInput{{Key: "memo", Type: domain.FieldTypeString}},
	})
	require.NoError(t, err)

	_, err = svc.CreateEntry(ctxRole("t1", "viewer"), "note", mustJSON(t, map[string]any{"memo": "x"}))
	assert.NoError(t, err, "an undeclared collection is open to everyone the verb allows")
}

// --- privilege escalation ------------------------------------------------------

// UpdateContentType runs on content:update, which an EDITOR holds. Without the
// extra schema:write check an editor could PATCH read_roles to [] and hand
// themselves a collection restricted to admins.
func TestTypePermission_EditorCannotRewriteTypePermissions(t *testing.T) {
	svc := NewContentService(&memRepo{}, authz.NewRBACAuthorizer(), staticPlan(Quota{}))
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, restrictedTypeInput())
	require.NoError(t, err)

	editor := ctxRole("t1", "editor")
	for _, name := range []string{"read_roles", "write_roles", "own_only_roles"} {
		t.Run(name, func(t *testing.T) {
			empty := []string{}
			in := UpdateTypeInput{Label: "Ledger"}
			switch name {
			case "read_roles":
				in.ReadRoles = &empty
			case "write_roles":
				in.WriteRoles = &empty
			case "own_only_roles":
				in.OwnOnlyRoles = &empty
			}
			_, err := svc.UpdateContentType(editor, "ledger", in)
			require.ErrorIs(t, err, apperrors.ErrForbidden)
		})
	}

	// The same editor keeps the ordinary label edit, or this has revoked the verb
	// rather than narrowing it.
	_, err = svc.UpdateContentType(editor, "ledger", UpdateTypeInput{Label: "Ledgers"})
	assert.NoError(t, err, "an editor still edits the parts of a type definition that are not a policy")
}

// An omitted list must leave the stored one alone. If UpdateContentType failed
// to carry the stored value forward, a routine label PATCH would silently
// unrestrict the collection — the repository writes all three unconditionally.
func TestTypePermission_LabelPatchDoesNotClearPermissions(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, restrictedTypeInput())
	require.NoError(t, err)

	got, err := svc.UpdateContentType(owner, "ledger", UpdateTypeInput{Label: "Ledgers"})
	require.NoError(t, err)
	assert.Equal(t, []string{"admin", "owner"}, got.ReadRoles)
	assert.Equal(t, []string{"owner"}, got.WriteRoles)

	// And it is not merely the RESPONSE that kept them.
	_, err = svc.ListEntries(ctxRole("t1", "editor"), "ledger", ListEntriesInput{})
	assert.Equal(t, "CONTENT_TYPE_READ_FORBIDDEN", codeOf(t, err))
}

// An explicit [] is a real instruction, distinct from an omitted key. This is
// what the pointer fields buy.
func TestTypePermission_ExplicitEmptyListUnrestricts(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, restrictedTypeInput())
	require.NoError(t, err)

	empty := []string{}
	_, err = svc.UpdateContentType(owner, "ledger", UpdateTypeInput{ReadRoles: &empty, WriteRoles: &empty})
	require.NoError(t, err)

	_, err = svc.ListEntries(ctxRole("t1", "editor"), "ledger", ListEntriesInput{})
	assert.NoError(t, err)
}

func TestTypePermission_UnknownRoleRefusedAtDefinitionTime(t *testing.T) {
	svc, _ := newSvc()
	in := restrictedTypeInput()
	in.OwnOnlyRoles = []string{"editors"} // the plausible typo
	_, err := svc.CreateContentType(ctxRole("t1", "owner"), in)

	assert.Equal(t, "CONTENT_TYPE_ROLE_UNKNOWN", codeOf(t, err))
	d := detailsOf(t, err)
	assert.Equal(t, "own_only_roles", d["list"], "the refusal must name WHICH list, not just the type")
	assert.Equal(t, "editors", d["role"])
}

// --- confinement ---------------------------------------------------------------

var (
	alice = uuid.New()
	bob   = uuid.New()
)

// seedConfined builds a collection where editors see only their own entries, and
// puts one entry in it from each of two editors.
func seedConfined(t *testing.T, svc ContentService) (aliceEntry, bobEntry uuid.UUID) {
	t.Helper()
	owner := ctxRoleUser("t1", "owner", uuid.New())
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name:         "article",
		OwnOnlyRoles: []string{"editor"},
		Fields:       []FieldInput{{Key: "title", Type: domain.FieldTypeString, Required: true}},
	})
	require.NoError(t, err)

	a, err := svc.CreateEntry(ctxRoleUser("t1", "editor", alice), "article", mustJSON(t, map[string]any{"title": "Alice"}))
	require.NoError(t, err)
	b, err := svc.CreateEntry(ctxRoleUser("t1", "editor", bob), "article", mustJSON(t, map[string]any{"title": "Bob"}))
	require.NoError(t, err)
	return a.ID, b.ID
}

// The list shows only the caller's own rows AND `total` agrees with them.
//
// The total is the half worth insisting on: filtering the page in Go would
// return the right rows while leaving total counting the whole collection, so
// the number of hidden entries is recoverable by subtraction — a leak that no
// test looking only at Items would catch.
func TestConfinement_ListShowsOwnEntriesAndTotalAgrees(t *testing.T) {
	svc, _ := newSvc()
	seedConfined(t, svc)

	res, err := svc.ListEntries(ctxRoleUser("t1", "editor", alice), "article", ListEntriesInput{})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.Equal(t, 1, mustTotal(t, res), "total must count what the caller may see, not the collection")

	// An unconfined role still sees everything, or confinement has become a
	// global restriction rather than a per-role one.
	res, err = svc.ListEntries(ctxRoleUser("t1", "admin", uuid.New()), "article", ListEntriesInput{})
	require.NoError(t, err)
	assert.Len(t, res.Items, 2)
	assert.Equal(t, 2, mustTotal(t, res))
}

// A row a confined caller does not own answers 404, NOT 403. The distinction is
// the whole point: "exists but forbidden" lets an editor enumerate a colleague's
// drafts one id at a time, which is what confinement exists to prevent.
func TestConfinement_ForeignEntryIsNotFoundNotForbidden(t *testing.T) {
	svc, _ := newSvc()
	_, bobEntry := seedConfined(t, svc)
	editorAlice := ctxRoleUser("t1", "editor", alice)

	calls := map[string]func() error{
		"get": func() error { _, err := svc.GetEntry(editorAlice, "article", bobEntry); return err },
		"update": func() error {
			_, err := svc.UpdateEntry(editorAlice, "article", bobEntry, mustJSON(t, map[string]any{"title": "x"}), 0)
			return err
		},
		"publish": func() error {
			_, err := svc.SetEntryStatus(editorAlice, "article", bobEntry, domain.StatusPublished, 0)
			return err
		},
		"delete":       func() error { return svc.DeleteEntry(editorAlice, "article", bobEntry) },
		"translations": func() error { _, err := svc.ListTranslations(editorAlice, "article", bobEntry); return err },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, call(), apperrors.ErrNotFound,
				"a refusal that is distinguishable from a nonexistent id is an oracle")
		})
	}
}

// The confinement check must run BEFORE the optimistic-lock check. A 409 on a
// row a confined caller may not touch confirms both that the id exists and what
// version it is on — the refusals have to be ordered so the least informative
// one wins.
func TestConfinement_OutranksTheVersionConflict(t *testing.T) {
	svc, _ := newSvc()
	_, bobEntry := seedConfined(t, svc)
	editorAlice := ctxRoleUser("t1", "editor", alice)

	// A deliberately wrong expected version: with the checks in the other order
	// this answers 409 and leaks that the row is at version 1.
	_, err := svc.UpdateEntry(editorAlice, "article", bobEntry, mustJSON(t, map[string]any{"title": "x"}), 99)
	require.ErrorIs(t, err, apperrors.ErrNotFound)

	_, err = svc.SetEntryStatus(editorAlice, "article", bobEntry, domain.StatusPublished, 99)
	require.ErrorIs(t, err, apperrors.ErrNotFound)
}

// The refused write must also have written nothing — a 404 raised after the
// merge would be a permission check that only affects the response.
func TestConfinement_RefusedWriteStoresNothing(t *testing.T) {
	svc, _ := newSvc()
	_, bobEntry := seedConfined(t, svc)
	admin := ctxRoleUser("t1", "admin", uuid.New())

	before, err := svc.GetEntry(admin, "article", bobEntry)
	require.NoError(t, err)

	_, err = svc.UpdateEntry(ctxRoleUser("t1", "editor", alice), "article", bobEntry,
		mustJSON(t, map[string]any{"title": "stolen"}), 0)
	require.Error(t, err)

	after, err := svc.GetEntry(admin, "article", bobEntry)
	require.NoError(t, err)
	assert.JSONEq(t, string(mustMarshalData(t, before)), string(mustMarshalData(t, after)))
}

// A confined caller keeps full control of their OWN rows. Without this the
// feature is indistinguishable from revoking the verb.
func TestConfinement_OwnEntriesRemainFullyWritable(t *testing.T) {
	svc, _ := newSvc()
	aliceEntry, _ := seedConfined(t, svc)
	editorAlice := ctxRoleUser("t1", "editor", alice)

	_, err := svc.UpdateEntry(editorAlice, "article", aliceEntry, mustJSON(t, map[string]any{"title": "mine"}), 0)
	require.NoError(t, err)
	_, err = svc.SetEntryStatus(editorAlice, "article", aliceEntry, domain.StatusPublished, 0)
	require.NoError(t, err)
	require.NoError(t, svc.DeleteEntry(editorAlice, "article", aliceEntry))
}

// A delivery credential is NEVER confined. It authors nothing, so matching it on
// created_by would hide every published entry from the public the moment an
// editorial setting was changed — a live-site outage with no error anywhere.
func TestConfinement_DeliveryIsNeverConfined(t *testing.T) {
	svc, _ := newSvc()
	aliceEntry, bobEntry := seedConfined(t, svc)
	owner := ctxRoleUser("t1", "owner", uuid.New())
	for _, id := range []uuid.UUID{aliceEntry, bobEntry} {
		_, err := svc.SetEntryStatus(owner, "article", id, domain.StatusPublished, 0)
		require.NoError(t, err)
	}

	res, err := svc.ListEntries(ctxDelivery("t1"), "article", ListEntriesInput{})
	require.NoError(t, err)
	assert.Len(t, res.Items, 2, "the public reads by publish state, not by authorship")
}

// A translation group is a set of ROWS, so confinement applies to the siblings
// too. Without it this endpoint reads exactly the rows GetEntry refuses — the
// shape of bypass it has already had to be closed against once.
func TestConfinement_TranslationSiblingsAreAlsoConfined(t *testing.T) {
	svc, _ := newSvc()
	aliceEntry, _ := seedConfined(t, svc)

	// Bob translates Alice's entry — allowed only because admin does it here;
	// a confined editor could not name her id at all (asserted below).
	admin := ctxRoleUser("t1", "admin", uuid.New())
	src := aliceEntry
	_, err := svc.CreateLocalizedEntry(admin, "article", CreateLocalizedInput{
		Payload: mustJSON(t, map[string]any{"title": "Alice (fr)"}),
		Locale:  "fr", TranslationOf: &src,
	})
	require.NoError(t, err)

	// Alice sees her own row in the group; the sibling admin authored is not hers.
	got, err := svc.ListTranslations(ctxRoleUser("t1", "editor", alice), "article", aliceEntry)
	require.NoError(t, err)
	assert.Len(t, got, 1, "a row a confined caller does not own is not readable through the group view")

	// And admin, unconfined, sees both.
	got, err = svc.ListTranslations(admin, "article", aliceEntry)
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

// Naming a colleague's id as translation_of both confirms it exists and joins
// their group — a read of a refused row dressed up as a write of an allowed one.
func TestConfinement_CannotTranslateAForeignEntry(t *testing.T) {
	svc, _ := newSvc()
	_, bobEntry := seedConfined(t, svc)

	src := bobEntry
	_, err := svc.CreateLocalizedEntry(ctxRoleUser("t1", "editor", alice), "article", CreateLocalizedInput{
		Payload: mustJSON(t, map[string]any{"title": "x"}),
		Locale:  "fr", TranslationOf: &src,
	})
	require.ErrorIs(t, err, apperrors.ErrNotFound)
}

// Turning confinement ON over entries with no recorded author is refused, with
// the count. Those rows match no author, so the alternative is that they simply
// vanish — the only refusal in this stack that is indistinguishable from data
// loss, which is why it is the only one checked against stored data.
func TestConfinement_EnablingRefusedWhenEntriesLackAnAuthor(t *testing.T) {
	svc, repo := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name:   "article",
		Fields: []FieldInput{{Key: "title", Type: domain.FieldTypeString}},
	})
	require.NoError(t, err)
	e, err := svc.CreateEntry(owner, "article", mustJSON(t, map[string]any{"title": "old"}))
	require.NoError(t, err)
	// A row predating migration 000021: present, and attributed to nobody.
	for _, stored := range repo.entries {
		if stored.ID == e.ID {
			stored.CreatedBy = nil
		}
	}

	confine := []string{"editor"}
	_, err = svc.UpdateContentType(owner, "article", UpdateTypeInput{OwnOnlyRoles: &confine})
	require.Equal(t, "CONTENT_ENTRY_AUTHOR_MISSING", codeOf(t, err))
	d := detailsOf(t, err)
	assert.EqualValues(t, 1, d["entries"], "the operator needs the count to know what they are about to hide")
	assert.Equal(t, []string{"editor"}, d["roles"])
}

// Relaxing confinement is never blocked by data — only tightening can hide
// something new.
func TestConfinement_RelaxingIsNeverBlocked(t *testing.T) {
	svc, repo := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name:         "article",
		OwnOnlyRoles: []string{"editor"},
		Fields:       []FieldInput{{Key: "title", Type: domain.FieldTypeString}},
	})
	require.NoError(t, err)
	e, err := svc.CreateEntry(owner, "article", mustJSON(t, map[string]any{"title": "old"}))
	require.NoError(t, err)
	for _, stored := range repo.entries {
		if stored.ID == e.ID {
			stored.CreatedBy = nil
		}
	}

	none := []string{}
	_, err = svc.UpdateContentType(owner, "article", UpdateTypeInput{OwnOnlyRoles: &none})
	assert.NoError(t, err, "un-confining hides nothing, so no count of rows can block it")
}

// --- what the schema tells the caller -------------------------------------------

// The type-level `writable` folds in the VERB, which is the gap FieldDTO's
// deliberately leaves: a viewer holds no content:update, so before this every
// field of every type came back writable and the admin app rendered an editable
// form for a caller who could not save it.
func TestContentTypeDTO_WritableFoldsInTheVerb(t *testing.T) {
	svc := NewContentService(&memRepo{}, authz.NewRBACAuthorizer(), staticPlan(Quota{}))
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name:   "article",
		Fields: []FieldInput{{Key: "title", Type: domain.FieldTypeString}},
	})
	require.NoError(t, err)

	viewer, err := svc.GetContentType(ctxRole("t1", "viewer"), "article")
	require.NoError(t, err)
	assert.True(t, viewer.Readable)
	assert.False(t, viewer.Writable, "a viewer cannot write entries of ANY type — the verb says so")

	editor, err := svc.GetContentType(ctxRole("t1", "editor"), "article")
	require.NoError(t, err)
	assert.True(t, editor.Writable)
}

// The per-caller answers must also reflect the TYPE lists, not only the verb.
func TestContentTypeDTO_ReportsTypeLevelAnswers(t *testing.T) {
	svc := NewContentService(&memRepo{}, authz.NewRBACAuthorizer(), staticPlan(Quota{}))
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, restrictedTypeInput())
	require.NoError(t, err)

	admin, err := svc.GetContentType(ctxRole("t1", "admin"), "ledger")
	require.NoError(t, err)
	assert.True(t, admin.Readable)
	assert.False(t, admin.Writable, "admin reads this collection and does not write it")
	assert.False(t, admin.OwnOnly)

	// The DECLARATION is public to anyone who may read the schema — an operator
	// whose collection came back empty has to be able to find out why.
	assert.Equal(t, []string{"admin", "owner"}, admin.ReadRoles)
}

// own_only is published so a confined caller's short list reads as "yours"
// rather than as an outage.
func TestContentTypeDTO_ReportsConfinement(t *testing.T) {
	svc, _ := newSvc()
	seedConfined(t, svc)

	got, err := svc.GetContentType(ctxRoleUser("t1", "editor", alice), "article")
	require.NoError(t, err)
	assert.True(t, got.OwnOnly)

	got, err = svc.GetContentType(ctxRoleUser("t1", "admin", uuid.New()), "article")
	require.NoError(t, err)
	assert.False(t, got.OwnOnly)
}

// --- relation existence ---------------------------------------------------------

// A relation field pointed at a collection the caller may not read is an
// existence oracle around the type gate: EntryExists answers "is there a row
// with this id", which is the fact guardTypeRead refuses to serve.
func TestTypePermission_RelationCannotProbeAnUnreadableCollection(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, restrictedTypeInput())
	require.NoError(t, err)
	secret, err := svc.CreateEntry(owner, "ledger", mustJSON(t, map[string]any{"memo": "rent"}))
	require.NoError(t, err)

	_, err = svc.CreateContentType(owner, CreateTypeInput{
		Name: "note",
		Fields: []FieldInput{
			{Key: "ref", Type: domain.FieldTypeRelation, RelationEntity: "ledger"},
		},
	})
	require.NoError(t, err)

	_, err = svc.CreateEntry(ctxRole("t1", "editor"), "note",
		mustJSON(t, map[string]any{"ref": secret.ID.String()}))
	assert.Equal(t, "CONTENT_TYPE_READ_FORBIDDEN", codeOf(t, err))
}

// --- parity ----------------------------------------------------------------------

// The legal role set now has FIVE homes: memberships' CHECK (000012), the field
// CHECK (000026), this migration's CHECK, Go, and the generator's Literal. This
// reads the SQL rather than hand-copying it — a constraint NARROWER than Go
// turns an accepted request into a driver error (a 500 for something the service
// just validated), and one WIDER lets a role reach the table that no policy
// decision can be made from.
func TestTypeRoleSQLParity(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "cannot locate test file")
	path := filepath.Join(filepath.Dir(file), "..", "migrations", "000027_content_type_permissions.up.sql")

	sql, err := os.ReadFile(path) //nolint:gosec // fixed test-local path
	require.NoError(t, err, "migration 000027 must exist — it defines the data permission contract")

	re := regexp.MustCompile(`(?is)(read_roles|write_roles|own_only_roles)\s*<@\s*ARRAY\s*\[([^\]]*)\]`)
	found := re.FindAllStringSubmatch(string(sql), -1)
	require.Len(t, found, 3, "all three lists must be constrained by content_types_roles_check")

	for _, part := range found {
		var fromSQL []string
		for _, lit := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(part[2], -1) {
			fromSQL = append(fromSQL, lit[1])
		}
		require.NotEmpty(t, fromSQL, "no role literals parsed out of the %s list", part[1])
		assert.ElementsMatch(t, fromSQL, domain.AllowedFieldRoles(),
			"domain.AllowedFieldRoles() is out of sync with the %s list of content_types_roles_check", part[1])
	}
}
