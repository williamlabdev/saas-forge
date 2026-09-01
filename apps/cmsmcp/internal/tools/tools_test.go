package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/apps/cmsmcp/internal/upstream"
	authjwt "github.com/williamlabdev/saas-forge/internal/auth/jwt"
)

// recorder is a stand-in Domain API that records what the MCP server asked it.
// The tools' whole job is turning a tool call into an HTTP call, so what went
// out on the wire IS the behaviour under test.
type recorder struct {
	mu      sync.Mutex
	paths   []string
	queries []url.Values
	// methods, bodies and headers exist for the write tools. A read tool is
	// fully described by where it went; a write is not — the same path with the
	// wrong verb, an empty body, or a dropped Idempotency-Key are all failures
	// that a path-only recorder reports as success.
	methods []string
	bodies  []string
	headers []http.Header
	// reply is the envelope body served for every request; status 0 means 200.
	status int
	body   string
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sent, _ := io.ReadAll(r.Body)

	rec.mu.Lock()
	rec.paths = append(rec.paths, r.URL.Path)
	rec.queries = append(rec.queries, r.URL.Query())
	rec.methods = append(rec.methods, r.Method)
	rec.bodies = append(rec.bodies, string(sent))
	rec.headers = append(rec.headers, r.Header.Clone())
	status, body := rec.status, rec.body
	rec.mu.Unlock()

	if status == 0 {
		status = http.StatusOK
	}
	if body == "" {
		body = `{"data":{"ok":true},"error":null,"meta":{}}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (rec *recorder) seen() ([]string, []url.Values) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.paths...), append([]url.Values(nil), rec.queries...)
}

// agentToken mints a real agent credential rather than hand-rolling a JWT, so
// the claim names this package reads stay tied to the ones the signer writes.
func agentToken(t *testing.T, allowed ...string) string {
	t.Helper()
	s := authjwt.NewSigner([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	raw, _, err := s.IssueAccessToken(uuid.New(), []string{"admin"}, "tenant-a", "admin", "eu", true)
	require.NoError(t, err)
	minter, err := s.ParseAccessToken(raw)
	require.NoError(t, err)
	agent, _, err := s.IssueAgentToken(minter, uuid.New(), "content-bot", "editor", allowed)
	require.NoError(t, err)
	return agent
}

// connect wires a real MCP client to a real MCP server over the in-memory
// transport. The tools are exercised through the protocol, not by calling the
// handlers directly: a tool whose schema the SDK rejects would still pass a
// direct call, and that failure only shows up at the transport.
func connect(t *testing.T, rec *recorder, token string) *mcp.ClientSession {
	t.Helper()
	api := httptest.NewServer(rec)
	t.Cleanup(api.Close)

	up := upstream.NewClient(api.URL, "gw-secret", 5*time.Second)
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	NewRegistry(up, token, 10, 50).Install(srv)

	ct, st := mcp.NewInMemoryTransports()
	_, err := srv.Connect(context.Background(), st, nil)
	require.NoError(t, err)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil).
		Connect(context.Background(), ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	return res
}

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.Len(t, res.Content, 1)
	tc, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok, "expected text content")
	return tc.Text
}

// The tool list is UX (ADR-013 §5), so this test is not an authorization
// boundary and must not be read as one. What it pins is SCOPE: step 7 adds
// exactly three write tools, and the four names §5 marks 刻意不開 —
// cms_delete_entry, media upload, webhook/usage, schema apply — stay absent.
//
// A tool appearing here is a deliberate act. A tool DISAPPEARING is the more
// interesting failure, because a write tool silently dropped looks from the
// outside like an agent that has stopped writing rather than a server missing a
// registration.
func TestTheToolSurfaceIsTheStepSevenSet(t *testing.T) {
	cs := connect(t, &recorder{}, agentToken(t, "post"))

	var got []string
	for tool, err := range cs.Tools(context.Background(), nil) {
		require.NoError(t, err)
		got = append(got, tool.Name)
	}
	assert.ElementsMatch(t, []string{
		"cms_describe", "cms_list_entries", "cms_get_entry", "cms_list_translations",
		"cms_create_entry", "cms_update_entry", "cms_set_status",
	}, got)
	assert.NotContains(t, got, "cms_delete_entry",
		"§5 blocks deletion on version history; step 7 does not change that")
	assert.NotContains(t, got, "cms_publish_entry",
		"ADR-014 §1: a person releases. The gate is agent_gate.go, not this list — "+
			"but a publish tool here would still be a scope change nobody ruled on")
}

// ADR-013 §5 and §A: describe walks AllowedTypes one at a time. GET /types
// names no single content type, so an agent credential is refused it by
// construction — a describe built on it would 403 for every agent alive.
func TestDescribeWalksAllowedTypesAndNeverAsksForTheTypeList(t *testing.T) {
	rec := &recorder{}
	cs := connect(t, rec, agentToken(t, "post", "note"))

	res := call(t, cs, "cms_describe", map[string]any{})
	assert.False(t, res.IsError, textOf(t, res))

	paths, _ := rec.seen()
	assert.Equal(t, []string{
		"/api/v1/content/types/post",
		"/api/v1/content/types/note",
	}, paths)
	assert.NotContains(t, paths, "/api/v1/content/types")

	// Both declarations come back, so the agent sees its whole scope.
	var docs []json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(textOf(t, res)), &docs))
	assert.Len(t, docs, 2)
}

func TestDescribeWithAnExplicitTypeFetchesOnlyThatOne(t *testing.T) {
	rec := &recorder{}
	cs := connect(t, rec, agentToken(t, "post", "note"))

	res := call(t, cs, "cms_describe", map[string]any{"type": "note"})
	assert.False(t, res.IsError, textOf(t, res))

	paths, _ := rec.seen()
	assert.Equal(t, []string{"/api/v1/content/types/note"}, paths)
}

// A credential with no whitelist is either a human token — which would make
// every ADR-013 narrowing inert — or an unscoped agent one, which the Domain
// API refuses outright. Either way the operator has to act, so the tool must
// say so rather than describe nothing and look empty.
func TestDescribeRefusesWhenTheCredentialNamesNoTypes(t *testing.T) {
	rec := &recorder{}
	cs := connect(t, rec, "not-a-jwt")

	res := call(t, cs, "cms_describe", map[string]any{})
	require.True(t, res.IsError)
	assert.Contains(t, textOf(t, res), "CMS_MCP_SCOPE_UNREADABLE")
	assert.Contains(t, textOf(t, res), "pass the type argument explicitly")

	paths, _ := rec.seen()
	assert.Empty(t, paths, "nothing should be asked of the CMS when the scope is unknown")
}

// ADR-013 §7: the page size is a token budget. Expectations are written out
// rather than derived from the registry — a clamp that asks the thing it is
// clamping agrees with itself by construction.
func TestListEntriesClampsThePageSize(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"unset falls back to the small default", map[string]any{"type": "post"}, "10"},
		{"a modest ask is honoured", map[string]any{"type": "post", "limit": 5}, "5"},
		{"at the cap", map[string]any{"type": "post", "limit": 50}, "50"},
		// Not the default: the Domain API turns an oversized limit into 20, so
		// answering the default here would make asking for 1000 return fewer
		// rows than asking for 50.
		{"over the cap lands on the cap", map[string]any{"type": "post", "limit": 1000}, "50"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			cs := connect(t, rec, agentToken(t, "post"))
			res := call(t, cs, "cms_list_entries", tc.args)
			assert.False(t, res.IsError, textOf(t, res))

			_, queries := rec.seen()
			require.Len(t, queries, 1)
			assert.Equal(t, tc.want, queries[0].Get("limit"))
		})
	}
}

func TestListEntriesForwardsEveryNarrowingParameter(t *testing.T) {
	rec := &recorder{}
	cs := connect(t, rec, agentToken(t, "post"))

	res := call(t, cs, "cms_list_entries", map[string]any{
		"type":   "post",
		"filter": []any{"title:contains:go", "status:eq:draft"},
		"fields": []any{"title", "body"},
		"sort":   "title:asc",
		"status": "draft",
		"locale": "en",
		"offset": 20,
	})
	assert.False(t, res.IsError, textOf(t, res))

	_, queries := rec.seen()
	require.Len(t, queries, 1)
	q := queries[0]
	assert.Equal(t, "post", q.Get("type"))
	// Repeated ?filter= and ?fields= are the spellings the handler reads.
	assert.Equal(t, []string{"title:contains:go", "status:eq:draft"}, q["filter"])
	assert.Equal(t, []string{"title", "body"}, q["fields"])
	assert.Equal(t, "title:asc", q.Get("sort"))
	assert.Equal(t, "draft", q.Get("status"))
	assert.Equal(t, "en", q.Get("locale"))
	assert.Equal(t, "20", q.Get("offset"))
}

func TestEntryToolsCarryTheTypeAndID(t *testing.T) {
	id := uuid.NewString()
	for _, tc := range []struct {
		tool string
		path string
	}{
		{"cms_get_entry", "/api/v1/content/entries/" + id},
		{"cms_list_translations", "/api/v1/content/entries/" + id + "/translations"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			rec := &recorder{}
			cs := connect(t, rec, agentToken(t, "post"))
			res := call(t, cs, tc.tool, map[string]any{"type": "post", "id": id})
			assert.False(t, res.IsError, textOf(t, res))

			paths, queries := rec.seen()
			assert.Equal(t, []string{tc.path}, paths)
			// Every entry path requires ?type= (handler.requireType); omitting
			// it is a 400 that looks like a malformed tool call.
			assert.Equal(t, "post", queries[0].Get("type"))
		})
	}
}

// ADR-013 §8. The details map is the whole point: it carries the allowed
// operators, the enum values, the "add a field, move the values, drop the old
// one" hint. An agent handed those repairs its own call; an agent handed a
// sentence guesses again.
func TestUpstreamErrorsReachTheAgentStructuredNotFlattened(t *testing.T) {
	rec := &recorder{
		status: http.StatusBadRequest,
		body: `{"data":null,"error":{"code":"CONTENT_FILTER_OP_UNSUPPORTED_FOR_FIELD",` +
			`"message":"operator not supported for this field",` +
			`"details":{"field":"body","op":"gt","supported":["eq","neq"]}},"meta":{}}`,
	}
	cs := connect(t, rec, agentToken(t, "post"))

	res := call(t, cs, "cms_list_entries", map[string]any{"type": "post"})
	require.True(t, res.IsError)

	var got upstream.APIError
	require.NoError(t, json.Unmarshal([]byte(textOf(t, res)), &got),
		"the error must still be JSON, not a sentence")
	assert.Equal(t, "CONTENT_FILTER_OP_UNSUPPORTED_FOR_FIELD", got.Code)
	assert.Equal(t, "operator not supported for this field", got.Message)
	assert.Equal(t, "body", got.Details["field"])
	assert.Equal(t, []any{"eq", "neq"}, got.Details["supported"],
		"the repair the agent needs lives in details")
}

// A 403 is the shape an agent meets constantly (a type outside its whitelist),
// so it must arrive as a readable refusal rather than as a protocol failure the
// model never sees.
func TestScopeRefusalsArriveAsToolErrorsNotProtocolErrors(t *testing.T) {
	rec := &recorder{
		status: http.StatusForbidden,
		body: `{"data":null,"error":{"code":"CONTENT_AGENT_TYPE_FORBIDDEN",` +
			`"message":"this credential may not touch that content type"},"meta":{}}`,
	}
	cs := connect(t, rec, agentToken(t, "post"))

	// CallTool itself must not fail — a protocol-level error is invisible to
	// the model, which is exactly who has to act on this one.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "cms_get_entry",
		Arguments: map[string]any{"type": "secret", "id": uuid.NewString()},
	})
	require.NoError(t, err)
	require.True(t, res.IsError)
	assert.Contains(t, textOf(t, res), "CONTENT_AGENT_TYPE_FORBIDDEN")
}

// A describe that quietly drops an unreachable type tells the agent that type
// does not exist. That is a worse answer than an error, and it is the failure
// the loop's early return exists to prevent.
func TestDescribeFailsRatherThanOmittingAnUnreachableType(t *testing.T) {
	rec := &recorder{
		status: http.StatusForbidden,
		body:   `{"data":null,"error":{"code":"CONTENT_AGENT_TYPE_FORBIDDEN","message":"nope"},"meta":{}}`,
	}
	cs := connect(t, rec, agentToken(t, "post", "note"))

	res := call(t, cs, "cms_describe", map[string]any{})
	require.True(t, res.IsError)
	assert.Contains(t, textOf(t, res), "CONTENT_AGENT_TYPE_FORBIDDEN")
	assert.False(t, strings.HasPrefix(textOf(t, res), "["),
		"a partial list must not be passed off as the answer")
}

// When the CMS was never reached there is no envelope to forward, and dressing
// a dial failure in a CONTENT_ code would misreport where it happened.
func TestTransportFailuresAreNamedAsSuch(t *testing.T) {
	api := httptest.NewServer(&recorder{})
	api.Close() // nothing is listening

	up := upstream.NewClient(api.URL, "", 2*time.Second)
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	NewRegistry(up, agentToken(t, "post"), 10, 50).Install(srv)
	ct, st := mcp.NewInMemoryTransports()
	_, err := srv.Connect(context.Background(), st, nil)
	require.NoError(t, err)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).
		Connect(context.Background(), ct, nil)
	require.NoError(t, err)
	defer func() { _ = cs.Close() }()

	res := call(t, cs, "cms_list_entries", map[string]any{"type": "post"})
	require.True(t, res.IsError)
	assert.Contains(t, textOf(t, res), "CMS_MCP_UPSTREAM_UNREACHABLE")
}

// --- write tools (ADR-013 step 7) -------------------------------------------

// sentAt returns everything the recorder saw about request i. The write tools
// need the verb and the body, which seen() does not carry.
func (rec *recorder) sentAt(i int) (method, path, body string, q url.Values, h http.Header) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.methods[i], rec.paths[i], rec.bodies[i], rec.queries[i], rec.headers[i]
}

func TestCreateEntryPostsThePayloadAndCarriesTheKeyAsAHeader(t *testing.T) {
	rec := &recorder{}
	cs := connect(t, rec, agentToken(t, "post"))

	res := call(t, cs, "cms_create_entry", map[string]any{
		"type":            "post",
		"payload":         map[string]any{"title": "hello"},
		"idempotency_key": "retry-key-0001",
	})
	require.False(t, res.IsError, textOf(t, res))

	method, path, body, q, h := rec.sentAt(0)
	assert.Equal(t, http.MethodPost, method)
	assert.Equal(t, "/api/v1/content/entries", path)
	assert.Equal(t, "post", q.Get("type"))
	assert.JSONEq(t, `{"title":"hello"}`, body, "the payload must go out as the request body")
	// A header, not a query parameter: that is where the CMS handler reads it,
	// and a key sent in the query is silently ignored — the create succeeds and
	// the promise simply does not exist.
	assert.Equal(t, "retry-key-0001", h.Get("Idempotency-Key"))
	assert.Empty(t, q.Get("idempotency_key"))
}

func TestCreateEntryWithoutAKeySendsNoHeader(t *testing.T) {
	rec := &recorder{}
	cs := connect(t, rec, agentToken(t, "post"))

	res := call(t, cs, "cms_create_entry", map[string]any{
		"type": "post", "payload": map[string]any{"title": "hello"},
	})
	require.False(t, res.IsError, textOf(t, res))

	_, _, _, _, h := rec.sentAt(0)
	assert.Empty(t, h.Get("Idempotency-Key"), "an empty key must not go out as an empty header")
}

func TestCreateEntryPassesLocaleAndTranslationOf(t *testing.T) {
	rec := &recorder{}
	cs := connect(t, rec, agentToken(t, "post"))
	src := uuid.NewString()

	res := call(t, cs, "cms_create_entry", map[string]any{
		"type": "post", "payload": map[string]any{"title": "bonjour"},
		"locale": "fr", "translation_of": src,
	})
	require.False(t, res.IsError, textOf(t, res))

	_, _, _, q, _ := rec.sentAt(0)
	assert.Equal(t, "fr", q.Get("locale"))
	assert.Equal(t, src, q.Get("translation_of"))
}

// The version is the optimistic lock. Sending the request without it would turn
// every agent write into a last-writer-wins overwrite of whoever wrote in
// between — the exact failure the CMS's expectedVersion exists to refuse.
func TestUpdateEntrySendsPatchWithTheVersion(t *testing.T) {
	rec := &recorder{}
	cs := connect(t, rec, agentToken(t, "post"))
	id := uuid.NewString()

	res := call(t, cs, "cms_update_entry", map[string]any{
		"type": "post", "id": id, "version": 7,
		"payload": map[string]any{"title": "edited"},
	})
	require.False(t, res.IsError, textOf(t, res))

	method, path, body, q, h := rec.sentAt(0)
	assert.Equal(t, http.MethodPatch, method)
	assert.Equal(t, "/api/v1/content/entries/"+id, path)
	assert.Equal(t, "post", q.Get("type"))
	assert.JSONEq(t, `{"title":"edited"}`, body)
	// If-Match, not ?version=. This assertion started life as the query
	// parameter and passed — the CMS reads the header, ignores the unknown
	// query key, and treats the absent version as "no check", so the lock was
	// gone entirely while this test stayed green. The e2e caught it; this line
	// is what stops it coming back.
	assert.Equal(t, "7", h.Get("If-Match"))
	assert.Empty(t, q.Get("version"))
}

// version is required, and a zero must not travel as "no check".
func TestUpdateEntryRefusesAMissingVersion(t *testing.T) {
	rec := &recorder{}
	cs := connect(t, rec, agentToken(t, "post"))

	res := call(t, cs, "cms_update_entry", map[string]any{
		"type": "post", "id": uuid.NewString(), "version": 0,
		"payload": map[string]any{"title": "edited"},
	})
	require.True(t, res.IsError, "a versionless update must not be attempted")
	assert.Contains(t, textOf(t, res), "CMS_MCP_VERSION_REQUIRED")

	paths, _ := rec.seen()
	assert.Empty(t, paths, "nothing should have gone to the CMS")
}

func TestSetStatusUnpublishes(t *testing.T) {
	rec := &recorder{}
	cs := connect(t, rec, agentToken(t, "post"))
	id := uuid.NewString()

	res := call(t, cs, "cms_set_status", map[string]any{"type": "post", "id": id})
	require.False(t, res.IsError, textOf(t, res))

	method, path, _, q, _ := rec.sentAt(0)
	assert.Equal(t, http.MethodPost, method)
	assert.Equal(t, "/api/v1/content/entries/"+id+"/unpublish", path)
	assert.Equal(t, "post", q.Get("type"))
}

func TestSetStatusAcceptsTheStatusItCanActuallySet(t *testing.T) {
	rec := &recorder{}
	cs := connect(t, rec, agentToken(t, "post"))

	res := call(t, cs, "cms_set_status", map[string]any{
		"type": "post", "id": uuid.NewString(), "status": "unpublished",
	})
	require.False(t, res.IsError, textOf(t, res))
	_, path, _, _, _ := rec.sentAt(0)
	assert.Contains(t, path, "/unpublish")
}

// Asking to publish is answered here, in the words that name the repair. This
// is NOT the gate — agent_gate.go refuses content:publish to an agent calling
// the endpoint directly, with no tool in the path. What this buys is that the
// model is told "a person releases" instead of "403 on content:publish", which
// reads as a permission to go asking for.
func TestSetStatusRefusesPublishAndNeverCallsUpstream(t *testing.T) {
	rec := &recorder{}
	cs := connect(t, rec, agentToken(t, "post"))

	res := call(t, cs, "cms_set_status", map[string]any{
		"type": "post", "id": uuid.NewString(), "status": "published",
	})
	require.True(t, res.IsError, "publishing must not be reported as done")
	assert.Contains(t, textOf(t, res), "CMS_MCP_STATUS_UNSUPPORTED")

	paths, _ := rec.seen()
	assert.Empty(t, paths, "nothing should have gone to the CMS at all")
}
