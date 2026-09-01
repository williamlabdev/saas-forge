// Package e2e_test runs ADR-013 step 6 end to end: a real MCP client, the real
// MCP server, real HTTP, the real router and authorizer, a real agent
// credential minted the production way, and a real database.
//
// WHY IT LIVES HERE AND NOT IN test/e2e. The server's tools and upstream client
// are under apps/cmsmcp/internal, which Go makes importable only from within
// apps/cmsmcp — the repository's main e2e suite structurally cannot run this
// server in-process. apps/delivery/test/e2e exists for the same reason and this
// harness is deliberately its twin, down to the applier and the boot check, so
// "how a schema gets built" keeps having one implementation (ADR-012).
//
// WHAT THIS PROVES THAT THE UNIT TESTS CANNOT. Those point the tools at a
// stand-in Domain API, which answers what went out on the wire but not whether
// the CMS accepts it. Verification plan item 8 requires the real assembly here
// because of what a hand-built second wiring already cost once: an e2e stood up
// its own worker without the content webhook provider, every event
// dead-lettered, and the suite stayed green because the copy was internally
// consistent with itself.
package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/williamlabdev/saas-forge/apps/cmsmcp/internal/tools"
	"github.com/williamlabdev/saas-forge/apps/cmsmcp/internal/upstream"
	"github.com/williamlabdev/saas-forge/internal"
	"github.com/williamlabdev/saas-forge/internal/pkg/config"
	"github.com/williamlabdev/saas-forge/internal/platform"
	"github.com/williamlabdev/saas-forge/internal/platform/migrate"
)

var (
	domainURL string
	e2eOn     bool
)

var mainKey = bytes.Repeat([]byte{3}, 32)

func dockerAvailable() bool {
	cmd := exec.Command("docker", "info")
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Run() == nil
}

func TestMain(m *testing.M) {
	ctx := context.Background()
	if os.Getenv("SKIP_E2E") == "1" || !dockerAvailable() {
		if !dockerAvailable() {
			fmt.Fprintln(os.Stderr, "cms-mcp e2e skipped: Docker not available")
		}
		os.Exit(m.Run())
	}

	container, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("e2e"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cms-mcp e2e skipped (postgres): %v\n", err)
		os.Exit(m.Run())
	}
	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		panic(err)
	}
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		_ = container.Terminate(ctx)
		panic(err)
	}
	ms, err := migrate.Discover(internal.MigrationsFS)
	if err != nil {
		pool.Close()
		_ = container.Terminate(ctx)
		panic(err)
	}
	if _, err := migrate.Up(ctx, pool, ms); err != nil {
		pool.Close()
		_ = container.Terminate(ctx)
		panic(err)
	}

	app, err := platform.BuildApp(ctx,
		config.User{
			DatabaseURL:      connStr,
			HTTPAddr:         ":0",
			EncryptionKey:    bytes.Repeat([]byte{1}, 32),
			BlindIndexPepper: bytes.Repeat([]byte{2}, 32),
		},
		config.Runtime{
			AuthzMode:           "rbac",
			JWTSecret:           mainKey,
			DeliveryJWTSecret:   bytes.Repeat([]byte{4}, 32),
			JWTAccessTTL:        15 * time.Minute,
			JWTRefreshTTL:       24 * time.Hour,
			AuthLoginRateLimit:  100,
			AuthLoginRateWindow: time.Minute,
		})
	if err != nil {
		pool.Close()
		_ = container.Terminate(ctx)
		panic(err)
	}

	domain := httptest.NewServer(app.Handler)
	// No signer here any more: agent credentials are minted through the Domain
	// API's own endpoint (see mintAgent), which is both the production path and
	// the only path that produces a token the middleware will honour.
	domainURL, e2eOn = domain.URL, true
	code := m.Run()

	domain.Close()
	app.Pool.Close()
	pool.Close()
	_ = container.Terminate(ctx)
	os.Exit(code)
}

func requireE2E(t *testing.T) {
	t.Helper()
	if !e2eOn {
		t.Skip("e2e requires Docker; set SKIP_E2E=1 to silence")
	}
}

// mcpFront puts the MCP server in front of the SAME handler the platform
// assembles, over a real listener — the server is an HTTP client, and an
// in-process recorder has no address to give it.
func mcpFront(t *testing.T, token string) *mcp.ClientSession {
	t.Helper()
	up := upstream.NewClient(domainURL, "", 10*time.Second)
	srv := mcp.NewServer(&mcp.Implementation{Name: "cms-mcp-e2e", Version: "0"}, nil)
	tools.NewRegistry(up, token, 10, 50).Install(srv)

	ct, st := mcp.NewInMemoryTransports()
	_, err := srv.Connect(context.Background(), st, nil)
	require.NoError(t, err)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "0"}, nil).
		Connect(context.Background(), ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func mcpCall(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	require.Len(t, res.Content, 1)
	tc, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	return res, tc.Text
}

func TestE2E_MCPServerReadsThroughARealAgentCredential(t *testing.T) {
	requireE2E(t)

	token := registerAndLogin(t, "mcpread")

	domainPost(t, "/api/v1/content/types", token,
		`{"name":"memo","label":"M","fields":[`+
			`{"key":"title","type":"text","label":"T","required":true},`+
			`{"key":"tags","type":"string","label":"G","multiple":true}]}`,
		http.StatusCreated)
	domainPost(t, "/api/v1/content/types", token,
		`{"name":"secret","label":"S","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		http.StatusCreated)

	for _, title := range []string{"alpha", "bravo", "charlie"} {
		domainPost(t, "/api/v1/content/entries?type=memo", token,
			fmt.Sprintf(`{"title":%q}`, title), http.StatusCreated)
	}

	agent := mintAgent(t, token, "reader-bot", []string{"memo"})
	cs := mcpFront(t, agent)

	t.Run("describe walks the credential's own scope", func(t *testing.T) {
		res, body := mcpCall(t, cs, "cms_describe", map[string]any{})
		require.False(t, res.IsError, body)

		var types []struct {
			Name   string `json:"name"`
			Fields []struct {
				Key       string   `json:"key"`
				Supported []string `json:"supported"`
			} `json:"fields"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &types))

		// Exactly the whitelisted type. `secret` exists and belongs to the same
		// tenant, so its absence is the whitelist working rather than an empty
		// database — and the walk never asked for the type list, which an agent
		// credential is refused by construction.
		require.Len(t, types, 1)
		assert.Equal(t, "memo", types[0].Name)

		// ADR-013 §6's actual deliverable: an agent learns which operators a
		// field accepts WITHOUT first sending a filter that gets a 400. Before
		// step 4 this list existed only inside that error's details.
		byKey := map[string][]string{}
		for _, f := range types[0].Fields {
			byKey[f.Key] = f.Supported
		}
		assert.Contains(t, byKey["title"], "contains")
		assert.NotEmpty(t, byKey["tags"], "a multi-valued field still accepts operators")
	})

	t.Run("listing and projection reach the real query path", func(t *testing.T) {
		res, body := mcpCall(t, cs, "cms_list_entries", map[string]any{
			"type":   "memo",
			"fields": []any{"title"},
			"sort":   "title:asc",
		})
		require.False(t, res.IsError, body)

		var page struct {
			Items []struct {
				ID   string          `json:"id"`
				Data json.RawMessage `json:"data"`
			} `json:"items"`
			Limit int `json:"limit"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &page))
		require.Len(t, page.Items, 3)

		// The page size the agent never asked for is the one ADR-013 §7 chose,
		// and it survived the round trip rather than being replaced by the
		// API's human-sized default of 20.
		assert.Equal(t, 10, page.Limit)

		var data map[string]any
		require.NoError(t, json.Unmarshal(page.Items[0].Data, &data))
		assert.Equal(t, []string{"title"}, keysOf(data),
			"projection must narrow the payload the model pays for")

		// The tools compose the way an agent would use them: an id from the
		// listing is what the next call fetches.
		res, body = mcpCall(t, cs, "cms_get_entry", map[string]any{
			"type": "memo", "id": page.Items[0].ID,
		})
		require.False(t, res.IsError, body)
		var entry map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &entry))
		assert.Equal(t, page.Items[0].ID, entry["id"])
	})

	// The refusal an agent meets most often. It must arrive as a readable tool
	// error carrying the CMS's own code — not as a protocol failure the model
	// never sees, and not flattened into a sentence.
	t.Run("a type outside the whitelist is refused with the code intact", func(t *testing.T) {
		res, body := mcpCall(t, cs, "cms_describe", map[string]any{"type": "secret"})
		require.True(t, res.IsError, body)

		var apiErr upstream.APIError
		require.NoError(t, json.Unmarshal([]byte(body), &apiErr),
			"the agent must receive JSON it can act on")
		assert.Contains(t, apiErr.Code, "CONTENT_AGENT",
			"the refusal is the CMS's, reported verbatim: %s", body)
	})

	// The dominating rule, from the other side. This server exposes no write
	// tool — but that is not what stops a write, so the credential THIS MCP
	// session is holding is sent straight at an endpoint no tool calls. If it
	// ever returns 2xx, the tool list has quietly become the security boundary,
	// which is the error the first version of ADR-013 made.
	//
	// test/e2e/agent_credential_test.go proves verification item 4 against a
	// freshly minted credential; this one proves it about the credential an
	// agent is using right now, in the same test, through the same session.
	//
	// The artifact is WELL FORMED on purpose. The handler validates the
	// envelope before the service authorizes, so a malformed body answers 400
	// and this assertion would fail — or, with the wrong expectation, pass —
	// without the authorizer ever running. Verified by watching it happen.
	t.Run("skipping MCP entirely changes nothing", func(t *testing.T) {
		validArtifact := `{"kind":"content-schema","artifact_version":"1","types":[]}`
		req, err := http.NewRequest(http.MethodPost,
			domainURL+"/api/v1/content/schema/apply", bytes.NewReader([]byte(validArtifact)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+agent)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(raw))
	})
}

// mintAgent posts to the Domain API's minting endpoint, so the credential this
// suite drives is one the platform actually issued — signed by the real key AND
// recorded in the agent_credentials row that keeps it alive. Calling the signer
// here would now produce a token the middleware refuses (no such row), and
// making that work would mean this file writing the row itself: a second copy
// of the minting path, free to drift from the one production uses.
func mintAgent(t *testing.T, humanToken, agentID string, allowedTypes []string) string {
	t.Helper()
	types, err := json.Marshal(allowedTypes)
	require.NoError(t, err)
	// tenant_role is REQUIRED since 補裁 S-1 (2026-08-20) — the endpoint no
	// longer copies the minter's. admin, because that is what these credentials
	// carried when the role was copied off an owner, and because an editor
	// agent holds no schema verb.
	//
	// ⚠️ This helper is a SECOND hand-built copy of the minting call — the one
	// in test/e2e is the other — and the field was added to that one first,
	// leaving this package red on CI while every local suite stayed green.
	minted := domainPost(t, "/api/v1/auth/agent-tokens", humanToken,
		fmt.Sprintf(`{"agent_id":%q,"tenant_role":"admin","allowed_types":%s}`, agentID, types), http.StatusCreated)
	return minted["token"].(string)
}

func registerAndLogin(t *testing.T, prefix string) string {
	t.Helper()
	email := fmt.Sprintf("%s-%s@example.com", prefix, uuid.NewString()[:8])
	domainPost(t, "/api/v1/users", "",
		fmt.Sprintf(`{"username":%q,"email":%q,"password":"password12"}`, prefix, email),
		http.StatusCreated)
	login := domainPost(t, "/api/v1/auth/login", "",
		fmt.Sprintf(`{"email":%q,"password":"password12"}`, email), http.StatusOK)
	return login["access_token"].(string)
}

func domainPost(t *testing.T, path, bearer, body string, want int) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, domainURL+path, bytes.NewReader([]byte(body)))
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, want, resp.StatusCode, "%s: %s", path, raw)

	var env struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &env), string(raw))
	return env.Data
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The write tools against the real Domain API (ADR-013 step 7).
//
// The tools' unit tests drive a RECORDER, so they prove what went out on the
// wire and nothing about whether the CMS reads it. Every parameter name is a
// silent-failure surface: ?version= read under another name means the
// optimistic lock is simply absent and every agent write becomes a
// last-writer-wins overwrite, and the recorder test passes either way.
// Idempotency-Key is the sharpest of them — sent where nobody reads it, the
// create still succeeds and only the PROMISE quietly does not exist.
func TestE2E_MCPServerWritesThroughARealAgentCredential(t *testing.T) {
	requireE2E(t)

	token := registerAndLogin(t, "mcpwrite")
	domainPost(t, "/api/v1/content/types", token,
		`{"name":"note","label":"N","fields":[{"key":"title","type":"text","label":"T","required":true}]}`,
		http.StatusCreated)

	agent := mintAgent(t, token, "writer-bot", []string{"note"})
	cs := mcpFront(t, agent)

	var createdID string
	const key = "e2e-retry-key-0001"

	t.Run("a create reaches the real write path", func(t *testing.T) {
		res, body := mcpCall(t, cs, "cms_create_entry", map[string]any{
			"type":            "note",
			"payload":         map[string]any{"title": "first"},
			"idempotency_key": key,
		})
		require.False(t, res.IsError, body)

		var entry map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &entry))
		createdID, _ = entry["id"].(string)
		require.NotEmpty(t, createdID, "no id came back: %s", body)
		// New entries are drafts. If this ever comes back published, the release
		// gate has been bypassed by the creation path itself (ADR-014 §1).
		assert.Equal(t, "draft", entry["status"])
	})

	t.Run("the same key and the same request replays instead of duplicating", func(t *testing.T) {
		res, body := mcpCall(t, cs, "cms_create_entry", map[string]any{
			"type":            "note",
			"payload":         map[string]any{"title": "first"},
			"idempotency_key": key,
		})
		require.False(t, res.IsError, body)

		var entry map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &entry))
		// THIS is the assertion the recorder tests cannot make: it is only equal
		// if the header travelled, the handler read it, and the row was written.
		assert.Equal(t, createdID, entry["id"],
			"the retry created a second entry — the key did not survive the trip")
	})

	t.Run("the same key with different content is refused, not silently replayed", func(t *testing.T) {
		res, body := mcpCall(t, cs, "cms_create_entry", map[string]any{
			"type":            "note",
			"payload":         map[string]any{"title": "SECOND"},
			"idempotency_key": key,
		})
		require.True(t, res.IsError, "a reused key with new content must not report success: %s", body)
		assert.Contains(t, body, "CONTENT_IDEMPOTENCY_KEY_REUSED")
	})

	t.Run("an update carries the version, and a stale one is refused", func(t *testing.T) {
		res, body := mcpCall(t, cs, "cms_update_entry", map[string]any{
			"type": "note", "id": createdID, "version": 1,
			"payload": map[string]any{"title": "edited"},
		})
		require.False(t, res.IsError, body)

		// The same version again is now stale. A refusal here is what proves
		// ?version= is being read: without it this second call succeeds.
		res, body = mcpCall(t, cs, "cms_update_entry", map[string]any{
			"type": "note", "id": createdID, "version": 1,
			"payload": map[string]any{"title": "overwrite"},
		})
		require.True(t, res.IsError, "a stale version was accepted: %s", body)
		assert.Contains(t, body, "CONTENT_VERSION_CONFLICT")
	})

	t.Run("an agent can retract what a person released", func(t *testing.T) {
		// A PERSON publishes — the agent cannot, which is the whole gate.
		domainPost(t, "/api/v1/content/entries/"+createdID+"/publish?type=note", token, "", http.StatusOK)

		res, body := mcpCall(t, cs, "cms_set_status", map[string]any{"type": "note", "id": createdID})
		require.False(t, res.IsError, body)

		var entry map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &entry))
		assert.NotEqual(t, "published", entry["status"],
			"retract is the one status change an agent has, and it must actually take effect")
	})

	t.Run("asking to publish is refused and never reaches the CMS", func(t *testing.T) {
		res, body := mcpCall(t, cs, "cms_set_status", map[string]any{
			"type": "note", "id": createdID, "status": "published",
		})
		require.True(t, res.IsError, "publishing must not be reported as done: %s", body)
		assert.Contains(t, body, "CMS_MCP_STATUS_UNSUPPORTED")
	})
}
