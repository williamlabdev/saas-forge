package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Publish revisions and restore (ADR-018).
//
// The through-line of this file is the sentence the feature is built on: a
// PUBLISH writes history, and a RESTORE writes the working copy and NOTHING
// ELSE. Nearly every test below is an assertion that one of those two halves did
// not leak into the other — a restore that quietly republished, or a publish
// that quietly failed to record, would each be invisible in the response and
// visible only here.

// --- fixtures -----------------------------------------------------------------

// seedRevisionType is deliberately three ordinary fields rather than one: the
// drift and diff tests need a key that can be removed from the type without
// taking the required field with it.
func seedRevisionType(t *testing.T, svc ContentService, owner context.Context) {
	t.Helper()
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name:  "post",
		Label: "Post",
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "body", Type: domain.FieldTypeText},
			{Key: "legacy", Type: domain.FieldTypeString},
		},
	})
	require.NoError(t, err)
}

// publishNow puts the entry live and returns the version it left behind, so a
// caller can chain publishes without re-reading.
func publishNow(t *testing.T, svc ContentService, ctx context.Context, id uuid.UUID) EntryDTO {
	t.Helper()
	got, err := svc.SetEntryStatus(ctx, "post", id, domain.StatusPublished, 0)
	require.NoError(t, err)
	return got
}

func saveDraft(t *testing.T, svc ContentService, ctx context.Context, id uuid.UUID, payload map[string]any) EntryDTO {
	t.Helper()
	got, err := svc.UpdateEntry(ctx, "post", id, mustJSON(t, payload), 0)
	require.NoError(t, err)
	return got
}

// --- what a publish records ---------------------------------------------------

// HONEST SCOPE FOR THIS SECTION. The revision write lives in the REPOSITORY —
// recordPublishRevision, inside SetEntryPublishState's transaction — so at this
// layer the numbering and the retention window are produced by memRepo mirroring
// that SQL, not by the SQL itself. These tests therefore pin the CONTRACT the
// service and its callers rely on (one row per release, ascending from 1,
// nothing on a retract, a bounded window) and would keep passing if the real
// statement broke. The statement itself is pinned in the repository package's
// Docker integration tests, which is the only place it can honestly be checked.

// The core claim: one release, one row, numbered from 1 and ascending.
func TestPublishRecordsOneRevisionPerRelease(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedRevisionType(t, svc, owner)

	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "first"}))
	require.NoError(t, err)

	publishNow(t, svc, owner, e.ID)
	saveDraft(t, svc, owner, e.ID, map[string]any{"title": "second"})
	publishNow(t, svc, owner, e.ID)

	revs, err := svc.ListEntryRevisions(owner, "post", e.ID)
	require.NoError(t, err)
	require.Len(t, revs, 2, "two releases must leave two rows")

	// Newest first, and the ordinals are the countable numbers a person is
	// shown — not entry versions, which have moved further than this.
	assert.Equal(t, 2, revs[0].RevisionNo)
	assert.Equal(t, 1, revs[1].RevisionNo)

	// The list carries no payload at all. This is not a size optimisation: the
	// list is the surface an editor browses, and a payload here would put every
	// restricted field of every past release into a response that has done no
	// field-level masking.
	raw, err := json.Marshal(revs[0])
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "\"data\"",
		"the list projection leaked a payload")

	// Each revision holds the snapshot THAT release put live, not the newest one.
	first, err := svc.GetEntryRevision(owner, "post", e.ID, 1)
	require.NoError(t, err)
	assert.JSONEq(t, `{"title":"first"}`, string(first.Data))
	second, err := svc.GetEntryRevision(owner, "post", e.ID, 2)
	require.NoError(t, err)
	assert.JSONEq(t, `{"title":"second"}`, string(second.Data))
}

// A direct publish carries the empty via, which is the vocabulary's word for "a
// person pressed it" (000041). A schedule id here would be a claim of approval
// that never happened.
func TestDirectPublishRecordsNoScheduleProvenance(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedRevisionType(t, svc, owner)
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "hi"}))
	require.NoError(t, err)
	publishNow(t, svc, owner, e.ID)

	revs, err := svc.ListEntryRevisions(owner, "post", e.ID)
	require.NoError(t, err)
	require.Len(t, revs, 1)
	assert.Equal(t, "", revs[0].Via)
	assert.Nil(t, revs[0].ViaScheduleID)
	assert.NotNil(t, revs[0].PublishedBy, "the person who pressed it must be recorded")
}

// UNPUBLISH RECORDS NOTHING. ADR-014 §5.1 keeps the snapshot on a retract, so
// there is no new release to record; a row here would invent a release that
// never went out and would give restore an entry pointing at a document the
// public never saw under that number.
func TestUnpublishRecordsNoRevision(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedRevisionType(t, svc, owner)
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "hi"}))
	require.NoError(t, err)
	publishNow(t, svc, owner, e.ID)

	_, err = svc.SetEntryStatus(owner, "post", e.ID, domain.StatusDraft, 0)
	require.NoError(t, err)

	revs, err := svc.ListEntryRevisions(owner, "post", e.ID)
	require.NoError(t, err)
	assert.Len(t, revs, 1, "a retract wrote a revision row")
}

// Retention: the newest MaxPublishRevisionsPerEntry survive and the oldest are
// purged. Asserted on the boundary rather than on a round number, so an
// off-by-one in the purge predicate cannot pass.
func TestRetentionKeepsTheNewestAndPurgesTheOldest(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedRevisionType(t, svc, owner)
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "v0"}))
	require.NoError(t, err)

	total := domain.MaxPublishRevisionsPerEntry + 3
	for i := 1; i <= total; i++ {
		saveDraft(t, svc, owner, e.ID, map[string]any{"title": "v" + strings.Repeat("i", i)})
		publishNow(t, svc, owner, e.ID)
	}

	revs, err := svc.ListEntryRevisions(owner, "post", e.ID)
	require.NoError(t, err)
	require.Len(t, revs, domain.MaxPublishRevisionsPerEntry)
	assert.Equal(t, total, revs[0].RevisionNo, "the newest release must survive")
	assert.Equal(t, total-domain.MaxPublishRevisionsPerEntry+1, revs[len(revs)-1].RevisionNo,
		"the window kept the wrong end")

	// The purged ones are gone, not merely hidden by a read cap.
	_, err = svc.GetEntryRevision(owner, "post", e.ID, 1)
	requireCode(t, err, "CONTENT_REVISION_NOT_FOUND")
}

// --- restore ------------------------------------------------------------------

// The central test. Everything about the live snapshot must be exactly as it was.
func TestRestoreWritesTheWorkingCopyAndNothingElse(t *testing.T) {
	// A CAPTURING repository, not the plain fake, and the reason is a trap this
	// test fell into first: memRepo.UpdateEntry copies only the columns the real
	// UPDATE writes, so it DISCARDS any published_payload the service had set on
	// the entry. Asserting on the fake's stored row therefore passes even when
	// the service republishes — the fake, not the code, was holding the property
	// up. What the service actually did is the entry it handed to UpdateEntry,
	// so that is what is inspected.
	base := &memRepo{}
	repo := &captureUpdateRepo{memRepo: base}
	svc := NewContentService(repo, authz.NewAllowAllAuthorizer(), staticPlan(Quota{}))
	owner := ctxRole("t1", "owner")
	seedRevisionType(t, svc, owner)
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "february", "body": "old"}))
	require.NoError(t, err)
	publishNow(t, svc, owner, e.ID)
	saveDraft(t, svc, owner, e.ID, map[string]any{"title": "march", "body": "new"})
	live := publishNow(t, svc, owner, e.ID)

	before := storedEntry(t, base, e.ID)
	liveSnapshot := append(json.RawMessage(nil), before.PublishedPayload...)
	livePublishedAt := before.PublishedAt
	livePublishedBy := before.PublishedBy
	versionBefore := before.Version
	require.NotEmpty(t, liveSnapshot, "fixture sanity: there is no live snapshot to protect")

	got, err := svc.RestoreEntryRevision(owner, "post", e.ID, RestoreEntryInput{RevisionNo: 1}, 0)
	require.NoError(t, err)

	// The working copy IS the old release.
	assert.JSONEq(t, `{"title":"february","body":"old"}`, string(got.Entry.Data))
	assert.Equal(t, 1, got.RestoredFrom.RevisionNo)
	assert.Empty(t, got.RestoredFrom.DroppedKeys)

	after := storedEntry(t, base, e.ID)
	assert.JSONEq(t, `{"title":"february","body":"old"}`, string(after.Payload))

	// THE VERSION ADVANCED. Without this a restore would be invisible to the
	// optimistic lock and to ADR-017's stale-schedule rule.
	assert.Equal(t, versionBefore+1, after.Version, "restore did not advance the version")
	assert.True(t, after.HasUnpublishedChanges,
		"after a restore the draft differs from the live snapshot and the flag must say so")

	// AND THE LIVE SNAPSHOT DID NOT MOVE. This is the assertion that separates a
	// restore from a publish, and it is the one a regression would break first.
	// Read off the entry the service SUBMITTED, per the comment on the fixture.
	saved := repo.lastUpdate
	require.NotNil(t, saved, "restore never called UpdateEntry")
	assert.JSONEq(t, string(liveSnapshot), string(saved.PublishedPayload),
		"restore rewrote what the public sees")
	assert.Equal(t, domain.StatusPublished, saved.Status, "restore changed the entry's status")
	assert.Equal(t, livePublishedAt, saved.PublishedAt, "restore moved published_at")
	assert.Equal(t, livePublishedBy, saved.PublishedBy, "restore reassigned who answers for the live release")
	assert.Equal(t, "march", func() string {
		var doc map[string]any
		require.NoError(t, json.Unmarshal(live.Data, &doc))
		return doc["title"].(string)
	}(), "fixture sanity: the second release was the one live before the restore")
}

// A restore is its own verb in the stream — neither an update nor a publish —
// and it carries the two facts that make the line auditable on its own.
func TestRestoreRecordsItsOwnActivityWithProvenance(t *testing.T) {
	svc, repo := newSvc()
	owner := ctxRole("t1", "owner")
	seedRevisionType(t, svc, owner)
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "february"}))
	require.NoError(t, err)
	publishNow(t, svc, owner, e.ID)
	saveDraft(t, svc, owner, e.ID, map[string]any{"title": "march"})

	_, err = svc.RestoreEntryRevision(owner, "post", e.ID, RestoreEntryInput{RevisionNo: 1}, 0)
	require.NoError(t, err)

	row := lastRow(t, repo)
	require.Equal(t, domain.ActivityEntryRestore, row.Action,
		"a restore must not be filed as an ordinary update")
	require.NotEmpty(t, row.Details, "the restore line has no provenance at all")
	var d struct {
		RevisionNo  int      `json:"revision_no"`
		PublishedAt string   `json:"published_at"`
		DroppedKeys []string `json:"dropped_keys"`
	}
	require.NoError(t, json.Unmarshal(row.Details, &d))
	assert.Equal(t, 1, d.RevisionNo)
	assert.NotEmpty(t, d.PublishedAt, "the source release's timestamp is the audit trail")
	assert.NotNil(t, d.DroppedKeys, "dropped_keys must serialise as [] rather than null")
	assert.Contains(t, row.ChangedKeys, "title")
}

// SCHEMA DRIFT. A field deleted from the type since the release must not make an
// entire era unrestorable, and must not vanish silently either.
func TestRestoreDropsKeysTheTypeNoLongerDefines(t *testing.T) {
	svc, repo := newSvc()
	owner := ctxRole("t1", "owner")
	seedRevisionType(t, svc, owner)
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "old", "legacy": "gone soon"}))
	require.NoError(t, err)
	publishNow(t, svc, owner, e.ID)
	saveDraft(t, svc, owner, e.ID, map[string]any{"title": "new", "legacy": "still here"})

	_, err = svc.DeleteField(owner, "post", "legacy", true)
	require.NoError(t, err)

	got, err := svc.RestoreEntryRevision(owner, "post", e.ID, RestoreEntryInput{RevisionNo: 1}, 0)
	require.NoError(t, err, "a removed field made an old release unrestorable")
	assert.Equal(t, []string{"legacy"}, got.RestoredFrom.DroppedKeys,
		"the dropped key must be named on the response")
	assert.NotContains(t, string(got.Entry.Data), "legacy",
		"a key the type no longer defines was written back into the document")

	row := lastRow(t, repo)
	assert.Contains(t, string(row.Details), "legacy",
		"the audit line must name what the restore silently discarded")
}

// FULL REPLACE, not PATCH merge. A key added to the draft after the release must
// be GONE after restoring, or the result is a third document nobody reviewed.
func TestRestoreIsAFullReplaceNotAMerge(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedRevisionType(t, svc, owner)
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "old"}))
	require.NoError(t, err)
	publishNow(t, svc, owner, e.ID)
	saveDraft(t, svc, owner, e.ID, map[string]any{"title": "old", "body": "added later"})

	got, err := svc.RestoreEntryRevision(owner, "post", e.ID, RestoreEntryInput{RevisionNo: 1}, 0)
	require.NoError(t, err)
	assert.JSONEq(t, `{"title":"old"}`, string(got.Entry.Data),
		"restore merged instead of replacing; the later key survived")
}

// The optimistic lock. If-Match names a version, and a document that has moved
// underneath the editor must not be silently overwritten by a restore any more
// than by an ordinary save.
func TestRestoreHonoursIfMatch(t *testing.T) {
	svc, repo := newSvc()
	owner := ctxRole("t1", "owner")
	seedRevisionType(t, svc, owner)
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "old"}))
	require.NoError(t, err)
	publishNow(t, svc, owner, e.ID)
	current := saveDraft(t, svc, owner, e.ID, map[string]any{"title": "new"})

	_, err = svc.RestoreEntryRevision(owner, "post", e.ID, RestoreEntryInput{RevisionNo: 1}, current.Version-1)
	assert.ErrorIs(t, err, repository.ErrVersionConflict)
	assert.JSONEq(t, `{"title":"new"}`, string(storedEntry(t, repo, e.ID).Payload),
		"a refused restore still rewrote the working copy")

	_, err = svc.RestoreEntryRevision(owner, "post", e.ID, RestoreEntryInput{RevisionNo: 1}, current.Version)
	require.NoError(t, err, "the matching version was refused")
}

// A restore that would collide with another entry's unique value must fail like
// any other save — and must leave the working copy exactly as it found it.
//
// The collision is injected at the repository seam because the in-memory fake
// does not model entry_unique_values; the REAL 23505 path is covered by
// TestPublishRevisionRestoreConflictLeavesWorkingCopy in the repository package.
// What is under test here is the service's behaviour when the write is refused:
// no partial state, no activity claiming success.
func TestRestoreLeavesTheWorkingCopyAloneWhenTheWriteIsRefused(t *testing.T) {
	base := &memRepo{}
	repo := &refuseUpdateRepo{memRepo: base}
	svc := NewContentService(repo, authz.NewAllowAllAuthorizer(), staticPlan(Quota{}))
	owner := ctxRole("t1", "owner")
	seedRevisionType(t, svc, owner)
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "old"}))
	require.NoError(t, err)
	publishNow(t, svc, owner, e.ID)
	saveDraft(t, svc, owner, e.ID, map[string]any{"title": "new"})

	repo.fail = errValueTakenForTest()
	_, err = svc.RestoreEntryRevision(owner, "post", e.ID, RestoreEntryInput{RevisionNo: 1}, 0)
	requireCodeStatus(t, err, "CONTENT_FIELD_VALUE_TAKEN", 409)
	assert.JSONEq(t, `{"title":"new"}`, string(storedEntry(t, base, e.ID).Payload),
		"the working copy moved even though the write was refused")
}

// --- refusals -----------------------------------------------------------------

func TestRevisionEndpointsRefuseAnUnknownOrdinal(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedRevisionType(t, svc, owner)
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "hi"}))
	require.NoError(t, err)
	publishNow(t, svc, owner, e.ID)

	_, err = svc.GetEntryRevision(owner, "post", e.ID, 99)
	requireCodeStatus(t, err, "CONTENT_REVISION_NOT_FOUND", 404)

	_, err = svc.DiffEntryRevision(owner, "post", e.ID, 99)
	requireCode(t, err, "CONTENT_REVISION_NOT_FOUND")

	_, err = svc.RestoreEntryRevision(owner, "post", e.ID, RestoreEntryInput{RevisionNo: 99}, 0)
	requireCode(t, err, "CONTENT_REVISION_NOT_FOUND")

	// Zero and negative are a malformed request, not a missing row: answering
	// 404 would tell a caller who typed nonsense to go looking for the record.
	_, err = svc.RestoreEntryRevision(owner, "post", e.ID, RestoreEntryInput{}, 0)
	requireCodeStatus(t, err, "CONTENT_REVISION_NO_INVALID", 400)
}

// THE AUDIENCE WALL. Delivery and preview are the two credentials that reach the
// content service from outside the console, and neither may see that history
// exists at all — a preview token is handed to a reviewer, and past releases are
// not what they were given a link to.
func TestRevisionEndpointsRefuseDeliveryAndPreview(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedRevisionType(t, svc, owner)
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "hi"}))
	require.NoError(t, err)
	publishNow(t, svc, owner, e.ID)

	for name, ctx := range map[string]context.Context{
		"delivery": ctxDelivery("t1"),
		"preview":  ctxPreview("t1", e.ID),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.ListEntryRevisions(ctx, "post", e.ID)
			assert.ErrorIs(t, err, apperrors.ErrForbidden, "list")
			_, err = svc.GetEntryRevision(ctx, "post", e.ID, 1)
			assert.ErrorIs(t, err, apperrors.ErrForbidden, "get")
			_, err = svc.DiffEntryRevision(ctx, "post", e.ID, 1)
			assert.ErrorIs(t, err, apperrors.ErrForbidden, "diff")
			_, err = svc.RestoreEntryRevision(ctx, "post", e.ID, RestoreEntryInput{RevisionNo: 1}, 0)
			assert.ErrorIs(t, err, apperrors.ErrForbidden, "restore")
		})
	}
}

// The DTOs the public and a preview reviewer actually receive must carry no
// trace of the feature. Asserted on the SERIALISED bytes rather than the struct,
// because EntryDTO.MarshalJSON is where the audience projection is finally
// applied.
func TestDeliveryAndPreviewDTOsCarryNoRevisionTrace(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedRevisionType(t, svc, owner)
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "hi"}))
	require.NoError(t, err)
	publishNow(t, svc, owner, e.ID)

	for name, ctx := range map[string]context.Context{
		"delivery": ctxDelivery("t1"),
		"preview":  ctxPreview("t1", e.ID),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := svc.GetEntry(ctx, "post", e.ID, GetEntryInput{})
			require.NoError(t, err)
			raw, err := json.Marshal(got)
			require.NoError(t, err)
			assert.NotContains(t, strings.ToLower(string(raw)), "revision",
				"the public projection mentions revisions")
		})
	}
}

// ADR-013 §4/§5 refuse the agent by tool list, but the tool list is UX, not
// authorization: an agent credential holds content:read and content:update
// outright, so the refusal has to exist at the service. Without it an agent
// could reconstruct and replay a document a person approved for a different day.
func TestRevisionEndpointsRefuseAgentCredentials(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedRevisionType(t, svc, owner)
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "hi"}))
	require.NoError(t, err)
	publishNow(t, svc, owner, e.ID)

	agent := ctxAgent("t1", "editor", uuid.New(), []string{"post"})
	_, err = svc.ListEntryRevisions(agent, "post", e.ID)
	requireCode(t, err, "CONTENT_REVISION_AGENT_FORBIDDEN")
	_, err = svc.GetEntryRevision(agent, "post", e.ID, 1)
	requireCode(t, err, "CONTENT_REVISION_AGENT_FORBIDDEN")
	_, err = svc.DiffEntryRevision(agent, "post", e.ID, 1)
	requireCode(t, err, "CONTENT_REVISION_AGENT_FORBIDDEN")
	_, err = svc.RestoreEntryRevision(agent, "post", e.ID, RestoreEntryInput{RevisionNo: 1}, 0)
	requireCode(t, err, "CONTENT_REVISION_AGENT_FORBIDDEN")

	// The control: the same agent may still write the working copy the ordinary
	// way, so the refusals above are about this feature and not about the
	// credential being refused everything.
	_, err = svc.UpdateEntry(agent, "post", e.ID, mustJSON(t, map[string]any{"title": "agent edit"}), 0)
	require.NoError(t, err)
}

// A viewer holds content:read but not content:update. Reading history is part of
// looking at an entry; replaying it is editing.
func TestRestoreRequiresContentUpdate(t *testing.T) {
	repo := &memRepo{}
	svc := NewContentService(repo, authz.NewRBACAuthorizer(), staticPlan(Quota{}))
	owner := ctxRole("t1", "owner")
	seedRevisionType(t, svc, owner)
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "hi"}))
	require.NoError(t, err)
	publishNow(t, svc, owner, e.ID)

	viewer := ctxRole("t1", "viewer")
	_, err = svc.ListEntryRevisions(viewer, "post", e.ID)
	require.NoError(t, err, "a viewer may read an entry, so it may read that entry's releases")

	_, err = svc.RestoreEntryRevision(viewer, "post", e.ID, RestoreEntryInput{RevisionNo: 1}, 0)
	assert.ErrorIs(t, err, apperrors.ErrForbidden, "a read-only role restored an entry")
}

// FIELD PERMISSION, the ruling ADR-014 §5 left open. Replaying a restricted
// field's IDENTICAL value is not a write to it; MOVING it still is.
func TestRestoreGuardsOnlyTheRestrictedFieldsItWouldActuallyChange(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	id := seedEmployee(t, svc, owner)
	_, err := svc.SetEntryStatus(owner, "employee", id, domain.StatusPublished, 0)
	require.NoError(t, err)

	// An admin may READ salary but not write it. Moving only `name` leaves
	// salary untouched, so the restore must go through.
	_, err = svc.UpdateEntry(owner, "employee", id, mustJSON(t, map[string]any{"name": "Grace"}), 0)
	require.NoError(t, err)

	admin := ctxRole("t1", "admin")
	got, err := svc.RestoreEntryRevision(admin, "employee", id, RestoreEntryInput{RevisionNo: 1}, 0)
	require.NoError(t, err,
		"a restore that does not move the restricted value was refused; ADR-014 §5's false positive is back")
	assert.Contains(t, string(got.Entry.Data), "Ada")

	// Now the value itself moves. Same caller, same endpoint, and this one must
	// be refused — otherwise restore is the bypass ADR-009 forbids.
	_, err = svc.UpdateEntry(owner, "employee", id, mustJSON(t, map[string]any{"salary": 200000}), 0)
	require.NoError(t, err)
	_, err = svc.SetEntryStatus(owner, "employee", id, domain.StatusPublished, 0)
	require.NoError(t, err)
	_, err = svc.UpdateEntry(owner, "employee", id, mustJSON(t, map[string]any{"salary": 300000}), 0)
	require.NoError(t, err)

	_, err = svc.RestoreEntryRevision(admin, "employee", id, RestoreEntryInput{RevisionNo: 2}, 0)
	require.Error(t, err, "restore moved a restricted value for a caller who may not write it")
	ae, ok := apperrors.As(err)
	require.True(t, ok, "expected an application error, got %v", err)
	assert.Equal(t, 403, ae.HTTPStatus, "a field-permission refusal must be a 403")
}

// The other direction of the same rule, and the one keepKeys structurally cannot
// express: restoring a revision from BEFORE a restricted field existed does not
// send that field at all, it DELETES it. If only the present keys were guarded,
// "wipe a value you may not write" would be reachable by anyone holding
// content:update — the bypass §4 of ADR-018 says does not exist.
func TestRestoreRefusesToDeleteARestrictedFieldTheCallerCannotWrite(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, salaryTypeInput())
	require.NoError(t, err)

	// Revision 1 predates the salary entirely. salary is optional, so this is an
	// ordinary document and not a contrived one.
	e, err := svc.CreateEntry(owner, "employee", mustJSON(t, map[string]any{"name": "Ada"}))
	require.NoError(t, err)
	_, err = svc.SetEntryStatus(owner, "employee", e.ID, domain.StatusPublished, 0)
	require.NoError(t, err)

	// Revision 2 has it.
	_, err = svc.UpdateEntry(owner, "employee", e.ID, mustJSON(t, map[string]any{"salary": 100000}), 0)
	require.NoError(t, err)
	_, err = svc.SetEntryStatus(owner, "employee", e.ID, domain.StatusPublished, 0)
	require.NoError(t, err)
	// A later edit that leaves salary alone, so the working copy and revision 2
	// agree on it and the control below is a genuine no-op for that field.
	_, err = svc.UpdateEntry(owner, "employee", e.ID, mustJSON(t, map[string]any{"name": "Grace"}), 0)
	require.NoError(t, err)

	// admin may READ salary but not write it. Restoring revision 1 would remove
	// the value — a write, and one this caller may not make.
	admin := ctxRole("t1", "admin")
	_, err = svc.RestoreEntryRevision(admin, "employee", e.ID, RestoreEntryInput{RevisionNo: 1}, 0)
	require.Error(t, err, "restore deleted a restricted field for a caller who may not write it")
	assert.Equal(t, "CONTENT_FIELD_WRITE_FORBIDDEN", codeOf(t, err))
	ae, ok := apperrors.As(err)
	require.True(t, ok, "expected an application error, got %v", err)
	assert.Equal(t, 403, ae.HTTPStatus)

	// And the refusal stored nothing: a 403 that had already written is a
	// permission check that only affects the response.
	after, err := svc.GetEntry(owner, "employee", e.ID, GetEntryInput{})
	require.NoError(t, err)
	assert.Contains(t, string(after.Data), "salary",
		"the refused restore removed the field anyway")

	// Control, same caller and same endpoint: revision 2 carries the SAME salary
	// the working copy already has, so nothing restricted moves and it must go
	// through. Without this half the test above would also pass against a
	// service that simply refuses every restore touching a restricted type.
	got, err := svc.RestoreEntryRevision(admin, "employee", e.ID, RestoreEntryInput{RevisionNo: 2}, 0)
	require.NoError(t, err,
		"a restore that leaves the restricted value where it is was refused; ADR-014 §5's false positive is back")
	assert.Contains(t, string(got.Entry.Data), "Ada")
}

// --- diff ---------------------------------------------------------------------

func TestDiffComparesTheRevisionAgainstTheWorkingCopy(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedRevisionType(t, svc, owner)
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "old", "body": "same"}))
	require.NoError(t, err)
	publishNow(t, svc, owner, e.ID)
	// The draft moves WITHOUT a second publish, which is the case the diff
	// exists for: "what would restoring undo".
	saveDraft(t, svc, owner, e.ID, map[string]any{"title": "new", "body": "same"})

	got, err := svc.DiffEntryRevision(owner, "post", e.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, []string{"title"}, got.ChangedKeys,
		"the diff must compare against the WORKING copy, not the live snapshot")
	assert.JSONEq(t, `{"title":"old","body":"same"}`, string(got.RevisionData))
	assert.JSONEq(t, `{"title":"new","body":"same"}`, string(got.Data))
	assert.False(t, got.HasHiddenChanges)
}

// A restricted field must not reach a caller who cannot read it, and its
// movement must still be reported as a fact so the editor is not told the
// documents are identical when they are not.
func TestDiffMasksUnreadableFieldsButAdmitsTheyMoved(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	id := seedEmployee(t, svc, owner)
	_, err := svc.SetEntryStatus(owner, "employee", id, domain.StatusPublished, 0)
	require.NoError(t, err)
	_, err = svc.UpdateEntry(owner, "employee", id, mustJSON(t, map[string]any{"salary": 999999}), 0)
	require.NoError(t, err)

	editor := ctxRole("t1", "editor") // may read neither salary
	got, err := svc.DiffEntryRevision(editor, "employee", id, 1)
	require.NoError(t, err)
	assert.NotContains(t, string(got.RevisionData), "salary", "the old salary leaked through the diff")
	assert.NotContains(t, string(got.Data), "salary", "the current salary leaked through the diff")
	assert.NotContains(t, got.ChangedKeys, "salary")
	assert.True(t, got.HasHiddenChanges,
		"a masked field moved and the diff claimed the documents were identical")
}

// --- test doubles -------------------------------------------------------------

// refuseUpdateRepo wraps the in-memory fake and refuses UpdateEntry with a
// preset error, standing in for a constraint the fake does not model. It
// delegates everything else so the entry, the type and the revisions are all
// real as far as the service is concerned.
// captureUpdateRepo keeps the entry the service handed to UpdateEntry. See the
// comment at the top of TestRestoreWritesTheWorkingCopyAndNothingElse for why
// the fake's stored row is not a sufficient witness for the publish-state
// columns: memRepo mirrors the real UPDATE and therefore ignores them, which
// makes "the snapshot did not move" true of the fake no matter what the service
// did.
type captureUpdateRepo struct {
	*memRepo
	lastUpdate *domain.Entry
}

func (r *captureUpdateRepo) UpdateEntry(ctx context.Context, e *domain.Entry) error {
	// Copied, not aliased: the service keeps writing to the same pointer after
	// this returns (ProjectEntry reads it), and a test that held the live
	// pointer would be asserting on a later state than the one submitted.
	snapshot := *e
	r.lastUpdate = &snapshot
	return r.memRepo.UpdateEntry(ctx, e)
}

var _ repository.ContentRepository = (*captureUpdateRepo)(nil)

type refuseUpdateRepo struct {
	*memRepo
	fail error
}

func (r *refuseUpdateRepo) UpdateEntry(ctx context.Context, e *domain.Entry) error {
	if r.fail != nil {
		return r.fail
	}
	return r.memRepo.UpdateEntry(ctx, e)
}

var _ repository.ContentRepository = (*refuseUpdateRepo)(nil)

func errValueTakenForTest() error {
	return apperrors.New("CONTENT_FIELD_VALUE_TAKEN", "value is already used by another entry", 409)
}
