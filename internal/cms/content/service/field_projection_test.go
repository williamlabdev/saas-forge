package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
)

// ADR-013 §7: ?fields= narrows each entry's payload to the keys the caller
// asked for, so an agent that needs three keys out of forty does not pay for
// forty on every row.
//
// Every assertion here reads the MARSHALLED bytes. The narrowing happens in
// MarshalJSON beside the permission strip, so a test that looked at dto.Data
// would pass whatever the gate did — the same trap the field-permission tests
// record.

// projectionType has three payload keys so "kept" and "dropped" are both
// non-empty sets; a two-key type makes a projection of one indistinguishable
// from several off-by-one bugs.
func projectionTypeInput() CreateTypeInput {
	return CreateTypeInput{
		Name:  "note",
		Label: "Note",
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "body", Type: domain.FieldTypeText},
			{Key: "author", Type: domain.FieldTypeString},
		},
	}
}

func seedNote(t *testing.T, svc ContentService, owner context.Context) {
	t.Helper()
	_, err := svc.CreateContentType(owner, projectionTypeInput())
	require.NoError(t, err)
	_, err = svc.CreateEntry(owner, "note", mustJSON(t, map[string]any{
		"title": "T", "body": "B", "author": "A",
	}))
	require.NoError(t, err)
}

func payloadOf(t *testing.T, dto EntryDTO) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(mustMarshalData(t, dto), &out))
	return out
}

func TestProjectionKeepsOnlyTheRequestedKeys(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedNote(t, svc, owner)

	res, err := svc.ListEntries(owner, "note", ListEntriesInput{Fields: []string{"title", "author"}})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)

	payload := payloadOf(t, res.Items[0])
	assert.Equal(t, map[string]any{"title": "T", "author": "A"}, payload,
		"the payload must be exactly the requested keys — written out, not derived from the request")

	// The envelope is NOT part of the projection. A caller that could not tell
	// the rows apart afterwards would have to ask for the ids back, which costs
	// more than the projection saved.
	assert.NotZero(t, res.Items[0].ID)
	assert.Equal(t, "note", res.Items[0].Type)
	assert.Equal(t, domain.StatusDraft, res.Items[0].Status)

	// The control: without the parameter the whole payload still comes back, so
	// the assertion above is about the projection and not about a lossy path.
	full, err := svc.ListEntries(owner, "note", ListEntriesInput{})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"title": "T", "body": "B", "author": "A"}, payloadOf(t, full.Items[0]))
}

// The delivery audience is a SECOND call site inside ListEntries, and it is the
// one an agent-adjacent integration is most likely to use. A projection honoured
// only for admins would be a silently ignored parameter — the failure mode this
// codebase refuses everywhere else (cursor, status, offset).
func TestProjectionAppliesToTheDeliveryAudienceToo(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedNote(t, svc, owner)

	listed, err := svc.ListEntries(owner, "note", ListEntriesInput{})
	require.NoError(t, err)
	_, err = svc.SetEntryStatus(owner, "note", listed.Items[0].ID, domain.StatusPublished, 0)
	require.NoError(t, err)

	res, err := svc.ListEntries(ctxDelivery("t1"), "note", ListEntriesInput{Fields: []string{"title"}})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.Equal(t, map[string]any{"title": "T"}, payloadOf(t, res.Items[0]),
		"the published snapshot narrows the same way the working copy does")
}

func TestProjectionRefusesAnUndefinedField(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedNote(t, svc, owner)

	_, err := svc.ListEntries(owner, "note", ListEntriesInput{Fields: []string{"title", "nope"}})
	assert.Equal(t, "CONTENT_FIELDS_FIELD_UNKNOWN", codeOf(t, err),
		"a typo'd key must not come back as an entry that merely lacks that value")
	assert.Equal(t, "nope", detailsOf(t, err)["field"])
}

// An unreadable field is refused, not dropped. Dropping it would leave the
// caller unable to tell "you may not read that" from "this entry has no value
// there" — and they would conclude the latter about every row in the tenant.
func TestProjectionRefusesAnUnreadableField(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedEmployee(t, svc, owner)

	editor := ctxRole("t1", "editor")
	_, err := svc.ListEntries(editor, "employee", ListEntriesInput{Fields: []string{"salary"}})
	assert.Equal(t, "CONTENT_FIELD_QUERY_FORBIDDEN", codeOf(t, err))
	assert.Equal(t, "fields", detailsOf(t, err)["clause"],
		"the refusal must name WHICH clause was refused — filter and sort already do")

	// The readable field on the same type still projects, so the refusal above
	// is the field gate and not the projection failing for everyone.
	ok, err := svc.ListEntries(editor, "employee", ListEntriesInput{Fields: []string{"name"}})
	require.NoError(t, err)
	require.Len(t, ok.Items, 1)
	assert.Equal(t, map[string]any{"name": "Ada"}, payloadOf(t, ok.Items[0]))
}

// A blank selection is how a URL builder spells "no selection". Answering it
// with an empty payload would be technically defensible and useless.
func TestBlankProjectionMeansTheWholePayload(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedNote(t, svc, owner)

	for _, fields := range [][]string{nil, {}, {""}, {"  ", ""}} {
		res, err := svc.ListEntries(owner, "note", ListEntriesInput{Fields: fields})
		require.NoError(t, err, "%q", fields)
		require.Len(t, res.Items, 1)
		assert.Equal(t, map[string]any{"title": "T", "body": "B", "author": "A"},
			payloadOf(t, res.Items[0]), "%q must mean the whole payload", fields)
	}
}

// WHITEBOX: a projection asking for a field the reader may not see gets neither
// the value nor an exemption.
//
// parseProjection already refuses that request with a 403, so no request can
// reach MarshalJSON in this state and no black-box test can produce one. The DTO
// below skips the parser, exactly as a later read path that narrows a payload
// its own way would.
//
// What it does NOT prove is an ordering. Both narrowings only remove keys, so
// they commute — the guarantee is that composing them cannot ADD one back, and
// the mutation this catches is the plausible shortcut of skipping the permission
// strip because the payload was "already narrowed".
func TestProjectionCannotResurrectAnUnreadableField(t *testing.T) {
	svc, repo := newSvc()
	owner := ctxRole("t1", "owner")
	id := seedEmployee(t, svc, owner)

	var stored *domain.Entry
	for _, e := range repo.entries {
		if e.ID == id {
			stored = e
		}
	}
	require.NotNil(t, stored)

	ct, err := repo.GetContentTypeByName(context.Background(), "t1", "employee")
	require.NoError(t, err)

	editorSub, ok := authn.SubjectFromContext(ctxRole("t1", "editor"))
	require.True(t, ok)
	dto := ProjectEntry(ct, stored, editorSub).narrowedTo([]string{"salary", "name"})

	payload := payloadOf(t, dto)
	assert.NotContains(t, payload, "salary",
		"the permission strip must run after the caller's narrowing, whatever asked for the narrowing")
	assert.Contains(t, payload, "name", "or this test would pass on an empty payload")
}
