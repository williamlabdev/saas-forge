package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/cms/content/service"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Built-in component templates (ADR-020 Amendment 2): GET /components/templates
// and POST /components/templates/{name} plumbing, and the routing precedence
// they depend on.

func TestComponentTemplateRoutes_ReachTheService(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		svc := &fakeContentService{componentTemplateList: []service.ComponentTemplateDTO{
			{Name: "seo", Label: "SEO", Fields: []service.FieldDTO{{Key: "meta_title", Type: "string"}}},
		}}
		rec := do(t, svc, http.MethodGet, "/api/v1/content/components/templates", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		var body struct {
			Data struct {
				Templates []service.ComponentTemplateDTO `json:"templates"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		if len(body.Data.Templates) != 1 || body.Data.Templates[0].Name != "seo" {
			t.Fatalf(`templates did not arrive wrapped as {"templates":[...]}: %s`, rec.Body)
		}
	})

	t.Run("install", func(t *testing.T) {
		svc := &fakeContentService{componentDTO: service.ComponentDTO{Name: "seo", Label: "SEO"}}
		rec := do(t, svc, http.MethodPost, "/api/v1/content/components/templates/seo", "")
		if rec.Code != http.StatusCreated {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
		if svc.lastTemplateName != "seo" {
			t.Fatalf("template name must come from the URL, got %q", svc.lastTemplateName)
		}
		var body struct {
			Data service.ComponentDTO `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		if body.Data.Name != "seo" {
			t.Fatalf("response is not the created ComponentDTO: %s", rec.Body)
		}
	})

	t.Run("install unknown template surfaces the service error", func(t *testing.T) {
		svc := &fakeContentService{err: apperrors.New("CONTENT_COMPONENT_TEMPLATE_NOT_FOUND", "component template is not defined", http.StatusNotFound)}
		rec := do(t, svc, http.MethodPost, "/api/v1/content/components/templates/nope", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("code=%d want 404 body=%s", rec.Code, rec.Body)
		}
	})
}

// TestComponentTemplateRoutes_StaticBeatsParam proves GET /components/templates
// resolves to the templates listing and NOT to GET /components/{name} with
// name="templates" — and that this holds regardless of which of the two
// routes chi registers first. A component whose name is literally "templates"
// is refused at create/rename time (validateComponentName) precisely because
// this route would otherwise make it unreachable.
func TestComponentTemplateRoutes_StaticBeatsParam(t *testing.T) {
	for _, order := range []string{"templates registered first", "name param registered first"} {
		t.Run(order, func(t *testing.T) {
			svc := &fakeContentService{
				componentTemplateList: []service.ComponentTemplateDTO{{Name: "seo"}},
				// what GetComponent("templates") would return, if this route
				// ever reached it instead of the templates listing.
				componentDTO: service.ComponentDTO{Name: "templates"},
			}
			h := NewHandler(svc)
			r := chi.NewRouter()
			if order == "templates registered first" {
				r.Get("/components/templates", h.listComponentTemplates)
				r.Get("/components/{name}", h.getComponent)
			} else {
				r.Get("/components/{name}", h.getComponent)
				r.Get("/components/templates", h.listComponentTemplates)
			}

			req := httptest.NewRequest(http.MethodGet, "/components/templates", nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			var body struct {
				Data map[string]json.RawMessage `json:"data"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			if _, ok := body.Data["templates"]; !ok {
				t.Fatalf("GET /components/templates must resolve to the templates listing, not GetComponent(name=\"templates\"): %s", rec.Body)
			}
			if svc.lastComponent != "" {
				t.Fatalf("GetComponent must never be called for the templates route, got name=%q", svc.lastComponent)
			}
		})
	}
}
