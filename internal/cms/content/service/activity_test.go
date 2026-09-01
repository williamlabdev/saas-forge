package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
)

// ADR-014 §3 — the activity record, at the service layer.
//
// The HTTP half of 驗證計畫第 3 條 lives in test/e2e/agent_activity_test.go, for
// the reason ADR-013 §4's tests give: what a caller can reach by talking to the
// API is a different claim from what the service does when called directly.

// --- helpers ------------------------------------------------------------------

// agentRows / rowsFor read the fake's stream. They filter rather than assert on
// the whole slice because most of these tests seed with an owner, and the seed's
// own activity is real activity — suppressing it in the fixture would be the
// fixture deciding what counts.
func rowsFor(repo *memRepo, kind string) []*domain.Activity {
	var out []*domain.Activity
	for _, a := range repo.activity {
		if a.ActorKind == kind {
			out = append(out, a)
		}
	}
	return out
}

func actionsOf(rows []*domain.Activity) []string {
	out := make([]string, 0, len(rows))
	for _, a := range rows {
		out = append(out, a.Action)
	}
	return out
}

// lastRow returns the most recent row, failing rather than returning a zero
// value: a test that reads a zero Activity asserts against fields that were
// never written and passes for the wrong reason.
func lastRow(t *testing.T, repo *memRepo) *domain.Activity {
	t.Helper()
	require.NotEmpty(t, repo.activity, "expected at least one activity row")
	return repo.activity[len(repo.activity)-1]
}

func seedTitledType(t *testing.T, svc ContentService, owner context.Context, name string) {
	t.Helper()
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name:  name,
		Label: name,
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "body", Type: domain.FieldTypeText},
		},
	})
	require.NoError(t, err)
}

// --- 驗證計畫第 6 條: the tool surface is the yardstick -------------------------

// agentTools is ADR-013 §5's table, transcribed by hand.
//
// IT IS NOT DERIVED FROM domain.AllActivityActions(), and that is the whole
// point of this test: a yardstick taken from the activity vocabulary would be
// the log certifying its own completeness. Adding a tool to ADR-013 §5 means
// adding a line here, and a line here that no service call satisfies is a red
// test — which is the dominating rule ("anything an agent can do, the console
// must be able to say afterwards what it did") made enforceable rather than
// aspirational.
var agentTools = []struct {
	tool   string
	action string
}{
	{"cms_describe", domain.ActivityTypeRead},
	{"cms_list_entries", domain.ActivityEntryList},
	{"cms_get_entry", domain.ActivityEntryRead},
	{"cms_create_entry", domain.ActivityEntryCreate},
	{"cms_update_entry", domain.ActivityEntryUpdate},
	{"cms_set_status", domain.ActivityEntryUnpublish},
	{"cms_list_translations", domain.ActivityEntryTranslations},
	{"cms_plan_schema", domain.ActivitySchemaPlan},
	{"cms_propose_schema", domain.ActivitySchemaPropose},
}

// TestEveryAgentToolLandsInTheActivityStream drives each tool's underlying
// service call with an agent credential and requires the matching action to
// appear.
//
// Behavioural, not a constant check. Asserting only that the constant EXISTS
// would pass on a vocabulary nobody emits — the same vacuum ADR-013's parity
// work ran into, where the expected value has to come from actually making the
// call rather than from asking the code under test what it supports.
func TestEveryAgentToolLandsInTheActivityStream(t *testing.T) {
	principal := uuid.New()

	for _, tc := range agentTools {
		t.Run(tc.tool, func(t *testing.T) {
			svc, repo := newSvc()
			owner := ctxRole("t1", "owner")
			seedTitledType(t, svc, owner, "post")
			seeded, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "seed"}))
			require.NoError(t, err)
			agent := ctxAgent("t1", "editor", principal, []string{"post"})

			switch tc.tool {
			case "cms_describe":
				_, err = svc.GetContentType(agent, "post")
			case "cms_list_entries":
				_, err = svc.ListEntries(agent, "post", ListEntriesInput{})
			case "cms_get_entry":
				_, err = svc.GetEntry(agent, "post", seeded.ID)
			case "cms_create_entry":
				_, err = svc.CreateEntry(agent, "post", mustJSON(t, map[string]any{"title": "made"}))
			case "cms_update_entry":
				_, err = svc.UpdateEntry(agent, "post", seeded.ID, mustJSON(t, map[string]any{"title": "edited"}), 0)
			case "cms_set_status":
				// Published first, by the person §1 says has to press it, so the
				// agent's unpublish is a real retraction rather than the no-op
				// SetEntryStatus short-circuits.
				_, perr := svc.SetEntryStatus(owner, "post", seeded.ID, domain.StatusPublished, 0)
				require.NoError(t, perr)
				_, err = svc.SetEntryStatus(agent, "post", seeded.ID, domain.StatusDraft, 0)
			case "cms_list_translations":
				_, err = svc.ListTranslations(agent, "post", seeded.ID)
			case "cms_plan_schema":
				art, xerr := svc.ExportSchema(owner)
				require.NoError(t, xerr)
				_, err = svc.PlanSchema(agent, art, false)
			case "cms_propose_schema":
				// The artifact is exported by the OWNER and proposed by the
				// agent, which is the real shape of step 8: the document names
				// types the agent may write, and filing it is the agent's half
				// of a change only a person can approve.
				art, xerr := svc.ExportSchema(owner)
				require.NoError(t, xerr)
				_, err = svc.ProposeSchema(agent, art, false)
			default:
				t.Fatalf("no service call wired for %s", tc.tool)
			}
			require.NoError(t, err, "the tool's underlying call must succeed, or the missing row proves nothing")

			assert.Contains(t, actionsOf(rowsFor(repo, domain.ActorKindAgent)), tc.action,
				"%s ran and the stream cannot say what it did", tc.tool)
		})
	}
}

// --- 驗證計畫第 3 條: refusals are rows -----------------------------------------

// TestRefusedAgentActionLandsInTheStream is the §3 requirement stated as a test:
// the 403 went to the agent, and the person answerable for it can see that it
// happened.
//
// The CONTROL GROUP is not optional. Without it a service that recorded
// absolutely nothing would pass the first half — "no row for the refusal" and
// "no row at all" are the same observation.
func TestRefusedAgentActionLandsInTheStream(t *testing.T) {
	svc, repo := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")
	seedTitledType(t, svc, owner, "invoice")

	principal := uuid.New()
	agent := ctxAgent("t1", "editor", principal, []string{"post"})

	_, err := svc.CreateEntry(agent, "invoice", mustJSON(t, map[string]any{"title": "x"}))
	require.Error(t, err)
	// Written out literally, not read back from the error: the point is that
	// the stream carries the code a reader can act on.
	require.Equal(t, "CONTENT_AGENT_TYPE_NOT_ALLOWED", codeOf(t, err))

	refused := lastRow(t, repo)
	assert.Equal(t, domain.ActivityOutcomeDenied, refused.Outcome)
	assert.Equal(t, "CONTENT_AGENT_TYPE_NOT_ALLOWED", refused.ErrorCode)
	assert.Equal(t, domain.ActivityEntryCreate, refused.Action)
	assert.Equal(t, "invoice", refused.TargetType)
	assert.Equal(t, domain.ActorKindAgent, refused.ActorKind)
	assert.Equal(t, &principal, refused.ActorUserID, "signature and accountability together (§4)")
	require.NotNil(t, refused.ActorAgentID)
	assert.Equal(t, "content-bot", *refused.ActorAgentID)

	// Control: the same action, permitted, is also a row — and says so.
	_, err = svc.CreateEntry(owner, "invoice", mustJSON(t, map[string]any{"title": "ok"}))
	require.NoError(t, err)
	allowed := lastRow(t, repo)
	assert.Equal(t, domain.ActivityOutcomeSuccess, allowed.Outcome)
	assert.Empty(t, allowed.ErrorCode)
	assert.Equal(t, domain.ActivityEntryCreate, allowed.Action)
	assert.Equal(t, domain.ActorKindHuman, allowed.ActorKind)
}

// racingRepo simulates a concurrent write landing between this caller's read
// and its write: it bumps the stored version just before delegating, so the
// fake's own optimistic lock produces the 409 exactly as Postgres would.
//
// A hand-injected error would have been shorter and would have tested the error
// plumbing instead of the race. This reaches the ONE refusal in UpdateEntry that
// happens after the method has already decided what it was going to change,
// which is the only place the "a refusal changed nothing" clearing can matter.
type racingRepo struct {
	*memRepo
	raced bool
}

func (r *racingRepo) UpdateEntry(ctx context.Context, e *domain.Entry) error {
	if !r.raced {
		r.raced = true
		for _, it := range r.entries {
			if it.ID == e.ID {
				it.Version++
			}
		}
	}
	return r.memRepo.UpdateEntry(ctx, e)
}

// TestDeniedRowCarriesNoChangedKeys pins the invariant migration 000032 also
// spells: a refusal changed nothing, so it may not claim to have.
//
// The version check the SERVICE runs is not the one that gets here — it fires
// before the merge, when there are no keys to clear yet, so a test built on it
// passes whether or not the clearing exists (it did, until the mutation run
// caught it). The refusal that matters is the repository's optimistic lock,
// which a caller sending no If-Match reaches whenever someone else saved first.
func TestDeniedRowCarriesNoChangedKeys(t *testing.T) {
	mem := &memRepo{}
	repo := &racingRepo{memRepo: mem}
	svc := NewContentService(repo, authz.NewAllowAllAuthorizer(), staticPlan(Quota{}))
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "a"}))
	require.NoError(t, err)

	// No expected version, so the service's own check is skipped and the write
	// travels all the way to the repository — which by then has moved on.
	_, err = svc.UpdateEntry(owner, "post", e.ID, mustJSON(t, map[string]any{"title": "b"}), 0)
	require.Error(t, err)
	require.Equal(t, "CONTENT_VERSION_CONFLICT", codeOf(t, err),
		"the refusal must be the repository's optimistic lock, or this reaches the wrong branch")

	row := lastRow(t, mem)
	require.Equal(t, domain.ActivityEntryUpdate, row.Action)
	require.Equal(t, domain.ActivityOutcomeDenied, row.Outcome)
	assert.Empty(t, row.ChangedKeys, "a refusal changed nothing")
}

// --- who is recorded, and who is not -------------------------------------------

// TestHumanReadsAreNotRecordedButAgentReadsAre is the asymmetry in activity.go's
// table, asserted from both sides. One side alone is not evidence: "human reads
// absent" also holds when nothing is recorded at all.
func TestHumanReadsAreNotRecordedButAgentReadsAre(t *testing.T) {
	svc, repo := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "a"}))
	require.NoError(t, err)

	before := len(repo.activity)
	_, err = svc.GetEntry(owner, "post", e.ID)
	require.NoError(t, err)
	_, err = svc.ListEntries(owner, "post", ListEntriesInput{})
	require.NoError(t, err)
	assert.Equal(t, before, len(repo.activity),
		"a row per console page view would bury the agent lines this table exists to surface")

	agent := ctxAgent("t1", "editor", uuid.New(), []string{"post"})
	_, err = svc.GetEntry(agent, "post", e.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.ActivityEntryRead, lastRow(t, repo).Action,
		"the dominating rule is about what an AGENT did, and reading is something it can do")
}

// TestHumanRefusedReadIsRecorded: the exemption above is for SUCCESSFUL reads
// only. A person hitting a wall is the same signal §3 wants from an agent
// hitting one.
func TestHumanRefusedReadIsRecorded(t *testing.T) {
	svc, repo := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name:      "post",
		Label:     "post",
		Fields:    []FieldInput{{Key: "title", Type: domain.FieldTypeString, Required: true}},
		ReadRoles: []string{"owner"},
	})
	require.NoError(t, err)

	viewer := ctxRole("t1", "viewer")
	_, err = svc.ListEntries(viewer, "post", ListEntriesInput{})
	require.Error(t, err)

	row := lastRow(t, repo)
	assert.Equal(t, domain.ActivityEntryList, row.Action)
	assert.Equal(t, domain.ActivityOutcomeDenied, row.Outcome)
	assert.Equal(t, domain.ActorKindHuman, row.ActorKind)
}

// TestDeliveryCredentialRecordsOnlyRefusedWrites covers the third actor kind.
//
// Its reads are public web traffic — ADR-004's counter accounts those, and a row
// per page view is an outage, not a record. Its refused WRITES are the rule
// ADR-004 says must not rest on one layer, so an attempt is worth a line.
func TestDeliveryCredentialRecordsOnlyRefusedWrites(t *testing.T) {
	svc, repo := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "a"}))
	require.NoError(t, err)
	_, err = svc.SetEntryStatus(owner, "post", e.ID, domain.StatusPublished, 0)
	require.NoError(t, err)

	delivery := ctxDelivery("t1")
	before := len(repo.activity)
	_, err = svc.GetEntry(delivery, "post", e.ID)
	require.NoError(t, err)
	assert.Equal(t, before, len(repo.activity), "delivery reads are traffic, not activity")

	_, err = svc.UpdateEntry(delivery, "post", e.ID, mustJSON(t, map[string]any{"title": "b"}), 0)
	require.Error(t, err)
	row := lastRow(t, repo)
	assert.Equal(t, domain.ActorKindService, row.ActorKind,
		"the third kind the schema has always allowed finally has an emitter")
	assert.Nil(t, row.ActorUserID, "no person to name — a reader must not invent one")
	assert.Equal(t, domain.ActivityOutcomeDenied, row.Outcome)
}

// --- what the row says --------------------------------------------------------

// TestUpdateRecordsOnlyTheKeysThatChanged. The list is what §1's per-field
// author attribution (step 4) will join on, so a list that names untouched keys
// would attribute someone else's field to whoever saved last.
func TestUpdateRecordsOnlyTheKeysThatChanged(t *testing.T) {
	svc, repo := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "a", "body": "unchanged"}))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"title", "body"}, lastRow(t, repo).ChangedKeys,
		"a create changed every key it wrote")

	// A PATCH that re-sends body with the SAME value and changes only title.
	_, err = svc.UpdateEntry(owner, "post", e.ID,
		mustJSON(t, map[string]any{"title": "b", "body": "unchanged"}), 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"title"}, lastRow(t, repo).ChangedKeys,
		"re-sending a value is not a change")
}

// TestTitleNeverComesFromAReadRestrictedField.
//
// The activity stream has NO field-level masking: the row is written under one
// caller's role and read later under another's. Sourcing a label from a
// restricted field would copy that value somewhere the restriction does not
// apply — the same leak §6's diff had to be fenced against, arriving by a
// different door.
//
// The CONTROL is what makes this non-vacuous: an unrestricted field of the same
// type DOES supply a label, so "TitleFor always returns empty" fails here.
func TestTitleNeverComesFromAReadRestrictedField(t *testing.T) {
	svc, repo := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name:  "secretive",
		Label: "secretive",
		Fields: []FieldInput{
			{Key: "codename", Type: domain.FieldTypeString, Required: true, ReadRoles: []string{"owner"}},
			{Key: "public_name", Type: domain.FieldTypeString},
		},
	})
	require.NoError(t, err)

	_, err = svc.CreateEntry(owner, "secretive", mustJSON(t, map[string]any{
		"codename": "OPERATION-BLUEBIRD", "public_name": "Spring launch",
	}))
	require.NoError(t, err)

	row := lastRow(t, repo)
	assert.NotContains(t, row.TargetTitle, "BLUEBIRD",
		"a restricted field's value must not be denormalised into a table with no masking")
	assert.Equal(t, "Spring launch", row.TargetTitle,
		"the control: an unrestricted field of the same type still supplies a label")
}

// TestTitleIsEmptyWhenOnlyRestrictedFieldsCouldSupplyIt — the honest
// degradation. "" means the console renders the id; it must not fall through to
// the restricted value because nothing else was available.
func TestTitleIsEmptyWhenOnlyRestrictedFieldsCouldSupplyIt(t *testing.T) {
	svc, repo := newSvc()
	owner := ctxRole("t1", "owner")
	_, err := svc.CreateContentType(owner, CreateTypeInput{
		Name:   "sealed",
		Label:  "sealed",
		Fields: []FieldInput{{Key: "codename", Type: domain.FieldTypeString, Required: true, ReadRoles: []string{"owner"}}},
	})
	require.NoError(t, err)
	_, err = svc.CreateEntry(owner, "sealed", mustJSON(t, map[string]any{"codename": "OPERATION-BLUEBIRD"}))
	require.NoError(t, err)
	assert.Empty(t, lastRow(t, repo).TargetTitle)
}

// TestDeleteRecordsTheTitle. Delete is the one action whose line cannot be
// enriched afterwards — the row it names will not exist to look up — so the
// label has to be captured before the delete, not derived from the stream later.
func TestDeleteRecordsTheTitle(t *testing.T) {
	svc, repo := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "Farewell"}))
	require.NoError(t, err)

	require.NoError(t, svc.DeleteEntry(owner, "post", e.ID))
	row := lastRow(t, repo)
	assert.Equal(t, domain.ActivityEntryDelete, row.Action)
	assert.Equal(t, "Farewell", row.TargetTitle)
	require.NotNil(t, row.TargetEntryID)
	assert.Equal(t, e.ID, *row.TargetEntryID)
}

// TestPublishAndUnpublishAreDistinctActions. They are one method behind one verb
// today; §1 splits them at the authorization layer in step 6. Recording them
// apart now means the stream does not have to be reinterpreted then.
func TestPublishAndUnpublishAreDistinctActions(t *testing.T) {
	svc, repo := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "a"}))
	require.NoError(t, err)

	_, err = svc.SetEntryStatus(owner, "post", e.ID, domain.StatusPublished, 0)
	require.NoError(t, err)
	assert.Equal(t, domain.ActivityEntryPublish, lastRow(t, repo).Action)

	_, err = svc.SetEntryStatus(owner, "post", e.ID, domain.StatusDraft, 0)
	require.NoError(t, err)
	assert.Equal(t, domain.ActivityEntryUnpublish, lastRow(t, repo).Action)
}

// TestRepublishingUnchangedContentIsNotARelease. SetEntryStatus treats it as a
// no-op on purpose; a stream showing a release per button press would disagree
// with the code about what a release is.
func TestRepublishingUnchangedContentIsNotARelease(t *testing.T) {
	svc, repo := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")
	e, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "a"}))
	require.NoError(t, err)
	_, err = svc.SetEntryStatus(owner, "post", e.ID, domain.StatusPublished, 0)
	require.NoError(t, err)

	before := len(repo.activity)
	_, err = svc.SetEntryStatus(owner, "post", e.ID, domain.StatusPublished, 0)
	require.NoError(t, err)
	assert.Equal(t, before, len(repo.activity))
}

// --- reading the stream --------------------------------------------------------

// TestAgentCannotReadTheActivityStream. It spans every content type, so it names
// none, so ADR-013 §4's untyped rule shuts an agent out BY CONSTRUCTION. That is
// the right answer and worth pinning: the record of what an agent did is for the
// person answerable for it.
func TestAgentCannotReadTheActivityStream(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")

	agent := ctxAgent("t1", "editor", uuid.New(), []string{"post"})
	_, err := svc.ListActivity(agent, ListActivityInput{})
	require.Error(t, err)
	assert.Equal(t, "CONTENT_AGENT_SCOPE_UNTYPED", codeOf(t, err))

	// Control: a person in the same tenant can read it, or the refusal above
	// would also hold for a stream nobody can reach.
	rows, err := svc.ListActivity(owner, ListActivityInput{})
	require.NoError(t, err)
	assert.NotEmpty(t, rows)
}

// TestDeliveryCredentialCannotReadTheActivityStream. content:list is a read
// action, so the chokepoint's delivery rule lets it past; the explicit refusal
// in ListActivity is what stops it. Same shape as Usage and ExportSchema, and
// for a sharper reason: this response is a list of who in the tenant did what.
func TestDeliveryCredentialCannotReadTheActivityStream(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")

	_, err := svc.ListActivity(ctxDelivery("t1"), ListActivityInput{})
	require.Error(t, err)
	assert.Equal(t, "FORBIDDEN", codeOf(t, err))
}

// TestActivityStreamIsTenantScoped. The fake enforces nothing the database does
// not; this asserts the SERVICE never asks for another tenant's rows — it takes
// the tenant from the subject and there is no parameter to override it.
func TestActivityStreamIsTenantScoped(t *testing.T) {
	svc, _ := newSvc()
	one := ctxRole("t1", "owner")
	two := ctxRole("t2", "owner")
	seedTitledType(t, svc, one, "post")
	seedTitledType(t, svc, two, "post")

	rows, err := svc.ListActivity(one, ListActivityInput{})
	require.NoError(t, err)
	require.NotEmpty(t, rows)

	other, err := svc.ListActivity(two, ListActivityInput{})
	require.NoError(t, err)
	require.NotEmpty(t, other)
	for _, r := range rows {
		for _, o := range other {
			assert.NotEqual(t, r.ID, o.ID, "one tenant's stream must not contain another's rows")
		}
	}
}

// TestActivityStreamNarrowsToOneEntry — the query step 4 will make when it
// attributes each changed field on the release screen.
func TestActivityStreamNarrowsToOneEntry(t *testing.T) {
	svc, _ := newSvc()
	owner := ctxRole("t1", "owner")
	seedTitledType(t, svc, owner, "post")
	a, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "a"}))
	require.NoError(t, err)
	b, err := svc.CreateEntry(owner, "post", mustJSON(t, map[string]any{"title": "b"}))
	require.NoError(t, err)
	_, err = svc.UpdateEntry(owner, "post", a.ID, mustJSON(t, map[string]any{"title": "a2"}), 0)
	require.NoError(t, err)

	rows, err := svc.ListActivity(owner, ListActivityInput{EntryID: &a.ID})
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	for _, r := range rows {
		require.NotNil(t, r.TargetEntryID)
		assert.Equal(t, a.ID, *r.TargetEntryID)
		assert.NotEqual(t, b.ID, *r.TargetEntryID)
	}
}

// --- attribution --------------------------------------------------------------

// TestUnattributableActorIsRecordedAsUnknown. §4's third state, on this table:
// a subject with no nameable person records NULL, and a reader renders unknown
// rather than inventing one.
func TestUnattributableActorIsRecordedAsUnknown(t *testing.T) {
	svc, repo := newSvc()
	nobody := authn.WithSubject(context.Background(), authn.Subject{
		UserID:     uuid.Nil,
		TenantID:   "t1",
		TenantRole: "owner",
	})
	seedTitledType(t, svc, nobody, "post")

	row := lastRow(t, repo)
	assert.Equal(t, domain.ActorKindHuman, row.ActorKind)
	assert.Nil(t, row.ActorUserID, "uuid.Nil is nobody nameable, and NULL is the honest storage of that")
}

// TestNothingIsRecordedWithoutATenant. There is no tenant to file the row under
// and RLS would refuse it; both refusals happen before any content state is
// touched, so nothing is lost but a line nobody could attribute.
func TestNothingIsRecordedWithoutATenant(t *testing.T) {
	svc, repo := newSvc()
	tenantless := authn.WithSubject(context.Background(), authn.Subject{UserID: uuid.New()})

	_, err := svc.ListEntries(tenantless, "post", ListEntriesInput{})
	require.Error(t, err)
	assert.Empty(t, repo.activity)

	_, err = svc.ListEntries(context.Background(), "post", ListEntriesInput{})
	require.Error(t, err)
	assert.Empty(t, repo.activity, "no subject at all is the same answer")
}
