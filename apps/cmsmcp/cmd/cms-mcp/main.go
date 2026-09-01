// Command cms-mcp is the CMS's Model Context Protocol server (ADR-013 step 6):
// the first slice an agent can actually connect to, with zero write tools.
//
// It is a transport adapter, not a door. It speaks MCP to the model and plain
// HTTP to the Domain API, holds no database handle and no signing key, and
// therefore cannot widen anything: every refusal an agent meets here is one the
// same credential meets speaking HTTP directly (ADR-013, dominating rule).
//
// Two transports, and the difference is whose credential is in play:
//
//   - stdio (default) — one process, one agent credential from CMS_AGENT_TOKEN,
//     launched by the MCP client. This is the local-tool shape.
//   - streamable HTTP (CMS_MCP_HTTP_ADDR) — many callers, each carrying its own
//     bearer, which this process forwards and never stores. This is the
//     deployable shape, and it is why the server can be shared without sharing
//     a credential.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/williamlabdev/saas-forge/apps/cmsmcp/internal/config"
	"github.com/williamlabdev/saas-forge/apps/cmsmcp/internal/tools"
	"github.com/williamlabdev/saas-forge/apps/cmsmcp/internal/upstream"
)

const serverName = "saas-platform-cms"

func main() {
	cfg := config.Load()
	if err := config.Validate(cfg); err != nil {
		log.Fatalf("cms-mcp: %v", err)
	}
	up := upstream.NewClient(cfg.DomainAPIURL, cfg.GatewaySecret, cfg.RequestTimeout)

	if cfg.HTTPAddr == "" {
		runStdio(cfg, up)
		return
	}
	runHTTP(cfg, up)
}

// newServer builds a server whose tools all act as ONE credential.
func newServer(cfg config.Config, up *upstream.Client, token string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: "0.1.0"}, nil)
	tools.NewRegistry(up, token, cfg.DefaultLimit, cfg.MaxLimit).Install(s)
	return s
}

func runStdio(cfg config.Config, up *upstream.Client) {
	// stdout is the transport, so every log line must go to stderr or it
	// corrupts the protocol stream.
	log.SetOutput(os.Stderr)
	log.Printf("cms-mcp: stdio transport (domain=%s)", cfg.DomainAPIURL)
	if err := newServer(cfg, up, cfg.AgentToken).Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("cms-mcp: %v", err)
	}
}

// httpHandler builds the streamable-HTTP chain. Split out of runHTTP so the
// property below can be tested without binding a port.
//
// STATELESS IS THE LOAD-BEARING OPTION, not a performance choice. The getServer
// callback is what binds a request to its caller's credential, and in the
// SDK's stateful mode it is consulted ONLY when a request arrives without a
// session id (go-sdk v1.7.0 streamable.go: `// No session ID: create a new
// session.`). Every later request carrying that session id is handed straight
// to the existing session's transport — and so acts as the bearer from the
// FIRST request, whatever Authorization it presented itself.
//
// That makes this file's own package doc false in the stateful mode ("many
// callers, each carrying its own bearer, which this process forwards and never
// stores"): a session stores one. Worse, a caller that learns a session id acts
// as the credential that opened it, across tenants — the isolation ADR-013 lists
// as an open item, and it turns out to be a property of this one option.
//
// Stateless makes the SDK build a temporary session per REQUEST, so getServer —
// and therefore bearerOf — runs on every one. The cost is that GET and DELETE
// answer 405 and the server cannot make server->client requests. This server
// makes none: it is seven read-and-write tools over plain request/response, with
// no sampling, elicitation or notifications.
func httpHandler(cfg config.Config, up *upstream.Client) http.Handler {
	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return newServer(cfg, up, bearerOf(r))
	}, &mcp.StreamableHTTPOptions{Stateless: true})
	return requireBearer(handler)
}

func runHTTP(cfg config.Config, up *upstream.Client) {
	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpHandler(cfg, up),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("cms-mcp: streamable HTTP on %s (domain=%s)", cfg.HTTPAddr, cfg.DomainAPIURL)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("cms-mcp: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}

// requireBearer refuses an unauthenticated session at the door.
//
// Without it the session would open, the tool list would be served, and the
// failure would only surface as a 401 forwarded from the CMS on the first tool
// call — an agent would read that as "this content is not available to me"
// rather than "you connected without a credential".
func requireBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bearerOf(r) == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+serverName+`"`)
			http.Error(w, "missing bearer credential", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerOf(r *http.Request) string {
	const prefix = "Bearer "
	v := r.Header.Get("Authorization")
	if len(v) < len(prefix) || !strings.EqualFold(v[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(v[len(prefix):])
}
