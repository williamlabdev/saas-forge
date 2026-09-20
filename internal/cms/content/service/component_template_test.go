package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// Built-in component templates (ADR-020 Amendment 2): ListComponentTemplates
// and InstallComponentTemplate run through the same code CreateComponent does,
// so these tests focus on the two things that path does not already cover:
// the catalog shape, and that installing reaches CreateComponent's own rules
// (duplicate name, and the "templates" name reservation) rather than a copy
// of them.

func TestListComponentTemplates(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	list, err := svc.ListComponentTemplates(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	tmpl := list[0]
	assert.Equal(t, "seo", tmpl.Name)
	assert.Equal(t, "SEO", tmpl.Label)
	assert.NotEmpty(t, tmpl.Description)
	require.Len(t, tmpl.Fields, 5)

	byKey := map[string]FieldDTO{}
	for _, f := range tmpl.Fields {
		byKey[f.Key] = f
	}

	title := byKey["meta_title"]
	assert.Equal(t, domain.FieldTypeString, title.Type)
	require.NotNil(t, title.Max)
	assert.Equal(t, float64(70), *title.Max)

	desc := byKey["meta_description"]
	assert.Equal(t, domain.FieldTypeText, desc.Type)
	require.NotNil(t, desc.Max)
	assert.Equal(t, float64(160), *desc.Max)

	ogImage := byKey["og_image"]
	assert.Equal(t, domain.FieldTypeFile, ogImage.Type)

	canonical := byKey["canonical_url"]
	assert.Equal(t, domain.FieldTypeString, canonical.Type)
	assert.NotEmpty(t, canonical.Pattern)

	noIndex := byKey["no_index"]
	assert.Equal(t, domain.FieldTypeBoolean, noIndex.Type)

	for _, f := range tmpl.Fields {
		assert.NotEmpty(t, f.Description, "field %s needs an editor-facing description", f.Key)
	}
}

func TestInstallComponentTemplate(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		svc, _ := newSvc()
		ctx := ctxTenant("t1")
		dto, err := svc.InstallComponentTemplate(ctx, "seo")
		require.NoError(t, err)
		assert.Equal(t, "seo", dto.Name)
		assert.Equal(t, "SEO", dto.Label)
		assert.Equal(t, []string{"meta_title", "meta_description", "og_image", "canonical_url", "no_index"}, componentFieldKeys(dto))

		// It is an ordinary component from here on: readable through the same
		// GetComponent path a hand-authored POST /components would produce.
		got, err := svc.GetComponent(ctx, "seo")
		require.NoError(t, err)
		assert.Equal(t, dto.Name, got.Name)
	})

	t.Run("duplicate name is CreateComponent's own error", func(t *testing.T) {
		svc, _ := newSvc()
		ctx := ctxTenant("t1")
		_, err := svc.InstallComponentTemplate(ctx, "seo")
		require.NoError(t, err)
		_, err = svc.InstallComponentTemplate(ctx, "seo")
		mustCode(t, err, "CONTENT_COMPONENT_EXISTS", 409)
	})

	t.Run("installing after a hand-authored component of the same name is also a duplicate", func(t *testing.T) {
		svc, _ := newSvc()
		ctx := ctxTenant("t1")
		_, err := svc.CreateComponent(ctx, seoComponentInput())
		require.NoError(t, err)
		_, err = svc.InstallComponentTemplate(ctx, "seo")
		mustCode(t, err, "CONTENT_COMPONENT_EXISTS", 409)
	})

	t.Run("unknown template is 404", func(t *testing.T) {
		svc, _ := newSvc()
		ctx := ctxTenant("t1")
		_, err := svc.InstallComponentTemplate(ctx, "nope")
		mustCode(t, err, "CONTENT_COMPONENT_TEMPLATE_NOT_FOUND", 404)
	})
}

// TestComponentNameReserved covers what makes GET /components/templates
// reachable at all: a component literally named "templates" is refused at
// both the points where a name is chosen, not only at creation — a rename
// could otherwise produce the same unreachable component just as easily.
func TestComponentNameReserved(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		svc, _ := newSvc()
		ctx := ctxTenant("t1")
		in := seoComponentInput()
		in.Name = "templates"
		_, err := svc.CreateComponent(ctx, in)
		mustCode(t, err, "CONTENT_COMPONENT_NAME_RESERVED", 422)
	})

	t.Run("rename", func(t *testing.T) {
		svc, _ := newSvc()
		ctx := ctxTenant("t1")
		_, err := svc.CreateComponent(ctx, seoComponentInput())
		require.NoError(t, err)
		_, err = svc.RenameComponent(ctx, "seo", RenameInput{Name: "templates"})
		mustCode(t, err, "CONTENT_COMPONENT_NAME_RESERVED", 422)
	})
}
