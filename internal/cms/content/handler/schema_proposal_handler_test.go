package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/williamlabdev/saas-forge/internal/cms/content/service"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// ADR-013 §3 step 8 at the HTTP layer.

func mustBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A proposal is a request to run an apply, so it reads ?prune from the same
// place apply does. Asserted rather than assumed: prune is what decides whether
// a document is an overlay or an authority, and a proposal that dropped it
// would be approvable into the meaning nobody filed.
func TestProposeSchema_ForwardsArtifactAndPrune(t *testing.T) {
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/schema/proposals?prune=true", mustBody(t, sampleArtifact()))

	if rec.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s — a filed proposal is a created resource", rec.Code, rec.Body)
	}
	if svc.proposeCalls != 1 {
		t.Fatalf("proposeCalls=%d", svc.proposeCalls)
	}
	if !svc.lastPrune {
		t.Fatal("?prune=true did not reach the service")
	}
	if len(svc.lastArtifact.Types) != 1 || svc.lastArtifact.Types[0].Name != "post" {
		t.Fatalf("artifact did not reach the service: %+v", svc.lastArtifact)
	}
}

// The envelope check is the SAME one apply uses (decodeArtifact). A proposal
// endpoint with its own looser rule would let a document into the queue that
// the apply then refuses — discovered by a person pressing a button rather than
// by the caller that sent it.
func TestProposeSchema_RejectsAForeignEnvelope(t *testing.T) {
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/schema/proposals",
		`{"artifact_version":"v1","kind":"cms.entries/v1","types":[]}`)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.proposeCalls != 0 {
		t.Fatal("a document of the wrong kind reached the service")
	}
}

func TestApproveAndRejectProposal_ForwardTheID(t *testing.T) {
	id := uuid.New()

	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/schema/proposals/"+id.String()+"/approve", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.approveCalls != 1 || svc.lastProposalID != id {
		t.Fatalf("approveCalls=%d id=%s", svc.approveCalls, svc.lastProposalID)
	}

	svc = &fakeContentService{}
	rec = do(t, svc, http.MethodPost, "/api/v1/content/schema/proposals/"+id.String()+"/reject", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.rejectCalls != 1 || svc.lastProposalID != id {
		t.Fatalf("rejectCalls=%d id=%s", svc.rejectCalls, svc.lastProposalID)
	}
}

func TestDecidingAProposalWithAMalformedIDNeverReachesTheService(t *testing.T) {
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/schema/proposals/not-a-uuid/approve", "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.approveCalls != 0 {
		t.Fatal("a malformed id reached the service — a uuid.Nil approval would answer about a proposal nobody filed")
	}
}

// The refusals the service produces must arrive with their codes intact: the
// console tells a stale proposal from an expired one, and both from a plain
// failure, using exactly these.
func TestProposalConflictsKeepTheirCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{"stale", service.ErrProposalStale, "CONTENT_SCHEMA_PROPOSAL_STALE"},
		{"expired", service.ErrProposalExpired, "CONTENT_SCHEMA_PROPOSAL_EXPIRED"},
		{"decided", service.ErrProposalDecided, "CONTENT_SCHEMA_PROPOSAL_DECIDED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeContentService{err: tc.err}
			rec := do(t, svc, http.MethodPost,
				"/api/v1/content/schema/proposals/"+uuid.New().String()+"/approve", "")
			if rec.Code != http.StatusConflict {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %s", rec.Body)
			}
			if body.Error.Code != tc.code {
				t.Fatalf("code=%q want %q — body=%s", body.Error.Code, tc.code, rec.Body)
			}
		})
	}
}

func TestListProposals_IsEnveloped(t *testing.T) {
	svc := &fakeContentService{proposals: []service.SchemaProposalDTO{{ID: uuid.New().String(), Status: "pending"}}}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/schema/proposals", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Data struct {
			Proposals []service.SchemaProposalDTO `json:"proposals"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %s", rec.Body)
	}
	if len(body.Data.Proposals) != 1 {
		t.Fatalf("proposals=%+v", body.Data.Proposals)
	}
}

func TestProposalEndpointsPropagateServiceErrors(t *testing.T) {
	svc := &fakeContentService{err: apperrors.ErrForbidden}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/schema/proposals", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// The proposer's door is a different route reaching a different method, and the
// two are distinguishable only by which one the router picked: both take the
// same id in the same position, so an assertion about the id passes either way.
// That is what ownProposalCalls is for.
func TestOwnProposalRouteReachesTheNarrowedRead(t *testing.T) {
	id := uuid.New()

	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/schema/proposals/mine/"+id.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.ownProposalCalls != 1 || svc.lastProposalID != id {
		t.Fatalf("ownProposalCalls=%d id=%s", svc.ownProposalCalls, svc.lastProposalID)
	}

	// And the approver's route did NOT move: /{id} must still reach the queue's
	// read, or this change would have quietly handed every approver the narrowed
	// view instead.
	svc = &fakeContentService{}
	rec = do(t, svc, http.MethodGet, "/api/v1/content/schema/proposals/"+id.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.ownProposalCalls != 0 {
		t.Fatal("the approver's route reached the proposer's narrowed read")
	}
}

func TestOwnProposalWithAMalformedIDNeverReachesTheService(t *testing.T) {
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/schema/proposals/mine/not-a-uuid", "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.ownProposalCalls != 0 {
		t.Fatal("a malformed id reached the service")
	}
}

// The two list routes differ by ONE path segment and answer with the same
// envelope, so a handler wired to the wrong service method is invisible to any
// assertion about the body. Which method was reached is the test.
func TestOwnProposalListRouteReachesTheNarrowedListAndNotTheQueue(t *testing.T) {
	svc := &fakeContentService{}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/schema/proposals/mine", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.ownProposalListCalls != 1 || svc.queueListCalls != 0 {
		t.Fatalf("ownProposalListCalls=%d queueListCalls=%d — /mine reached the approvers' queue, whose plans are full-scope",
			svc.ownProposalListCalls, svc.queueListCalls)
	}

	// And the queue did NOT move onto the proposer's method: /schema/proposals
	// must still be the approvers' list, or every approver would silently be
	// served the narrowed view instead.
	svc = &fakeContentService{}
	rec = do(t, svc, http.MethodGet, "/api/v1/content/schema/proposals", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.queueListCalls != 1 || svc.ownProposalListCalls != 0 {
		t.Fatalf("queueListCalls=%d ownProposalListCalls=%d", svc.queueListCalls, svc.ownProposalListCalls)
	}

	// The list did not swallow the single read: /mine/{id} still reaches it.
	// chi resolves a static segment ahead of a param, but "mine" is now BOTH a
	// leaf and a prefix, which is the arrangement that goes wrong quietly.
	svc = &fakeContentService{}
	id := uuid.New()
	rec = do(t, svc, http.MethodGet, "/api/v1/content/schema/proposals/mine/"+id.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if svc.ownProposalCalls != 1 || svc.ownProposalListCalls != 0 || svc.lastProposalID != id {
		t.Fatalf("ownProposalCalls=%d ownProposalListCalls=%d id=%s",
			svc.ownProposalCalls, svc.ownProposalListCalls, svc.lastProposalID)
	}
}

// Same envelope key as the queue's list, so the console decodes both with one
// shape. The ROW type is what differs.
func TestOwnProposalList_IsEnvelopedLikeTheQueue(t *testing.T) {
	svc := &fakeContentService{ownProposals: []service.OwnSchemaProposalDTO{
		{ID: uuid.New().String(), Status: "pending", PlanRecorded: true},
	}}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/schema/proposals/mine", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Data struct {
			Proposals []service.OwnSchemaProposalDTO `json:"proposals"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %s", rec.Body)
	}
	if len(body.Data.Proposals) != 1 || !body.Data.Proposals[0].PlanRecorded {
		t.Fatalf("proposals=%+v", body.Data.Proposals)
	}
}

func TestOwnProposalListPropagatesServiceRefusals(t *testing.T) {
	svc := &fakeContentService{err: apperrors.ErrForbidden}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/schema/proposals/mine", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}
