package service

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// ADR-024 (2.8a / 2.6a). These exercise the service methods through the
// memRepo fake — draft/published merge, agent-whitelist row filtering, and the
// force-delete / in-use gate. The SQL itself (referencedByQuery, the schema-
// aware backfill) is repository-layer and covered by the integration tests in
// internal/cms/content/repository instead.

// TestEntryReferencedBy_MergesDraftAndPublished pins the three states a
// referrer can be in — draft only, then draft AND published once the post is
// published — surviving independently rather than collapsing to one bool.
func TestEntryReferencedBy_MergesDraftAndPublished(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	a1, _ := relationFixture(t, svc, ctx,
		FieldInput{Key: "author", Type: domain.FieldTypeRelation, RelationEntity: "author"})

	post, err := svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{"title": "T", "author": a1}))
	require.NoError(t, err)
	authorID := uuid.MustParse(a1)

	got, err := svc.EntryReferencedBy(ctx, "author", authorID, 0, 0)
	require.NoError(t, err)
	require.Len(t, got.Items, 1)
	assert.Equal(t, post.ID, got.Items[0].EntryID)
	assert.Equal(t, "post", got.Items[0].Type)
	assert.Equal(t, "author", got.Items[0].Field)
	assert.True(t, got.Items[0].Draft, "the reference is in the working copy")
	assert.False(t, got.Items[0].Published, "nothing has been published yet")
	assert.Equal(t, 1, got.Total)

	_, err = svc.SetEntryStatus(ctx, "post", post.ID, domain.StatusPublished, 0)
	require.NoError(t, err)

	got, err = svc.EntryReferencedBy(ctx, "author", authorID, 0, 0)
	require.NoError(t, err)
	require.Len(t, got.Items, 1)
	assert.True(t, got.Items[0].Draft, "the working copy still names it")
	assert.True(t, got.Items[0].Published, "and now the published snapshot does too")
}

// TestEntryReferencedBy_AgentWhitelistFiltersRowsNotTotal is the same
// acceptance toComponentDTO's used_by already has (ADR-020): an agent
// credential's Total counts the FULL match set at the database, and only the
// per-row projection narrows to what the whitelist allows.
func TestEntryReferencedBy_AgentWhitelistFiltersRowsNotTotal(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxTenant("t1")
	a1, _ := relationFixture(t, svc, owner,
		FieldInput{Key: "author", Type: domain.FieldTypeRelation, RelationEntity: "author"})
	_, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "T1", "author": a1}))
	require.NoError(t, err)

	_, err = svc.CreateContentType(owner, CreateTypeInput{
		Name:  "note",
		Label: "Note",
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "author", Type: domain.FieldTypeRelation, RelationEntity: "author"},
		},
	})
	require.NoError(t, err)
	_, err = svc.CreateEntry(owner, "note", mustJSON(t, map[string]any{"title": "N1", "author": a1}))
	require.NoError(t, err)

	authorID := uuid.MustParse(a1)
	principal := uuid.New()
	// "author" itself must be in the whitelist too — EntryReferencedBy
	// authorizes against the TARGET's own type.
	agent := ctxAgent("t1", "editor", principal, []string{"author", "post"})

	got, err := svc.EntryReferencedBy(agent, "author", authorID, 0, 0)
	require.NoError(t, err)
	require.Len(t, got.Items, 1, "the note referrer is outside the whitelist and must be dropped")
	assert.Equal(t, "post", got.Items[0].Type)
	assert.Equal(t, 2, got.Total, "Total is the database's full count, not the narrowed one")
}

// TestEntryReferencedBy_RoleReadRolesFiltersRowsNotTotal is the HUMAN half of
// the same leak the agent-whitelist test above covers: a referrer can be of
// ANY type, not just the one the caller asked about, so an editor whose role
// is excluded from a DIFFERENT type's ReadRoles must not learn — via
// referenced-by alone — that a restricted type even has an entry naming this
// author, let alone its id or title. Total still reads the database's full
// count, matching the agent case and toComponentDTO's used_by precedent
// (ADR-024 §Known limitations).
func TestEntryReferencedBy_RoleReadRolesFiltersRowsNotTotal(t *testing.T) {
	svc, _ := newSvc()
	// owner both creates the schema and is IN the restricted type's ReadRoles,
	// so the setup writes are not themselves blocked by the gate under test.
	owner := ctxRole("t1", "owner")
	a1, _ := relationFixture(t, svc, owner,
		FieldInput{Key: "author", Type: domain.FieldTypeRelation, RelationEntity: "author"})
	_, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "T1", "author": a1}))
	require.NoError(t, err)

	_, err = svc.CreateContentType(owner, CreateTypeInput{
		Name:      "secret",
		Label:     "Secret",
		ReadRoles: []string{"admin", "owner"}, // editor excluded
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "author", Type: domain.FieldTypeRelation, RelationEntity: "author"},
		},
	})
	require.NoError(t, err)
	_, err = svc.CreateEntry(owner, "secret", mustJSON(t, map[string]any{"title": "S1", "author": a1}))
	require.NoError(t, err)

	authorID := uuid.MustParse(a1)
	editor := ctxRole("t1", "editor")

	got, err := svc.EntryReferencedBy(editor, "author", authorID, 0, 0)
	require.NoError(t, err)
	require.Len(t, got.Items, 1, "the secret referrer's type excludes editor from ReadRoles and must be dropped")
	assert.Equal(t, "post", got.Items[0].Type)
	assert.Equal(t, 2, got.Total, "Total is the database's full count, not the role-narrowed one")
}

// TestMediaReferencedBy_MergesDraftAndPublished mirrors the entry test for
// entry_media / entry_media_published.
func TestMediaReferencedBy_MergesDraftAndPublished(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")
	require.NoError(t, seedFileType(t, svc, ctx))
	asset := uploadAsset(t, svc, repo, store, ctx)

	doc, err := svc.CreateEntry(ctx, "doc", mustJSON(t, map[string]any{"title": "T", "cover": asset.String()}))
	require.NoError(t, err)

	got, err := svc.MediaReferencedBy(ctx, asset, 0, 0)
	require.NoError(t, err)
	require.Len(t, got.Items, 1)
	assert.Equal(t, doc.ID, got.Items[0].EntryID)
	assert.Equal(t, "doc", got.Items[0].Type)
	assert.Empty(t, got.Items[0].Field, "entry_media carries no field-level granularity")
	assert.True(t, got.Items[0].Draft)
	assert.False(t, got.Items[0].Published)

	_, err = svc.SetEntryStatus(ctx, "doc", doc.ID, domain.StatusPublished, 0)
	require.NoError(t, err)

	got, err = svc.MediaReferencedBy(ctx, asset, 0, 0)
	require.NoError(t, err)
	require.Len(t, got.Items, 1)
	assert.True(t, got.Items[0].Draft)
	assert.True(t, got.Items[0].Published)
}

// TestMediaReferencedBy_AgentCredentialIsRefused: media authorization is
// always untyped (a media asset belongs to no content type until an entry
// links it), and §4 hard-refuses ANY agent credential an untyped call — so an
// agent never reaches row-filtering at all, unlike the entry endpoint.
func TestMediaReferencedBy_AgentCredentialIsRefused(t *testing.T) {
	svc, repo, store := newMediaSvc()
	admin := ctxTenant("t1")
	require.NoError(t, seedFileType(t, svc, admin))
	asset := uploadAsset(t, svc, repo, store, admin)

	agent := ctxAgent("t1", "editor", uuid.New(), []string{"doc"})
	_, err := svc.MediaReferencedBy(agent, asset, 0, 0)
	assert.Equal(t, "CONTENT_AGENT_SCOPE_UNTYPED", codeOf(t, err))
}

// TestMediaReferencedBy_RoleReadRolesFiltersRowsNotTotal is the human-role
// counterpart of TestEntryReferencedBy_RoleReadRolesFiltersRowsNotTotal for
// media: entry_media rows carry the REFERRING entry's own type, and an editor
// excluded from one referrer's type must not see that row, even though the
// media asset itself has no type of its own to gate on.
func TestMediaReferencedBy_RoleReadRolesFiltersRowsNotTotal(t *testing.T) {
	svc, repo, store := newMediaSvc()
	owner := ctxRole("t1", "owner")
	require.NoError(t, seedFileType(t, svc, owner))
	asset := uploadAsset(t, svc, repo, store, owner)

	_, err := svc.CreateEntry(owner, "doc", mustJSON(t, map[string]any{"title": "T", "cover": asset.String()}))
	require.NoError(t, err)

	_, err = svc.CreateContentType(owner, CreateTypeInput{
		Name:      "secretdoc",
		Label:     "Secret Doc",
		ReadRoles: []string{"admin", "owner"}, // editor excluded
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "cover", Type: domain.FieldTypeFile},
		},
	})
	require.NoError(t, err)
	_, err = svc.CreateEntry(owner, "secretdoc", mustJSON(t, map[string]any{"title": "S", "cover": asset.String()}))
	require.NoError(t, err)

	editor := ctxRole("t1", "editor")
	got, err := svc.MediaReferencedBy(editor, asset, 0, 0)
	require.NoError(t, err)
	require.Len(t, got.Items, 1, "the secretdoc referrer's type excludes editor from ReadRoles and must be dropped")
	assert.Equal(t, "doc", got.Items[0].Type)
	assert.Equal(t, 2, got.Total, "Total is the database's full count, not the role-narrowed one")
}

// TestDeleteMediaAsset_RefusedWhenStillReferenced is the 409 half of ADR-024
// §5: a bare DELETE (force defaults to false at the handler) must not remove
// an asset a live entry still names.
func TestDeleteMediaAsset_RefusedWhenStillReferenced(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")
	require.NoError(t, seedFileType(t, svc, ctx))
	asset := uploadAsset(t, svc, repo, store, ctx)
	doc, err := svc.CreateEntry(ctx, "doc", mustJSON(t, map[string]any{"title": "T", "cover": asset.String()}))
	require.NoError(t, err)

	err = svc.DeleteMediaAsset(ctx, asset, false)
	assert.Equal(t, "CONTENT_MEDIA_IN_USE", codeOf(t, err))
	d := detailsOf(t, err)
	assert.Equal(t, 1, d["total"])
	entries, ok := d["entries"].([]map[string]any)
	require.True(t, ok, "details.entries has the wrong shape: %#v", d["entries"])
	require.Len(t, entries, 1)
	assert.Equal(t, doc.ID, entries[0]["entry_id"])

	// The asset must still be there — a refused delete is not a partial one.
	_, err = svc.GetMediaAsset(ctx, asset)
	require.NoError(t, err)
}

// TestDeleteMediaAsset_ForceBypassesTheCheck: ?force=true is the pre-ADR-024
// behavior verbatim — the check is skipped and the delete proceeds even though
// something still points at the asset.
func TestDeleteMediaAsset_ForceBypassesTheCheck(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")
	require.NoError(t, seedFileType(t, svc, ctx))
	asset := uploadAsset(t, svc, repo, store, ctx)
	_, err := svc.CreateEntry(ctx, "doc", mustJSON(t, map[string]any{"title": "T", "cover": asset.String()}))
	require.NoError(t, err)

	require.NoError(t, svc.DeleteMediaAsset(ctx, asset, true))
	_, err = svc.GetMediaAsset(ctx, asset)
	require.Error(t, err, "force=true must delete despite the still-live reference")
}

// TestDeleteMediaAsset_409DetailsFilteredByRole: the block itself does not
// depend on visibility (an asset a restricted type still names must stay
// refused even for a caller who cannot see that type — deleting it out from
// under a type you cannot even read would be a much worse leak than the
// refusal), but the 409's `details.entries` preview must apply the SAME
// ReadRoles filter EntryReferencedBy/MediaReferencedBy do, or a low-privilege
// caller could learn a restricted type's entry id by provoking the delete
// refusal on an asset they CAN otherwise see. `total` in the details is left
// unfiltered, matching ReferencedByDTO.Total.
func TestDeleteMediaAsset_409DetailsFilteredByRole(t *testing.T) {
	svc, repo, store := newMediaSvc()
	owner := ctxRole("t1", "owner")
	require.NoError(t, seedFileType(t, svc, owner))
	asset := uploadAsset(t, svc, repo, store, owner)

	_, err := svc.CreateEntry(owner, "doc", mustJSON(t, map[string]any{"title": "T", "cover": asset.String()}))
	require.NoError(t, err)

	_, err = svc.CreateContentType(owner, CreateTypeInput{
		Name:      "secretdoc",
		Label:     "Secret Doc",
		ReadRoles: []string{"admin", "owner"}, // editor excluded
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "cover", Type: domain.FieldTypeFile},
		},
	})
	require.NoError(t, err)
	_, err = svc.CreateEntry(owner, "secretdoc", mustJSON(t, map[string]any{"title": "S", "cover": asset.String()}))
	require.NoError(t, err)

	editor := ctxRole("t1", "editor")
	err = svc.DeleteMediaAsset(editor, asset, false)
	assert.Equal(t, "CONTENT_MEDIA_IN_USE", codeOf(t, err), "still referenced by secretdoc, so the block itself must hold regardless of the caller's visibility")

	d := detailsOf(t, err)
	assert.Equal(t, 2, d["total"], "total is unfiltered, matching ReferencedByDTO.Total")
	entries, ok := d["entries"].([]map[string]any)
	require.True(t, ok, "details.entries has the wrong shape: %#v", d["entries"])
	require.Len(t, entries, 1, "the secretdoc row must be dropped from the preview even though it still counts toward total")
	assert.Equal(t, "doc", entries[0]["type"])

	// Still there — a refused delete is not a partial one.
	_, err = svc.GetMediaAsset(owner, asset)
	require.NoError(t, err)
}
