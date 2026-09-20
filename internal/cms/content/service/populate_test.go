package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
)

// --- fixture -----------------------------------------------------------------

// populateFixture builds the two-type shape every test below needs: an `author`
// collection, and a `post` whose relations point at it BOTH ways round — one
// scalar (`author`), one multi-valued (`authors`) — because the cardinality
// contract is the thing most likely to regress and it cannot be observed from a
// type that only has one of the two.
func populateFixture(t *testing.T, authorFields []FieldInput, authorReadRoles []string) (ContentService, *memRepo, context.Context) {
	t.Helper()
	svc, repo := newSvc()
	admin := ctxTenant("tenant-a")
	if authorFields == nil {
		authorFields = []FieldInput{{Key: "name", Type: domain.FieldTypeString}}
	}
	_, err := svc.CreateContentType(admin, CreateTypeInput{
		Name: "author", Label: "Author",
		Fields:    authorFields,
		ReadRoles: authorReadRoles,
	})
	require.NoError(t, err)
	_, err = svc.CreateContentType(admin, CreateTypeInput{
		Name: "post", Label: "Post",
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "author", Type: domain.FieldTypeRelation, RelationEntity: "author"},
			{Key: "authors", Type: domain.FieldTypeRelation, RelationEntity: "author", Multiple: true},
		},
	})
	require.NoError(t, err)
	return svc, repo, admin
}

func mkAuthor(t *testing.T, svc ContentService, ctx context.Context, name string, publish bool) EntryDTO {
	t.Helper()
	e, err := svc.CreateEntry(ctx, "author", mustJSON(t, map[string]any{"name": name}))
	require.NoError(t, err)
	if publish {
		e = publishEntry(t, svc, ctx, "author", e.ID)
	}
	return e
}

func mkPost(t *testing.T, svc ContentService, ctx context.Context, payload map[string]any, publish bool) EntryDTO {
	t.Helper()
	e, err := svc.CreateEntry(ctx, "post", mustJSON(t, payload))
	require.NoError(t, err)
	if publish {
		e = publishEntry(t, svc, ctx, "post", e.ID)
	}
	return e
}

func publishEntry(t *testing.T, svc ContentService, ctx context.Context, typeName string, id uuid.UUID) EntryDTO {
	t.Helper()
	got, err := svc.SetEntryStatus(ctx, typeName, id, domain.StatusPublished, 0)
	require.NoError(t, err)
	return got
}

// wireOf renders a DTO exactly as the HTTP layer would, so every assertion below
// is about the BYTES a client receives rather than about unexported struct
// state. `related` is an omitempty field with a custom marshaller; asserting on
// the Go value would miss both halves of that.
func wireJSON(t *testing.T, d EntryDTO) map[string]any {
	t.Helper()
	b, err := json.Marshal(d)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

func relatedOf(t *testing.T, d EntryDTO) map[string]any {
	t.Helper()
	w := wireJSON(t, d)
	rel, ok := w["related"]
	require.True(t, ok, "response carries no `related` key: %v", w)
	m, ok := rel.(map[string]any)
	require.True(t, ok, "`related` is not an object: %T", rel)
	return m
}

// --- parse-time refusals ------------------------------------------------------

// Four causes, four codes. Collapsing them into one "bad populate key" would be
// cheaper to implement and useless to the caller: a typo, a non-relation field,
// a nested path and a forbidden field have four different fixes and the response
// is the only place the difference can be stated.
func TestPopulate_ParseRefusals(t *testing.T) {
	svc, _, admin := populateFixture(t, nil, nil)
	a := mkAuthor(t, svc, admin, "one", true)
	p := mkPost(t, svc, admin, map[string]any{"title": "T", "author": a.ID.String()}, true)
	del := ctxDelivery("tenant-a")

	cases := []struct {
		name     string
		populate []string
		code     string
		status   int
	}{
		{"unknown key", []string{"nope"}, "CONTENT_POPULATE_FIELD_UNKNOWN", 400},
		{"not a relation", []string{"title"}, "CONTENT_POPULATE_NOT_RELATION", 400},
		{"nested path to a non-relation field", []string{"author.name"}, "CONTENT_POPULATE_NOT_RELATION", 400},
		{"nested path, unknown second segment", []string{"author.nope"}, "CONTENT_POPULATE_FIELD_UNKNOWN", 400},
		{"path deeper than the cap", []string{"author.name.foo.bar"}, "CONTENT_POPULATE_TOO_DEEP", 400},
	}
	for _, tc := range cases {
		t.Run(tc.name+" (list)", func(t *testing.T) {
			_, err := svc.ListEntries(del, "post", ListEntriesInput{Populate: tc.populate})
			assert.Equal(t, tc.code, codeOf(t, err))
		})
		t.Run(tc.name+" (get)", func(t *testing.T) {
			_, err := svc.GetEntry(del, "post", p.ID, GetEntryInput{Populate: tc.populate})
			assert.Equal(t, tc.code, codeOf(t, err))
		})
	}

	t.Run("unknown key names the readable relations, not every field", func(t *testing.T) {
		_, err := svc.ListEntries(del, "post", ListEntriesInput{Populate: []string{"nope"}})
		require.Error(t, err)
		d := detailsOf(t, err)
		assert.Equal(t, "nope", d["field"])
		assert.ElementsMatch(t, []string{"author", "authors"}, d["known"],
			"the hint must list the relation fields — and only those")
	})

	t.Run("a path deeper than the cap is refused as too-deep before any segment is looked up", func(t *testing.T) {
		_, err := svc.ListEntries(del, "post", ListEntriesInput{Populate: []string{"author.name.foo.bar"}})
		assert.Equal(t, "CONTENT_POPULATE_TOO_DEEP", codeOf(t, err))
		d := detailsOf(t, err)
		assert.Equal(t, "author.name.foo.bar", d["populate"])
		assert.Equal(t, maxPopulateDepth, d["max_depth"])
		assert.Equal(t, 4, d["depth"])
	})

	t.Run("an empty value is `no selection`, not an error", func(t *testing.T) {
		got, err := svc.ListEntries(del, "post", ListEntriesInput{Populate: []string{"", "  "}})
		require.NoError(t, err)
		require.Len(t, got.Items, 1)
		_, has := wireJSON(t, got.Items[0])["related"]
		assert.False(t, has, "an empty populate must behave exactly like an absent one")
	})
}

// --- audience --------------------------------------------------------------

// The admin audience expands too (ADR-006 Amendment 7), in the admin VIEW: the
// working copy at both ends. The parent's `data` is the draft, so the ids that
// get expanded are the draft's ids, and the targets come back as their working
// copies — which is what an editor is about to publish and therefore the only
// answer that lets them check a page before releasing it.
func TestPopulate_AdminExpandsTheWorkingCopy(t *testing.T) {
	svc, _, admin := populateFixture(t, nil, nil)
	a1 := mkAuthor(t, svc, admin, "one", true)
	a2 := mkAuthor(t, svc, admin, "two", true)
	p := mkPost(t, svc, admin, map[string]any{
		"title": "T", "author": a1.ID.String(), "authors": []any{a2.ID.String(), a1.ID.String()},
	}, true)

	t.Run("get", func(t *testing.T) {
		got, err := svc.GetEntry(admin, "post", p.ID, GetEntryInput{Populate: []string{"author", "authors"}})
		require.NoError(t, err)

		w := wireJSON(t, got)
		assert.Equal(t, a1.ID.String(), w["data"].(map[string]any)["author"],
			"`data` must still hold the raw id for admin too")

		rel := relatedOf(t, got)
		single, ok := rel["author"].(map[string]any)
		require.True(t, ok, "a scalar relation must expand to ONE object: %T", rel["author"])
		assert.Equal(t, "one", single["data"].(map[string]any)["name"])

		multi, ok := rel["authors"].([]any)
		require.True(t, ok, "a multi-valued relation must expand to an array: %T", rel["authors"])
		require.Len(t, multi, 2)
		// Same contract as delivery: the array order is the id order in `data`.
		assert.Equal(t, "two", multi[0].(map[string]any)["data"].(map[string]any)["name"])
		assert.Equal(t, "one", multi[1].(map[string]any)["data"].(map[string]any)["name"])
	})

	t.Run("list", func(t *testing.T) {
		list, err := svc.ListEntries(admin, "post", ListEntriesInput{Populate: []string{"author"}})
		require.NoError(t, err)
		require.Len(t, list.Items, 1)
		single := relatedOf(t, list.Items[0])["author"].(map[string]any)
		assert.Equal(t, "one", single["data"].(map[string]any)["name"])
		// Offset paging is untouched — the admin list still reports a total.
		assert.Equal(t, 1, mustTotal(t, list))
	})

	t.Run("still absent when not asked", func(t *testing.T) {
		got, err := svc.GetEntry(admin, "post", p.ID, GetEntryInput{})
		require.NoError(t, err)
		_, has := wireJSON(t, got)["related"]
		assert.False(t, has, "an admin read without populate is byte-identical to before")
	})
}

// The mirror of TestPopulate_ReadsTheSnapshotNotTheDraft, and it has to answer
// the OPPOSITE way. Delivery expanding the working copy would leak an unreleased
// edit; admin expanding the snapshot would show an editor the relation they
// already replaced, which is the one thing the admin API exists not to do.
//
// The draft's target here has NEVER been published, so this also pins the second
// half of Amendment 7: a row delivery cannot see at all is an ordinary draft to
// an editor, not a dangling reference.
func TestPopulate_AdminReadsTheDraftNotTheSnapshot(t *testing.T) {
	svc, _, admin := populateFixture(t, nil, nil)
	published := mkAuthor(t, svc, admin, "published-target", true)
	unreleased := mkAuthor(t, svc, admin, "never-published-target", false)
	p := mkPost(t, svc, admin, map[string]any{"title": "T", "author": published.ID.String()}, true)

	// The working copy now points at a target that has no snapshot at all.
	_, err := svc.UpdateEntry(admin, "post", p.ID, mustJSON(t, map[string]any{
		"title": "T", "author": unreleased.ID.String(),
	}), 0)
	require.NoError(t, err)

	got, err := svc.GetEntry(admin, "post", p.ID, GetEntryInput{Populate: []string{"author"}})
	require.NoError(t, err)
	single, ok := relatedOf(t, got)["author"].(map[string]any)
	require.True(t, ok, "a never-published target must expand for admin, not render null")
	assert.Equal(t, "never-published-target", single["data"].(map[string]any)["name"])
	assert.Equal(t, "draft", single["status"])

	// The same entry, same parameter, delivery credential: the snapshot's id,
	// expanded from the snapshot. Both halves in one test so a change that
	// collapsed the two views has to fail here.
	pub, err := svc.GetEntry(ctxDelivery("tenant-a"), "post", p.ID, GetEntryInput{Populate: []string{"author"}})
	require.NoError(t, err)
	assert.Equal(t, "published-target",
		relatedOf(t, pub)["author"].(map[string]any)["data"].(map[string]any)["name"])
}

// Deleted is the one "not there" that means the same thing to both audiences:
// the row is gone, so there is nothing to serve in any view. Null for a scalar,
// skipped for a multi — the delivery answer, because a dangling id must not take
// down the page that links to it for an editor either.
func TestPopulate_AdminDeletedTargetIsNullOrSkipped(t *testing.T) {
	svc, _, admin := populateFixture(t, nil, nil)
	kept := mkAuthor(t, svc, admin, "kept", false)
	doomed := mkAuthor(t, svc, admin, "doomed", false)
	p := mkPost(t, svc, admin, map[string]any{
		"title": "T", "author": doomed.ID.String(),
		"authors": []any{kept.ID.String(), doomed.ID.String()},
	}, false)
	require.NoError(t, svc.DeleteEntry(admin, "author", doomed.ID))

	got, err := svc.GetEntry(admin, "post", p.ID, GetEntryInput{Populate: []string{"author", "authors"}})
	require.NoError(t, err)
	rel := relatedOf(t, got)

	v, has := rel["author"]
	require.True(t, has, "the key must be present even when the target is gone")
	assert.Nil(t, v, "a scalar relation whose target was deleted renders null")

	multi := rel["authors"].([]any)
	require.Len(t, multi, 1, "the deleted element is skipped, not an error")
	assert.Equal(t, "kept", multi[0].(map[string]any)["data"].(map[string]any)["name"])
}

// The related entry carries the ADMIN shape, because it goes through the same
// projector under the same subject as the parent. `data` is the working copy and
// `published_data` is the snapshot beside it — the two-copy view is the whole
// reason an editor reads the admin API rather than the delivery one.
func TestPopulate_AdminRelatedEntryCarriesTheAdminShape(t *testing.T) {
	svc, _, admin := populateFixture(t, nil, nil)
	a := mkAuthor(t, svc, admin, "released", true)
	// The target now differs between its two copies.
	_, err := svc.UpdateEntry(admin, "author", a.ID, mustJSON(t, map[string]any{"name": "edited"}), 0)
	require.NoError(t, err)
	p := mkPost(t, svc, admin, map[string]any{"title": "T", "author": a.ID.String()}, true)

	got, err := svc.GetEntry(admin, "post", p.ID, GetEntryInput{Populate: []string{"author"}})
	require.NoError(t, err)
	single := relatedOf(t, got)["author"].(map[string]any)

	assert.Equal(t, "edited", single["data"].(map[string]any)["name"], "`data` is the working copy")
	assert.Equal(t, "released", single["published_data"].(map[string]any)["name"], "the snapshot rides beside it")
	assert.Equal(t, true, single["has_unpublished_changes"])
	for _, want := range []string{"updated_at", "created_by"} {
		assert.Contains(t, single, want, "an admin-audience entry keeps its admin fields")
	}
	// Still one level: a related entry is never itself populated.
	assert.NotContains(t, single, "related")
}

// The target collection's own read_roles gate the admin caller exactly as they
// gate a delivery one. The parent being readable says nothing about the
// collection at the other end, and populate is a read of that collection.
func TestPopulate_AdminTargetTypeReadRolesAreEnforced(t *testing.T) {
	svc, _, _ := populateFixture(t, nil, []string{"admin"})
	owner := ctxRole("tenant-a", "admin")
	a := mkAuthor(t, svc, owner, "one", false)
	p := mkPost(t, svc, owner, map[string]any{"title": "T", "author": a.ID.String()}, false)

	// An admin-audience caller who may read `post` but not `author`.
	editor := ctxRole("tenant-a", "editor")
	_, err := svc.GetEntry(editor, "post", p.ID, GetEntryInput{Populate: []string{"author"}})
	assert.Equal(t, "CONTENT_TYPE_READ_FORBIDDEN", codeOf(t, err))
	assert.Equal(t, 403, statusOf(t, err))

	_, err = svc.ListEntries(editor, "post", ListEntriesInput{Populate: []string{"author"}})
	assert.Equal(t, "CONTENT_TYPE_READ_FORBIDDEN", codeOf(t, err))

	// The parent row itself stays readable — this refuses the expansion, not the
	// entry.
	got, err := svc.GetEntry(editor, "post", p.ID, GetEntryInput{})
	require.NoError(t, err)
	assert.Contains(t, string(got.Data), a.ID.String())
}

// Field-level masking on the TARGET type applies to the admin expansion too. The
// caller's own field permissions are not waived one level down just because the
// parent was readable.
func TestPopulate_AdminRelatedEntryKeepsItsOwnFieldMasking(t *testing.T) {
	svc, _, _ := populateFixture(t, []FieldInput{
		{Key: "name", Type: domain.FieldTypeString},
		{Key: "email", Type: domain.FieldTypeString, ReadRoles: []string{"admin"}},
	}, nil)
	owner := ctxRole("tenant-a", "admin")
	a, err := svc.CreateEntry(owner, "author", mustJSON(t, map[string]any{"name": "one", "email": "a@example.com"}))
	require.NoError(t, err)
	p := mkPost(t, svc, owner, map[string]any{"title": "T", "author": a.ID.String()}, false)

	got, err := svc.GetEntry(ctxRole("tenant-a", "editor"), "post", p.ID, GetEntryInput{Populate: []string{"author"}})
	require.NoError(t, err)
	data := relatedOf(t, got)["author"].(map[string]any)["data"].(map[string]any)
	assert.Equal(t, "one", data["name"])
	assert.NotContains(t, data, "email", "a masked field must stay masked one level down")

	// The same read as the role that MAY see it, so the assertion above is about
	// the mask rather than about the key being absent from the payload.
	got, err = svc.GetEntry(owner, "post", p.ID, GetEntryInput{Populate: []string{"author"}})
	require.NoError(t, err)
	assert.Equal(t, "a@example.com",
		relatedOf(t, got)["author"].(map[string]any)["data"].(map[string]any)["email"])
}

// Own-only confinement, one level down. A role the TARGET type confines sees a
// colleague's row as absent through `related`, exactly as the target's own list
// would never return it — otherwise a relation field on an unconfined parent
// would be a way around the confinement.
//
// Absent rather than 403, for guardOwned's reason: a confined editor must not be
// able to tell a colleague's row from an id that names nothing.
func TestPopulate_AdminOwnOnlyConfinementAppliesToRelated(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("tenant-a", "owner")
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name: "author", Label: "Author",
		Fields:       []FieldInput{{Key: "name", Type: domain.FieldTypeString}},
		OwnOnlyRoles: []string{"editor"},
	})
	require.NoError(t, err)
	_, err = svc.CreateContentType(owner, CreateTypeInput{
		Name: "post", Label: "Post",
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "authors", Type: domain.FieldTypeRelation, RelationEntity: "author", Multiple: true},
		},
	})
	require.NoError(t, err)

	mine := uuid.New()
	editor := ctxRoleUser("tenant-a", "editor", mine)
	own, err := svc.CreateEntry(editor, "author", mustJSON(t, map[string]any{"name": "mine"}))
	require.NoError(t, err)
	theirs, err := svc.CreateEntry(ctxRoleUser("tenant-a", "editor", uuid.New()), "author",
		mustJSON(t, map[string]any{"name": "a colleague's"}))
	require.NoError(t, err)

	p, err := svc.CreateEntry(editor, "post", mustJSON(t, map[string]any{
		"title": "T", "authors": []any{own.ID.String(), theirs.ID.String()},
	}))
	require.NoError(t, err)

	got, err := svc.GetEntry(editor, "post", p.ID, GetEntryInput{Populate: []string{"authors"}})
	require.NoError(t, err)
	multi := relatedOf(t, got)["authors"].([]any)
	require.Len(t, multi, 1, "a confined role must not read a colleague's row through a relation")
	assert.Equal(t, "mine", multi[0].(map[string]any)["data"].(map[string]any)["name"])

	// The unconfined owner sees both, so the omission above is the confinement
	// rather than a broken fixture.
	got, err = svc.GetEntry(owner, "post", p.ID, GetEntryInput{Populate: []string{"authors"}})
	require.NoError(t, err)
	assert.Len(t, relatedOf(t, got)["authors"].([]any), 2)
}

// One query per page holds for the admin list too — the property populate exists
// for is not audience-specific, and an admin console renders exactly the pages
// that would otherwise cost N+1.
func TestPopulate_AdminOneQueryPerPage(t *testing.T) {
	svc, repo, admin := populateFixture(t, nil, nil)
	a1 := mkAuthor(t, svc, admin, "one", false)
	a2 := mkAuthor(t, svc, admin, "two", false)
	for range 3 {
		mkPost(t, svc, admin, map[string]any{
			"title": "T", "author": a1.ID.String(), "authors": []any{a1.ID.String(), a2.ID.String()},
		}, false)
	}

	list, err := svc.ListEntries(admin, "post", ListEntriesInput{Populate: []string{"author", "authors"}})
	require.NoError(t, err)
	require.Len(t, list.Items, 3)
	assert.Equal(t, 1, repo.byIDsCalls, "three entries × two fields must still be ONE query")
	assert.Len(t, repo.lastByIDs, 2, "nine id occurrences deduplicate to the two distinct authors")
}

// A preview credential names ONE entry. Populate is precisely a way to read
// others with it, so it stays a 403 about scope — the ONE audience still
// refused, after Amendment 7 granted admin the parameter.
func TestPopulate_RefusedForPreviewAudience(t *testing.T) {
	svc, _, admin := populateFixture(t, nil, nil)
	a := mkAuthor(t, svc, admin, "one", true)
	p := mkPost(t, svc, admin, map[string]any{"title": "T", "author": a.ID.String()}, false)

	_, err := svc.GetEntry(ctxPreview("tenant-a", p.ID), "post", p.ID, GetEntryInput{Populate: []string{"author"}})
	assert.Equal(t, "CONTENT_POPULATE_PREVIEW_UNSUPPORTED", codeOf(t, err))
	assert.Equal(t, 403, statusOf(t, err))

	// Without populate the preview still serves its working copy.
	got, err := svc.GetEntry(ctxPreview("tenant-a", p.ID), "post", p.ID, GetEntryInput{})
	require.NoError(t, err)
	assert.Contains(t, string(got.Data), a.ID.String())
}

// --- shapes ------------------------------------------------------------------

// The two cardinalities, and the invariant that binds them: `data` is unchanged
// by populate. A client that already decodes `author` as a string keeps working
// when another caller starts asking for the expansion.
func TestPopulate_ShapeFollowsTheSchemaNotTheData(t *testing.T) {
	svc, _, admin := populateFixture(t, nil, nil)
	a1 := mkAuthor(t, svc, admin, "one", true)
	a2 := mkAuthor(t, svc, admin, "two", true)
	p := mkPost(t, svc, admin, map[string]any{
		"title": "T", "author": a1.ID.String(), "authors": []any{a2.ID.String(), a1.ID.String()},
	}, true)
	del := ctxDelivery("tenant-a")

	got, err := svc.GetEntry(del, "post", p.ID, GetEntryInput{Populate: []string{"author", "authors"}})
	require.NoError(t, err)

	w := wireJSON(t, got)
	data := w["data"].(map[string]any)
	assert.Equal(t, a1.ID.String(), data["author"], "`data` must still hold the raw id")
	assert.Equal(t, []any{a2.ID.String(), a1.ID.String()}, data["authors"])

	rel := relatedOf(t, got)
	single, ok := rel["author"].(map[string]any)
	require.True(t, ok, "a scalar relation must expand to ONE object, not a one-element array: %T", rel["author"])
	assert.Equal(t, "one", single["data"].(map[string]any)["name"])

	multi, ok := rel["authors"].([]any)
	require.True(t, ok, "a multi-valued relation must expand to an array: %T", rel["authors"])
	require.Len(t, multi, 2)
	// Order is the contract: a client zips `related.authors` against
	// `data.authors`, which is the entire reason the ids stayed in `data`.
	assert.Equal(t, "two", multi[0].(map[string]any)["data"].(map[string]any)["name"])
	assert.Equal(t, "one", multi[1].(map[string]any)["data"].(map[string]any)["name"])
}

// Not requested, not present. `related` is omitempty precisely so that every
// consumer written before this change sees byte-identical responses.
func TestPopulate_AbsentWhenNotRequested(t *testing.T) {
	svc, _, admin := populateFixture(t, nil, nil)
	a := mkAuthor(t, svc, admin, "one", true)
	p := mkPost(t, svc, admin, map[string]any{"title": "T", "author": a.ID.String()}, true)
	del := ctxDelivery("tenant-a")

	got, err := svc.GetEntry(del, "post", p.ID, GetEntryInput{})
	require.NoError(t, err)
	_, has := wireJSON(t, got)["related"]
	assert.False(t, has, "no populate must mean no `related` key at all")

	list, err := svc.ListEntries(del, "post", ListEntriesInput{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	_, has = wireJSON(t, list.Items[0])["related"]
	assert.False(t, has)

	_ = p
}

// --- nothing live at the other end --------------------------------------------

// A relation pointing at a draft, a retracted entry, or a deleted one is not an
// error: delivery sees the live world and dangling references are the ordinary
// state of it. But the KEY still has to be there — "you asked and nothing is
// live" and "you did not ask" are different answers, and a consumer that cannot
// tell them apart retries a request that already succeeded.
func TestPopulate_MissingTargetsAreNullOrSkipped(t *testing.T) {
	svc, _, admin := populateFixture(t, nil, nil)
	live := mkAuthor(t, svc, admin, "live", true)
	draft := mkAuthor(t, svc, admin, "draft", false)
	retracted := mkAuthor(t, svc, admin, "retracted", true)
	_, err := svc.SetEntryStatus(admin, "author", retracted.ID, domain.StatusDraft, 0)
	require.NoError(t, err)

	p := mkPost(t, svc, admin, map[string]any{
		"title": "T", "author": draft.ID.String(),
		"authors": []any{live.ID.String(), draft.ID.String(), retracted.ID.String()},
	}, true)
	del := ctxDelivery("tenant-a")

	got, err := svc.GetEntry(del, "post", p.ID, GetEntryInput{Populate: []string{"author", "authors"}})
	require.NoError(t, err)
	rel := relatedOf(t, got)

	v, has := rel["author"]
	require.True(t, has, "the key must be present even when the target is not live")
	assert.Nil(t, v, "a scalar relation with no live target renders null")

	multi, ok := rel["authors"].([]any)
	require.True(t, ok)
	require.Len(t, multi, 1, "only the live target survives; the others are skipped, not errors")
	assert.Equal(t, "live", multi[0].(map[string]any)["data"].(map[string]any)["name"])

	// An unpublish RETAINS published_payload (migration 000033), so "live" has to
	// mean status published AND a snapshot — a by-ids fetch that only checked the
	// snapshot would have served `retracted` here.
	for _, item := range multi {
		assert.NotEqual(t, "retracted", item.(map[string]any)["data"].(map[string]any)["name"])
	}
}

// An empty multi-valued relation renders `[]`, not `null` and not an absent key.
// json.Marshal of a nil slice spells "no list", which is a different fact from
// "a list of nothing".
func TestPopulate_EmptyMultiRendersEmptyArray(t *testing.T) {
	svc, _, admin := populateFixture(t, nil, nil)
	p := mkPost(t, svc, admin, map[string]any{"title": "T", "authors": []any{}}, true)

	got, err := svc.GetEntry(ctxDelivery("tenant-a"), "post", p.ID, GetEntryInput{Populate: []string{"authors"}})
	require.NoError(t, err)
	b, err := json.Marshal(got)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"authors":[]`)
}

// The ids come from the PUBLISHED snapshot, never from the working copy. A draft
// that repoints a relation must not have that edit observable through `related`
// — the same leak class ADR-006 Amendment 4 closed for filters and 5 for sorts.
func TestPopulate_ReadsTheSnapshotNotTheDraft(t *testing.T) {
	svc, _, admin := populateFixture(t, nil, nil)
	published := mkAuthor(t, svc, admin, "published-target", true)
	secret := mkAuthor(t, svc, admin, "draft-only-target", true)
	p := mkPost(t, svc, admin, map[string]any{"title": "T", "author": published.ID.String()}, true)

	// The working copy now points somewhere else; the snapshot does not.
	_, err := svc.UpdateEntry(admin, "post", p.ID, mustJSON(t, map[string]any{
		"title": "T", "author": secret.ID.String(),
	}), 0)
	require.NoError(t, err)

	got, err := svc.GetEntry(ctxDelivery("tenant-a"), "post", p.ID, GetEntryInput{Populate: []string{"author"}})
	require.NoError(t, err)
	single := relatedOf(t, got)["author"].(map[string]any)
	assert.Equal(t, "published-target", single["data"].(map[string]any)["name"],
		"expanding the working copy's relation would leak an unreleased edit")
}

// --- projection independence ---------------------------------------------------

// ?fields= is the PARENT's projection. Applying it to a different type's payload
// would drop every key whose name happens not to be in the list, which for
// unrelated types is all of them.
func TestPopulate_FieldsDoesNotNarrowRelated(t *testing.T) {
	svc, _, admin := populateFixture(t, nil, nil)
	a := mkAuthor(t, svc, admin, "one", true)
	mkPost(t, svc, admin, map[string]any{"title": "T", "author": a.ID.String()}, true)

	list, err := svc.ListEntries(ctxDelivery("tenant-a"), "post", ListEntriesInput{
		Fields: []string{"title"}, Populate: []string{"author"},
	})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)

	w := wireJSON(t, list.Items[0])
	data := w["data"].(map[string]any)
	assert.Equal(t, []string{"title"}, keysOfMap(data), "the parent is narrowed as asked")

	single := w["related"].(map[string]any)["author"].(map[string]any)
	assert.Equal(t, "one", single["data"].(map[string]any)["name"],
		"`name` is not in ?fields= and must survive anyway — it is a different type's key")
}

func keysOfMap(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- permissions ---------------------------------------------------------------

// The target collection has its OWN read_roles, and populate is a read of it.
// Without this check a public type with a relation into a restricted one would
// hand the restricted entries out through the parent, whose permissions say
// nothing about them.
func TestPopulate_TargetTypeReadRolesAreEnforced(t *testing.T) {
	svc, _, _ := populateFixture(t, nil, []string{"admin"})
	// A restricted collection can only be written by someone who may read it,
	// so the fixture's member context cannot seed this one.
	admin := ctxRole("tenant-a", "admin")
	a := mkAuthor(t, svc, admin, "one", true)
	p := mkPost(t, svc, admin, map[string]any{"title": "T", "author": a.ID.String()}, true)
	del := ctxDelivery("tenant-a")

	_, err := svc.GetEntry(del, "post", p.ID, GetEntryInput{Populate: []string{"author"}})
	assert.Equal(t, "CONTENT_TYPE_READ_FORBIDDEN", codeOf(t, err))
	assert.Equal(t, 403, statusOf(t, err))

	// The parent itself stays readable — this refuses the expansion, not the row.
	got, err := svc.GetEntry(del, "post", p.ID, GetEntryInput{})
	require.NoError(t, err)
	assert.Contains(t, string(got.Data), a.ID.String())
}

// A relation field the caller may not READ cannot be expanded either, and the
// refusal is the same 403 filter/sort/fields give. Dropping the key silently
// would tell the caller the relation is empty.
func TestPopulate_UnreadableRelationFieldIsForbidden(t *testing.T) {
	svc, _, _ := populateFixture(t, nil, nil)
	admin := ctxRole("tenant-a", "admin")
	// Add a restricted relation to `post` after the fact via a second type, so
	// the fixture stays shared: a dedicated type is clearer than mutating one.
	_, err := svc.CreateContentType(admin, CreateTypeInput{
		Name: "review", Label: "Review",
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "secret_author", Type: domain.FieldTypeRelation, RelationEntity: "author", ReadRoles: []string{"admin"}},
		},
	})
	require.NoError(t, err)
	a := mkAuthor(t, svc, admin, "one", true)
	e, err := svc.CreateEntry(admin, "review", mustJSON(t, map[string]any{"title": "T", "secret_author": a.ID.String()}))
	require.NoError(t, err)
	publishEntry(t, svc, admin, "review", e.ID)

	_, err = svc.GetEntry(ctxDelivery("tenant-a"), "review", e.ID, GetEntryInput{Populate: []string{"secret_author"}})
	assert.Equal(t, "CONTENT_FIELD_QUERY_FORBIDDEN", codeOf(t, err))
	assert.Equal(t, "populate", detailsOf(t, err)["clause"])

	// And the 400 for a typo must not name it either — the hint would otherwise
	// disclose the half of the schema the 403 is careful to hide.
	_, err = svc.GetEntry(ctxDelivery("tenant-a"), "review", e.ID, GetEntryInput{Populate: []string{"nope"}})
	assert.Equal(t, "CONTENT_POPULATE_FIELD_UNKNOWN", codeOf(t, err))
	assert.Empty(t, detailsOf(t, err)["known"])
}

// The related entry is projected with the TARGET type's field permissions, not
// waived because the parent was readable.
func TestPopulate_RelatedEntryKeepsItsOwnFieldMasking(t *testing.T) {
	svc, _, _ := populateFixture(t, []FieldInput{
		{Key: "name", Type: domain.FieldTypeString},
		{Key: "email", Type: domain.FieldTypeString, ReadRoles: []string{"admin"}},
	}, nil)
	admin := ctxRole("tenant-a", "admin")
	a, err := svc.CreateEntry(admin, "author", mustJSON(t, map[string]any{"name": "one", "email": "a@example.com"}))
	require.NoError(t, err)
	publishEntry(t, svc, admin, "author", a.ID)
	p := mkPost(t, svc, admin, map[string]any{"title": "T", "author": a.ID.String()}, true)

	got, err := svc.GetEntry(ctxDelivery("tenant-a"), "post", p.ID, GetEntryInput{Populate: []string{"author"}})
	require.NoError(t, err)
	single := relatedOf(t, got)["author"].(map[string]any)
	data := single["data"].(map[string]any)
	assert.Equal(t, "one", data["name"])
	assert.NotContains(t, data, "email", "a masked field must stay masked one level down")
}

// A related entry is served with the delivery SHAPE too — the published
// snapshot, no updated_at, no authorship. It goes through the same projector as
// the parent precisely so this cannot drift.
func TestPopulate_RelatedEntryCarriesTheDeliveryShape(t *testing.T) {
	svc, _, admin := populateFixture(t, nil, nil)
	a := mkAuthor(t, svc, admin, "one", true)
	p := mkPost(t, svc, admin, map[string]any{"title": "T", "author": a.ID.String()}, true)

	got, err := svc.GetEntry(ctxDelivery("tenant-a"), "post", p.ID, GetEntryInput{Populate: []string{"author"}})
	require.NoError(t, err)
	single := relatedOf(t, got)["author"].(map[string]any)
	for _, banned := range []string{"updated_at", "created_by", "updated_by", "published_payload", "related"} {
		assert.NotContains(t, single, banned, "a related entry must carry the delivery shape")
	}
	assert.Equal(t, "published", single["status"])
}

// --- one query per page --------------------------------------------------------

// The whole economic case for populate is that it replaces N round trips with
// one. Nothing in the RESPONSE can hold that property, so the call count has to.
func TestPopulate_OneQueryPerPage(t *testing.T) {
	svc, repo, admin := populateFixture(t, nil, nil)
	a1 := mkAuthor(t, svc, admin, "one", true)
	a2 := mkAuthor(t, svc, admin, "two", true)
	for range 3 {
		mkPost(t, svc, admin, map[string]any{
			"title": "T", "author": a1.ID.String(), "authors": []any{a1.ID.String(), a2.ID.String()},
		}, true)
	}

	list, err := svc.ListEntries(ctxDelivery("tenant-a"), "post", ListEntriesInput{
		Populate: []string{"author", "authors"},
	})
	require.NoError(t, err)
	require.Len(t, list.Items, 3)

	assert.Equal(t, 1, repo.byIDsCalls, "three entries × two fields must still be ONE query")
	assert.Len(t, repo.lastByIDs, 2, "nine id occurrences deduplicate to the two distinct authors")
}

// A repeated ?populate=author&populate=author is what an unconditional URL
// builder emits. It must not double the id budget or emit the key twice.
func TestPopulate_RepeatedKeyIsIdempotent(t *testing.T) {
	svc, repo, admin := populateFixture(t, nil, nil)
	a := mkAuthor(t, svc, admin, "one", true)
	p := mkPost(t, svc, admin, map[string]any{"title": "T", "author": a.ID.String()}, true)

	got, err := svc.GetEntry(ctxDelivery("tenant-a"), "post", p.ID, GetEntryInput{
		Populate: []string{"author", "author"},
	})
	require.NoError(t, err)
	assert.Len(t, relatedOf(t, got), 1)
	assert.Equal(t, 1, repo.byIDsCalls)
	assert.Len(t, repo.lastByIDs, 1)
}

// --- the id budget --------------------------------------------------------------

// maxPopulateIDs is exercised against populateFor directly. Reaching 501 ids
// through the write path would need 501 author rows and a multi-valued field
// larger than domain.MaxMultipleElements allows; the boundary is a property of
// this function, and testing it here is what keeps the constant honest.
func TestPopulate_IDBudget(t *testing.T) {
	svc, _, admin := populateFixture(t, nil, nil)
	impl := svc.(*contentService)
	sub := authn.Subject{UserID: uuid.New(), TenantID: "tenant-a", TenantRole: "editor", PublicDelivery: true}
	ct, err := impl.repo.GetContentTypeByName(context.Background(), "tenant-a", "post")
	require.NoError(t, err)
	specs, err := impl.parsePopulate(context.Background(), ct, sub, []string{"authors"})
	require.NoError(t, err)
	_ = admin

	// A synthetic page, because the point is the id COUNT and the write path
	// would refuse ids that name nothing.
	page := func(n int) []*domain.Entry {
		ids := make([]string, 0, n)
		for range n {
			ids = append(ids, uuid.New().String())
		}
		body, err := json.Marshal(map[string]any{"title": "T", "authors": ids})
		require.NoError(t, err)
		return []*domain.Entry{{ID: uuid.New(), PublishedPayload: body}}
	}

	t.Run("at the cap it still runs", func(t *testing.T) {
		_, err := impl.populateFor(context.Background(), specs, page(maxPopulateIDs), sub)
		require.NoError(t, err)
	})

	t.Run("one past the cap is refused, not truncated", func(t *testing.T) {
		_, err := impl.populateFor(context.Background(), specs, page(maxPopulateIDs+1), sub)
		assert.Equal(t, "CONTENT_POPULATE_TOO_MANY", codeOf(t, err))
		assert.Equal(t, 400, statusOf(t, err))
		d := detailsOf(t, err)
		assert.Equal(t, maxPopulateIDs+1, d["count"])
		assert.Equal(t, maxPopulateIDs, d["max"])
		assert.NotEmpty(t, d["hint"], "the caller needs to be told which knob to turn")
	})

	// The SAME number for the admin audience. The cap is an abuse backstop on
	// the id set, not a property of which copy those ids came out of, and an
	// audience-specific budget is how one of the two quietly loses the backstop.
	t.Run("the admin audience gets the same budget", func(t *testing.T) {
		adminSub := authn.Subject{UserID: uuid.New(), TenantID: "tenant-a", TenantRole: "admin"}
		draftPage := func(n int) []*domain.Entry {
			// Ids in the WORKING copy, which is where the admin expansion reads
			// them; a snapshot-only page would spend nothing and pass for free.
			e := page(n)[0]
			return []*domain.Entry{{ID: e.ID, Payload: e.PublishedPayload}}
		}
		_, err := impl.populateFor(context.Background(), specs, draftPage(maxPopulateIDs), adminSub)
		require.NoError(t, err)
		_, err = impl.populateFor(context.Background(), specs, draftPage(maxPopulateIDs+1), adminSub)
		assert.Equal(t, "CONTENT_POPULATE_TOO_MANY", codeOf(t, err))
	})
}

// --- dotted paths (Amendment 8) ------------------------------------------------

// depthFixture builds a three-type chain — post -> author -> avatar — plus a
// self-relation on author (`mentor`), so a dotted path up to the depth cap has
// something real to walk and a self-reference has somewhere to loop.
func depthFixture(t *testing.T) (ContentService, *memRepo, context.Context) {
	t.Helper()
	svc, repo := newSvc()
	admin := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(admin, CreateTypeInput{
		Name: "avatar", Label: "Avatar",
		Fields: []FieldInput{{Key: "url", Type: domain.FieldTypeString}},
	})
	require.NoError(t, err)
	_, err = svc.CreateContentType(admin, CreateTypeInput{
		Name: "author", Label: "Author",
		Fields: []FieldInput{
			{Key: "name", Type: domain.FieldTypeString},
			{Key: "avatar", Type: domain.FieldTypeRelation, RelationEntity: "avatar"},
			{Key: "mentor", Type: domain.FieldTypeRelation, RelationEntity: "author"},
		},
	})
	require.NoError(t, err)
	_, err = svc.CreateContentType(admin, CreateTypeInput{
		Name: "post", Label: "Post",
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "author", Type: domain.FieldTypeRelation, RelationEntity: "author"},
		},
	})
	require.NoError(t, err)
	return svc, repo, admin
}

func mkAvatar(t *testing.T, svc ContentService, ctx context.Context, url string, publish bool) EntryDTO {
	t.Helper()
	e, err := svc.CreateEntry(ctx, "avatar", mustJSON(t, map[string]any{"url": url}))
	require.NoError(t, err)
	if publish {
		e = publishEntry(t, svc, ctx, "avatar", e.ID)
	}
	return e
}

// A dotted path expands one level PER SEGMENT: `author.mentor.avatar` walks
// post -> author -> mentor -> avatar, and each level's expansion lands in the
// SAME `related` key its own level's populate would have used, nested inside
// the entry ABOVE it — `related.author.related.mentor.related.avatar` — not in
// a flat sibling map. That nesting is what lets a client that only knows the
// single-level contract read one more level by walking the SAME key it already
// understands.
func TestPopulate_DottedPathExpandsMultipleLevels(t *testing.T) {
	svc, _, admin := depthFixture(t)
	avatarY := mkAvatar(t, svc, admin, "y.png", true)
	mentor, err := svc.CreateEntry(admin, "author", mustJSON(t, map[string]any{
		"name": "mentor-b", "avatar": avatarY.ID.String(),
	}))
	require.NoError(t, err)
	mentor = publishEntry(t, svc, admin, "author", mentor.ID)

	avatarX := mkAvatar(t, svc, admin, "x.png", true)
	authorA, err := svc.CreateEntry(admin, "author", mustJSON(t, map[string]any{
		"name": "author-a", "avatar": avatarX.ID.String(), "mentor": mentor.ID.String(),
	}))
	require.NoError(t, err)
	authorA = publishEntry(t, svc, admin, "author", authorA.ID)

	p := mkPost(t, svc, admin, map[string]any{"title": "T", "author": authorA.ID.String()}, true)
	del := ctxDelivery("tenant-a")

	t.Run("depth 2", func(t *testing.T) {
		got, err := svc.GetEntry(del, "post", p.ID, GetEntryInput{Populate: []string{"author.avatar"}})
		require.NoError(t, err)
		authorObj := relatedOf(t, got)["author"].(map[string]any)
		assert.Equal(t, "author-a", authorObj["data"].(map[string]any)["name"])
		nested, ok := authorObj["related"].(map[string]any)
		require.True(t, ok, "the populated author must itself carry `related`")
		assert.Equal(t, "x.png", nested["avatar"].(map[string]any)["data"].(map[string]any)["url"])
	})

	t.Run("depth 3", func(t *testing.T) {
		got, err := svc.GetEntry(del, "post", p.ID, GetEntryInput{Populate: []string{"author.mentor.avatar"}})
		require.NoError(t, err)
		authorObj := relatedOf(t, got)["author"].(map[string]any)
		l2, ok := authorObj["related"].(map[string]any)
		require.True(t, ok)
		mentorObj := l2["mentor"].(map[string]any)
		assert.Equal(t, "mentor-b", mentorObj["data"].(map[string]any)["name"])
		l3, ok := mentorObj["related"].(map[string]any)
		require.True(t, ok, "the third level must be reachable — the cap is 3, not 2")
		assert.Equal(t, "y.png", l3["avatar"].(map[string]any)["data"].(map[string]any)["url"])
	})

	t.Run("list forwards the same nesting", func(t *testing.T) {
		list, err := svc.ListEntries(del, "post", ListEntriesInput{Populate: []string{"author.avatar"}})
		require.NoError(t, err)
		require.Len(t, list.Items, 1)
		authorObj := relatedOf(t, list.Items[0])["author"].(map[string]any)
		nested := authorObj["related"].(map[string]any)
		assert.Equal(t, "x.png", nested["avatar"].(map[string]any)["data"].(map[string]any)["url"])
	})
}

// A self-referencing relation that points back at an entry already on the
// branch renders that entry — the caller asked for one more level and gets the
// object, not a hole — but does NOT expand it again: the depth budget had one
// more level to spend (3 requested, only 1 used), yet the walk stops anyway,
// because it recognised the loop rather than merely running out of budget.
func TestPopulate_SelfReferenceOnPathIsNotReExpanded(t *testing.T) {
	svc, repo, admin := depthFixture(t)
	a, err := svc.CreateEntry(admin, "author", mustJSON(t, map[string]any{"name": "loopy"}))
	require.NoError(t, err)
	_, err = svc.UpdateEntry(admin, "author", a.ID, mustJSON(t, map[string]any{
		"name": "loopy", "mentor": a.ID.String(),
	}), 0)
	require.NoError(t, err)
	a = publishEntry(t, svc, admin, "author", a.ID)
	p := mkPost(t, svc, admin, map[string]any{"title": "T", "author": a.ID.String()}, true)

	got, err := svc.GetEntry(ctxDelivery("tenant-a"), "post", p.ID, GetEntryInput{
		Populate: []string{"author.mentor.mentor"},
	})
	require.NoError(t, err)
	authorObj := relatedOf(t, got)["author"].(map[string]any)
	l2, ok := authorObj["related"].(map[string]any)
	require.True(t, ok, "level 1 -> level 2 is not itself a cycle: `a` is reached via `post`, not via itself")
	mentorObj := l2["mentor"].(map[string]any)
	assert.Equal(t, "loopy", mentorObj["data"].(map[string]any)["name"], "the self-reference still renders the entry")
	_, hasRelated := mentorObj["related"]
	assert.False(t, hasRelated,
		"an entry already on the branch must not be re-expanded, even with depth budget left")

	// level 1 (author) + level 2 (mentor) each cost one query; the pruned
	// level 3 must cost none — proof the walk was cut short by the cycle
	// guard and not merely by running to the end of a shorter chain.
	assert.Equal(t, 2, repo.byIDsCalls)
}

// The nested audience rules are the SAME ones the top level already enforces,
// reused rather than re-implemented — this is the read half of that reuse. A
// collection this credential may not read at all is refused at PARSE time,
// whatever depth it is named at: the parent being readable does not open a
// restricted collection two hops down any more than it does one hop down.
func TestPopulate_NestedTargetTypeReadRolesAreEnforced(t *testing.T) {
	svc, _, admin := depthFixture(t)
	// Schema mutation runs under the allow-all authorizer regardless of role
	// (newSvc), but DATA-level read/write roles bind from here on — so the
	// entries below are written by a subject whose TenantRole is actually
	// "admin", not by the role-less fixture subject `admin` is named for.
	adminRole := ctxRole("tenant-a", "admin")
	_, err := svc.CreateContentType(admin, CreateTypeInput{
		Name: "secret_avatar", Label: "Secret Avatar",
		Fields:    []FieldInput{{Key: "url", Type: domain.FieldTypeString}},
		ReadRoles: []string{"admin"},
	})
	require.NoError(t, err)
	_, err = svc.CreateContentType(admin, CreateTypeInput{
		Name: "guarded_author", Label: "Guarded Author",
		Fields: []FieldInput{
			{Key: "name", Type: domain.FieldTypeString},
			{Key: "avatar", Type: domain.FieldTypeRelation, RelationEntity: "secret_avatar"},
		},
	})
	require.NoError(t, err)
	_, err = svc.CreateContentType(admin, CreateTypeInput{
		Name: "guarded_post", Label: "Guarded Post",
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "author", Type: domain.FieldTypeRelation, RelationEntity: "guarded_author"},
		},
	})
	require.NoError(t, err)
	sa, err := svc.CreateEntry(adminRole, "secret_avatar", mustJSON(t, map[string]any{"url": "s.png"}))
	require.NoError(t, err)
	sa = publishEntry(t, svc, adminRole, "secret_avatar", sa.ID)
	ga, err := svc.CreateEntry(adminRole, "guarded_author", mustJSON(t, map[string]any{
		"name": "g", "avatar": sa.ID.String(),
	}))
	require.NoError(t, err)
	ga = publishEntry(t, svc, adminRole, "guarded_author", ga.ID)
	gp, err := svc.CreateEntry(adminRole, "guarded_post", mustJSON(t, map[string]any{
		"title": "T", "author": ga.ID.String(),
	}))
	require.NoError(t, err)
	gp = publishEntry(t, svc, adminRole, "guarded_post", gp.ID)

	del := ctxDelivery("tenant-a")
	_, err = svc.GetEntry(del, "guarded_post", gp.ID, GetEntryInput{Populate: []string{"author.avatar"}})
	assert.Equal(t, "CONTENT_TYPE_READ_FORBIDDEN", codeOf(t, err))
	assert.Equal(t, 403, statusOf(t, err))

	// The top level alone, and the entry itself, stay readable — this refuses
	// the SECOND level's expansion, not the request as a whole.
	got, err := svc.GetEntry(del, "guarded_post", gp.ID, GetEntryInput{Populate: []string{"author"}})
	require.NoError(t, err)
	assert.Equal(t, "g", relatedOf(t, got)["author"].(map[string]any)["data"].(map[string]any)["name"])
}

// The own-only rule applies at EVERY level, not merely the first. A row a
// confined caller may not own is omitted from a NESTED `related` for the exact
// reason it is omitted from the top-level one: absent, not 403, so a colleague's
// row cannot be distinguished from an id that names nothing.
func TestPopulate_NestedOwnOnlyConfinementIsOmitted(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("tenant-a", "owner")
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name: "author", Label: "Author",
		Fields: []FieldInput{
			{Key: "name", Type: domain.FieldTypeString},
			{Key: "mentor", Type: domain.FieldTypeRelation, RelationEntity: "author"},
		},
		OwnOnlyRoles: []string{"editor"},
	})
	require.NoError(t, err)
	_, err = svc.CreateContentType(owner, CreateTypeInput{
		Name: "post", Label: "Post",
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "author", Type: domain.FieldTypeRelation, RelationEntity: "author"},
		},
	})
	require.NoError(t, err)

	mine := uuid.New()
	editorA := ctxRoleUser("tenant-a", "editor", mine)
	colleague, err := svc.CreateEntry(ctxRoleUser("tenant-a", "editor", uuid.New()), "author",
		mustJSON(t, map[string]any{"name": "a colleague's mentor"}))
	require.NoError(t, err)
	main, err := svc.CreateEntry(editorA, "author", mustJSON(t, map[string]any{
		"name": "mine", "mentor": colleague.ID.String(),
	}))
	require.NoError(t, err)
	p, err := svc.CreateEntry(editorA, "post", mustJSON(t, map[string]any{
		"title": "T", "author": main.ID.String(),
	}))
	require.NoError(t, err)

	got, err := svc.GetEntry(editorA, "post", p.ID, GetEntryInput{Populate: []string{"author.mentor"}})
	require.NoError(t, err)
	authorObj := relatedOf(t, got)["author"].(map[string]any)
	assert.Equal(t, "mine", authorObj["data"].(map[string]any)["name"], "the level the caller owns is readable")
	nested, ok := authorObj["related"].(map[string]any)
	require.True(t, ok, "populate=author.mentor still asked for the second level")
	assert.Nil(t, nested["mentor"], "a colleague's row must not leak through the second level")

	// The unconfined owner sees it, so the omission above is the confinement
	// rather than a broken fixture.
	got, err = svc.GetEntry(owner, "post", p.ID, GetEntryInput{Populate: []string{"author.mentor"}})
	require.NoError(t, err)
	nested = relatedOf(t, got)["author"].(map[string]any)["related"].(map[string]any)
	assert.Equal(t, "a colleague's mentor", nested["mentor"].(map[string]any)["data"].(map[string]any)["name"])
}

// The 500-id cap is a property of the whole request, not of any one level: a
// path that stays under the cap at EVERY level individually must still be
// refused once the DEDUPLICATED total across levels crosses it.
func TestPopulate_IDBudgetAppliesAcrossLevels(t *testing.T) {
	svc, _, admin := populateFixture(t, nil, nil)
	impl := svc.(*contentService)
	_, err := svc.AddField(admin, "author", FieldInput{
		Key: "avatars", Type: domain.FieldTypeRelation, RelationEntity: "author", Multiple: true,
	})
	require.NoError(t, err)

	authorCT, err := impl.repo.GetContentTypeByName(context.Background(), "tenant-a", "author")
	require.NoError(t, err)

	// Each author's own second-level fan-out is one FRESH, well-formed id — it
	// need not resolve to a real entry, because the budget counts what a page
	// NAMES, before anything is fetched. Written straight into the repo, below
	// the service's write path, because that path validates a relation target
	// exists — the thing this fixture deliberately does not give it.
	const perLevel = 260 // under the cap alone at every level; over it combined
	authorIDs := make([]string, 0, perLevel)
	for range perLevel {
		body, err := json.Marshal(map[string]any{
			"name": "a", "avatars": []any{uuid.New().String()},
		})
		require.NoError(t, err)
		e := &domain.Entry{
			ID: uuid.New(), TenantID: "tenant-a", ContentTypeID: authorCT.ID,
			// Distinct per entry — the fake mirrors the (tenant, translation_group,
			// locale) unique index, and 260 rows sharing the zero group id would
			// collide with each other on the very first duplicate check.
			TranslationGroupID: uuid.New(),
			Payload:            body, Status: domain.StatusDraft,
		}
		require.NoError(t, impl.repo.CreateEntry(context.Background(), e))
		authorIDs = append(authorIDs, e.ID.String())
	}

	ct, err := impl.repo.GetContentTypeByName(context.Background(), "tenant-a", "post")
	require.NoError(t, err)
	sub := authn.Subject{UserID: uuid.New(), TenantID: "tenant-a", TenantRole: "admin"}
	specs, err := impl.parsePopulate(context.Background(), ct, sub, []string{"authors.avatars"})
	require.NoError(t, err)

	body, err := json.Marshal(map[string]any{"title": "T", "authors": authorIDs})
	require.NoError(t, err)
	root := []*domain.Entry{{ID: uuid.New(), Payload: body}}

	_, err = impl.populateFor(context.Background(), specs, root, sub)
	assert.Equal(t, "CONTENT_POPULATE_TOO_MANY", codeOf(t, err))
	d := detailsOf(t, err)
	count, ok := d["count"].(int)
	require.True(t, ok)
	assert.Greater(t, count, maxPopulateIDs, "level 1 (%d) alone is under the cap; only the sum crosses it", perLevel)
}

// --- cross-tenant isolation and multi-node cycles (verifier findings, feat/delivery-dx) ---

// A nested populate level reads GetEntriesByIDs with the SAME sub.TenantID at
// every depth (populateLevel never re-derives tenancy from the target row), so
// a row that happens to share its id with a live, published entry in ANOTHER
// tenant must still come back absent one level down — exactly as it would at
// the top level. This plants that decoy directly in the fake repo (bypassing
// the write path, which would itself refuse a cross-tenant relation target)
// so the id collision is real rather than merely impossible to construct.
func TestPopulate_NestedTargetTenantIsolationIsEnforced(t *testing.T) {
	svc, repo, admin := depthFixture(t)

	avatarCT, err := repo.GetContentTypeByName(context.Background(), "tenant-a", "avatar")
	require.NoError(t, err)
	authorCT, err := repo.GetContentTypeByName(context.Background(), "tenant-a", "author")
	require.NoError(t, err)

	// A live, published row in a DIFFERENT tenant, deliberately given the id
	// author-a's `avatar` field will name. GetEntriesByIDs does not bind
	// content_type_id (see postgres_repository.go), so tenant_id is the only
	// thing standing between this row and the response — which is exactly the
	// property this test pins.
	decoyID := uuid.New()
	decoyPayload := mustJSON(t, map[string]any{"url": "leaked.png"})
	decoy := &domain.Entry{
		ID: decoyID, TenantID: "tenant-b", ContentTypeID: avatarCT.ID,
		TranslationGroupID: uuid.New(),
		Payload:            decoyPayload, PublishedPayload: decoyPayload,
		Status: domain.StatusPublished,
	}
	require.NoError(t, repo.CreateEntry(context.Background(), decoy))

	// author-a lives in tenant-a and points `avatar` at the tenant-b id above.
	// Written straight into the repo, below the service's write path, because
	// checkRelations would itself refuse a target the writing tenant cannot
	// see — the thing this fixture needs to bypass to construct the collision.
	authorPayload := mustJSON(t, map[string]any{"name": "author-a", "avatar": decoyID.String()})
	authorA := &domain.Entry{
		ID: uuid.New(), TenantID: "tenant-a", ContentTypeID: authorCT.ID,
		TranslationGroupID: uuid.New(),
		Payload:            authorPayload, PublishedPayload: authorPayload,
		Status: domain.StatusPublished,
	}
	require.NoError(t, repo.CreateEntry(context.Background(), authorA))

	p := mkPost(t, svc, admin, map[string]any{"title": "T", "author": authorA.ID.String()}, true)

	got, err := svc.GetEntry(ctxDelivery("tenant-a"), "post", p.ID, GetEntryInput{Populate: []string{"author.avatar"}})
	require.NoError(t, err)
	authorObj := relatedOf(t, got)["author"].(map[string]any)
	assert.Equal(t, "author-a", authorObj["data"].(map[string]any)["name"])
	nested, ok := authorObj["related"].(map[string]any)
	require.True(t, ok, "populate=author.avatar still asked for the second level")
	v, has := nested["avatar"]
	require.True(t, has, "the key must be present even though the target is not visible")
	assert.Nil(t, v, "a row belonging to a different tenant must not be reachable through nested populate, even sharing an id with one that is live")
}

// The mirror of TestPopulate_SelfReferenceOnPathIsNotReExpanded for a genuine
// two-node cycle: author A's mentor is B, and B's mentor is A — no entry
// points at itself, so the guard has to recognise a PATH it is walking, not
// merely a row equal to the one just fetched. `populate=author.mentor.mentor`
// walks post -> A -> B -> back to A, and the third level must render A again
// (the caller asked for it) without a `related` of its own and without a
// fourth query — proof the walk terminates on the cycle rather than merely
// running out of the depth budget it happened to also be at the end of.
func TestPopulate_TwoNodeCycleIsNotReExpanded(t *testing.T) {
	svc, repo, admin := depthFixture(t)

	a, err := svc.CreateEntry(admin, "author", mustJSON(t, map[string]any{"name": "author-a"}))
	require.NoError(t, err)
	b, err := svc.CreateEntry(admin, "author", mustJSON(t, map[string]any{
		"name": "author-b", "mentor": a.ID.String(),
	}))
	require.NoError(t, err)
	_, err = svc.UpdateEntry(admin, "author", a.ID, mustJSON(t, map[string]any{
		"name": "author-a", "mentor": b.ID.String(),
	}), 0)
	require.NoError(t, err)
	a = publishEntry(t, svc, admin, "author", a.ID)
	_ = publishEntry(t, svc, admin, "author", b.ID)
	p := mkPost(t, svc, admin, map[string]any{"title": "T", "author": a.ID.String()}, true)

	got, err := svc.GetEntry(ctxDelivery("tenant-a"), "post", p.ID, GetEntryInput{
		Populate: []string{"author.mentor.mentor"},
	})
	require.NoError(t, err)

	authorObj := relatedOf(t, got)["author"].(map[string]any)
	assert.Equal(t, "author-a", authorObj["data"].(map[string]any)["name"])
	l2, ok := authorObj["related"].(map[string]any)
	require.True(t, ok, "level 1 -> level 2 (A -> B) is not itself on the path yet")
	mentorB := l2["mentor"].(map[string]any)
	assert.Equal(t, "author-b", mentorB["data"].(map[string]any)["name"])
	l3, ok := mentorB["related"].(map[string]any)
	require.True(t, ok, "level 2 -> level 3 (B -> A) still runs — A was not yet ON the path at level 2")
	backToA := l3["mentor"].(map[string]any)
	assert.Equal(t, "author-a", backToA["data"].(map[string]any)["name"], "the cycle still renders the entry it closes on")
	_, hasRelated := backToA["related"]
	assert.False(t, hasRelated, "A must not be re-expanded once it reappears on its own branch's path")

	// One query per level actually walked (author, mentor=B, mentor=A) and no
	// more — a broken guard that re-expanded A would cost a fourth query here.
	assert.Equal(t, 3, repo.byIDsCalls)
}

// --- relationIDs (pure) ----------------------------------------------------------

// Cardinality is read from the SCHEMA, not from what the stored JSON happens to
// look like. Stored data that predates or bypassed validation must make an entry
// unexpandable, never unservable.
func TestRelationIDs_ToleratesMalformedStoredValues(t *testing.T) {
	id := uuid.New()
	cases := []struct {
		name     string
		raw      string
		multiple bool
		want     int
	}{
		{"scalar id", `"` + id.String() + `"`, false, 1},
		{"scalar null", `null`, false, 0},
		{"scalar not a uuid", `"abc"`, false, 0},
		{"scalar holding an array", `["` + id.String() + `"]`, false, 0},
		{"array", `["` + id.String() + `","` + id.String() + `"]`, true, 2},
		{"array with one bad element", `["` + id.String() + `","abc"]`, true, 1},
		{"array holding a scalar", `"` + id.String() + `"`, true, 0},
		{"absent", ``, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Len(t, relationIDs(json.RawMessage(tc.raw), tc.multiple), tc.want)
		})
	}
}
