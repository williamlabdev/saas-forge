package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/service"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

func sampleArtifact() domain.Artifact {
	return domain.Artifact{
		ArtifactVersion: domain.ArtifactVersion1,
		Kind:            domain.KindContentSchema,
		Types: []domain.ArtifactType{{
			Name:   "post",
			Label:  "Post",
			Fields: []domain.ArtifactField{{Key: "title", Type: "text", Label: "Title", Required: true}},
		}},
	}
}

// Export answers with the artifact itself — no envelope — because the file it
// returns is meant to be saved, reviewed in a diff, and posted back to apply.
func TestExportSchema_ReturnsBareArtifactAsAnAttachment(t *testing.T) {
	svc := &fakeContentService{artifact: sampleArtifact()}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/schema/export", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "schema.artifact.json") {
		t.Fatalf("Content-Disposition=%q — the response is a file to save", cd)
	}
	var envelopeProbe map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &envelopeProbe); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if _, wrapped := envelopeProbe["data"]; wrapped {
		t.Fatalf("export must NOT be enveloped — the body is the artifact: %s", rec.Body)
	}
	if string(envelopeProbe["kind"]) != `"`+domain.KindContentSchema+`"` {
		t.Fatalf("kind=%s, want the schema kind at the top level", envelopeProbe["kind"])
	}
}

func TestExportSchema_ServiceError(t *testing.T) {
	svc := &fakeContentService{err: apperrors.New("FORBIDDEN", "nope", http.StatusForbidden)}
	rec := do(t, svc, http.MethodGet, "/api/v1/content/schema/export", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// The round trip is the whole point of the format: what export writes, apply
// must accept. Asserting it across the two HTTP routes catches a divergence
// that neither endpoint's own test would — an added field that export emits and
// apply's DisallowUnknownFields then rejects.
func TestExportedArtifactIsAcceptedByApply(t *testing.T) {
	exporter := &fakeContentService{artifact: sampleArtifact()}
	exported := do(t, exporter, http.MethodGet, "/api/v1/content/schema/export", "")
	if exported.Code != http.StatusOK {
		t.Fatalf("export failed: code=%d body=%s", exported.Code, exported.Body)
	}

	applier := &fakeContentService{}
	rec := do(t, applier, http.MethodPost, "/api/v1/content/schema/apply", exported.Body.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("apply rejected what export produced: code=%d body=%s", rec.Code, rec.Body)
	}
	if applier.lastArtifact.Kind != domain.KindContentSchema || len(applier.lastArtifact.Types) != 1 {
		t.Fatalf("artifact did not survive the round trip: %+v", applier.lastArtifact)
	}
}

// prune turns destructive steps on. Only the exact string "true" may do that:
// anything looser means a typo, or a client sending ?prune=1 out of habit,
// silently authorises deletions.
func TestSchemaChange_OnlyExactTrueEnablesPrune(t *testing.T) {
	body, err := domain.MarshalArtifact(sampleArtifact())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cases := []struct {
		query string
		want  bool
	}{
		{"", false},
		{"?prune=true", true},
		{"?prune=1", false},
		{"?prune=yes", false},
		{"?prune=TRUE", false},
		{"?prune=true%20", false},
	}
	for _, route := range []string{"plan", "apply"} {
		for _, tc := range cases {
			t.Run(route+" "+tc.query, func(t *testing.T) {
				svc := &fakeContentService{}
				rec := do(t, svc, http.MethodPost,
					"/api/v1/content/schema/"+route+tc.query, string(body))
				if rec.Code != http.StatusOK {
					t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
				}
				if svc.lastPrune != tc.want {
					t.Fatalf("prune=%v, want %v for %q", svc.lastPrune, tc.want, tc.query)
				}
			})
		}
	}
}

// The two routes differ only in which service method they call. Swap them and
// a "show me what would happen" request rewrites the schema instead.
func TestPlanAndApplyDispatchToTheirOwnVerb(t *testing.T) {
	body, err := domain.MarshalArtifact(sampleArtifact())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	planner := &fakeContentService{}
	if rec := do(t, planner, http.MethodPost, "/api/v1/content/schema/plan", string(body)); rec.Code != http.StatusOK {
		t.Fatalf("plan: code=%d body=%s", rec.Code, rec.Body)
	}
	if planner.planCalls != 1 || planner.applyCalls != 0 {
		t.Fatalf("plan route ran plan=%d apply=%d — a plan must never write", planner.planCalls, planner.applyCalls)
	}

	applier := &fakeContentService{}
	if rec := do(t, applier, http.MethodPost, "/api/v1/content/schema/apply", string(body)); rec.Code != http.StatusOK {
		t.Fatalf("apply: code=%d body=%s", rec.Code, rec.Body)
	}
	if applier.applyCalls != 1 || applier.planCalls != 0 {
		t.Fatalf("apply route ran apply=%d plan=%d", applier.applyCalls, applier.planCalls)
	}
}

func TestSchemaChange_RejectsMalformedAndForeignDocuments(t *testing.T) {
	good := sampleArtifact()
	cases := []struct {
		name string
		body string
		want int
	}{
		{"malformed json", `{bad`, http.StatusBadRequest},
		{"unknown field", `{"artifact_version":"` + domain.ArtifactVersion1 + `","kind":"` + domain.KindContentSchema + `","types":[],"surprise":1}`, http.StatusBadRequest},
		{"wrong kind", `{"artifact_version":"` + domain.ArtifactVersion1 + `","kind":"something.else","types":[]}`, http.StatusUnprocessableEntity},
		{"wrong version", `{"artifact_version":"v99","kind":"` + domain.KindContentSchema + `","types":[]}`, http.StatusUnprocessableEntity},
		{"empty body", ``, http.StatusBadRequest},
	}
	for _, route := range []string{"plan", "apply"} {
		for _, tc := range cases {
			t.Run(route+" "+tc.name, func(t *testing.T) {
				svc := &fakeContentService{artifact: good, plan: service.PlanResult{}}
				rec := do(t, svc, http.MethodPost, "/api/v1/content/schema/"+route, tc.body)
				if rec.Code != tc.want {
					t.Fatalf("code=%d want=%d body=%s", rec.Code, tc.want, rec.Body)
				}
				// A document refused at the envelope must never have reached the
				// service — that is the point of checking kind before types.
				if svc.planCalls != 0 || svc.applyCalls != 0 {
					t.Fatalf("refused document still reached the service (plan=%d apply=%d)",
						svc.planCalls, svc.applyCalls)
				}
			})
		}
	}
}

func TestSchemaChange_ServiceErrorKeepsItsStatus(t *testing.T) {
	body, err := domain.MarshalArtifact(sampleArtifact())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	svc := &fakeContentService{err: apperrors.New("CONTENT_SCHEMA_BLOCKED", "blocked", http.StatusConflict)}
	rec := do(t, svc, http.MethodPost, "/api/v1/content/schema/apply", string(body))
	if rec.Code != http.StatusConflict {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}
