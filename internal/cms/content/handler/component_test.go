package handler

import (
	"net/http"
	"testing"

	"github.com/williamlabdev/saas-forge/internal/cms/content/service"
)

// Reusable components (ADR-020 §6): every route is a distinct verb that hands
// the service the name (and key) the URL carried and the body as typed input.
// Nothing here asserts policy — that is the service's; these assert the
// PLUMBING, which is the only thing a handler can get wrong.

func TestComponentRoutes_ReachTheServiceWithNameAndKey(t *testing.T) {
	cases := []struct {
		name   string
		method string
		target string
		body   string
		code   int
		key    string
	}{
		{"create", http.MethodPost, "/api/v1/content/components", `{"name":"seo","fields":[{"key":"title","type":"string"}]}`, http.StatusCreated, ""},
		{"list", http.MethodGet, "/api/v1/content/components", "", http.StatusOK, ""},
		{"get", http.MethodGet, "/api/v1/content/components/seo", "", http.StatusOK, ""},
		{"patch", http.MethodPatch, "/api/v1/content/components/seo", `{"label":"SEO"}`, http.StatusOK, ""},
		{"delete", http.MethodDelete, "/api/v1/content/components/seo", "", http.StatusNoContent, ""},
		{"add field", http.MethodPost, "/api/v1/content/components/seo/fields", `{"key":"kind","type":"enum","enum_values":["a"]}`, http.StatusCreated, ""},
		{"patch field", http.MethodPatch, "/api/v1/content/components/seo/fields/title", `{"label":"Title"}`, http.StatusOK, "title"},
		{"rename field", http.MethodPost, "/api/v1/content/components/seo/fields/title/rename", `{"key":"headline"}`, http.StatusOK, "title"},
		{"delete field", http.MethodDelete, "/api/v1/content/components/seo/fields/title", "", http.StatusOK, "title"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeContentService{componentDTO: service.ComponentDTO{Name: "seo"}}
			rec := do(t, svc, tc.method, tc.target, tc.body)
			if rec.Code != tc.code {
				t.Fatalf("code=%d want %d body=%s", rec.Code, tc.code, rec.Body)
			}
			if tc.name != "create" && tc.name != "list" && svc.lastComponent != "seo" {
				t.Fatalf("component name must come from the URL, got %q", svc.lastComponent)
			}
			if tc.key != "" && svc.lastFieldKey != tc.key {
				t.Fatalf("field key must come from the URL, got %q", svc.lastFieldKey)
			}
		})
	}
}

func TestComponentRoutes_BodiesArriveTyped(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		svc := &fakeContentService{componentDTO: service.ComponentDTO{Name: "seo"}}
		rec := do(t, svc, http.MethodPost, "/api/v1/content/components",
			`{"name":"seo","label":"SEO","fields":[{"key":"title","type":"string","required":true},{"key":"tags","type":"string","multiple":true}]}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		in := svc.lastComponentCreate
		if in.Name != "seo" || in.Label != "SEO" || len(in.Fields) != 2 {
			t.Fatalf("create input did not reach the service intact: %+v", in)
		}
		if !in.Fields[0].Required || !in.Fields[1].Multiple {
			t.Fatalf("sub-field attributes lost on the way: %+v", in.Fields)
		}
	})
	t.Run("patch label", func(t *testing.T) {
		svc := &fakeContentService{componentDTO: service.ComponentDTO{Name: "seo"}}
		do(t, svc, http.MethodPatch, "/api/v1/content/components/seo", `{"label":"Search"}`)
		if svc.lastComponentUpdate.Label == nil || *svc.lastComponentUpdate.Label != "Search" {
			t.Fatalf("label did not reach the service: %+v", svc.lastComponentUpdate)
		}
	})
	t.Run("add field", func(t *testing.T) {
		svc := &fakeContentService{componentDTO: service.ComponentDTO{Name: "seo"}}
		do(t, svc, http.MethodPost, "/api/v1/content/components/seo/fields", `{"key":"kind","type":"enum","enum_values":["a","b"]}`)
		f := svc.lastComponentField
		if f.Key != "kind" || f.Type != "enum" || len(f.EnumValues) != 2 {
			t.Fatalf("field input did not reach the service intact: %+v", f)
		}
	})
	t.Run("patch field keeps absent keys absent", func(t *testing.T) {
		svc := &fakeContentService{componentDTO: service.ComponentDTO{Name: "seo"}}
		do(t, svc, http.MethodPatch, "/api/v1/content/components/seo/fields/title", `{"required":true}`)
		in := svc.lastUpdateField
		if in.Required == nil || !*in.Required {
			t.Fatalf("required did not reach the service: %+v", in)
		}
		if in.Label != nil || in.Type != nil || in.Multiple != nil || in.Unique != nil {
			t.Fatalf("keys that were never sent must arrive nil: %+v", in)
		}
	})
	t.Run("rename", func(t *testing.T) {
		svc := &fakeContentService{componentDTO: service.ComponentDTO{Name: "seo"}}
		do(t, svc, http.MethodPost, "/api/v1/content/components/seo/fields/title/rename", `{"key":"headline"}`)
		if svc.lastRename.Key != "headline" {
			t.Fatalf("the NEW key must come from the body, got %q", svc.lastRename.Key)
		}
	})
	t.Run("delete", func(t *testing.T) {
		svc := &fakeContentService{}
		rec := do(t, svc, http.MethodDelete, "/api/v1/content/components/seo", "")
		if rec.Code != http.StatusNoContent || svc.deleteComponentCall != 1 {
			t.Fatalf("delete: code=%d calls=%d", rec.Code, svc.deleteComponentCall)
		}
	})
}

// force is a query flag, exactly as on the type-field route: a body key
// would be a second spelling of the same consent.
func TestComponentRoutes_DeleteFieldForceIsAQueryFlag(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{"", false},
		{"?force=true", true},
		{"?force=1", false},
		{"?force=false", false},
	} {
		svc := &fakeContentService{componentDTO: service.ComponentDTO{Name: "seo"}}
		do(t, svc, http.MethodDelete, "/api/v1/content/components/seo/fields/title"+tc.query, "")
		if svc.lastForce != tc.want {
			t.Fatalf("force=%v want %v for query %q", svc.lastForce, tc.want, tc.query)
		}
	}
}

func TestComponentRoutes_MalformedBodiesAre400(t *testing.T) {
	for _, tc := range []struct {
		method, target string
	}{
		{http.MethodPost, "/api/v1/content/components"},
		{http.MethodPatch, "/api/v1/content/components/seo"},
		{http.MethodPost, "/api/v1/content/components/seo/fields"},
		{http.MethodPatch, "/api/v1/content/components/seo/fields/title"},
		{http.MethodPost, "/api/v1/content/components/seo/fields/title/rename"},
	} {
		svc := &fakeContentService{}
		rec := do(t, svc, tc.method, tc.target, `{not json`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s %s: code=%d want 400 body=%s", tc.method, tc.target, rec.Code, rec.Body)
		}
		if svc.lastComponent != "" || svc.deleteComponentCall != 0 {
			t.Fatalf("a malformed body must not reach the service")
		}
	}
}
