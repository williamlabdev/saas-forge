package tools

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ADR-020 §10: the two component tools are plain reads of the component
// routes. What this pins is the PATH and the absence of any narrowing on this
// side — the CMS decides which components a credential may see, and a client
// that filtered again would agree with a CMS that had forgotten to.
func TestComponentToolsReadTheComponentRoutes(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args map[string]any
		path string
	}{
		{"cms_list_components", map[string]any{}, "/api/v1/content/components"},
		{"cms_get_component", map[string]any{"name": "seo"}, "/api/v1/content/components/seo"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			rec := &recorder{}
			cs := connect(t, rec, agentToken(t, "post"))
			res := call(t, cs, tc.tool, tc.args)
			assert.False(t, res.IsError, textOf(t, res))

			paths, queries := rec.seen()
			require.Equal(t, []string{tc.path}, paths)
			assert.Empty(t, queries[0], "component reads carry no query; the CMS scopes them from the bearer")
			assert.Equal(t, []string{http.MethodGet}, rec.methods)
		})
	}
}

// The CMS answers 403 CONTENT_AGENT_COMPONENT_NOT_ALLOWED for a component no
// whitelisted type embeds. That envelope has to reach the model intact —
// it is the only way it learns the name was outside its scope rather than
// misspelled.
func TestGetComponentForwardsTheScopeRefusal(t *testing.T) {
	rec := &recorder{
		status: http.StatusForbidden,
		body: `{"data":null,"error":{"code":"CONTENT_AGENT_COMPONENT_NOT_ALLOWED",` +
			`"message":"no content type on this credential's whitelist embeds component \"billing\"",` +
			`"details":{"component":"billing"}},"meta":{}}`,
	}
	cs := connect(t, rec, agentToken(t, "post"))
	res := call(t, cs, "cms_get_component", map[string]any{"name": "billing"})
	assert.True(t, res.IsError)
	body := textOf(t, res)
	assert.Contains(t, body, "CONTENT_AGENT_COMPONENT_NOT_ALLOWED")
	assert.Contains(t, body, `"component":"billing"`)
}
