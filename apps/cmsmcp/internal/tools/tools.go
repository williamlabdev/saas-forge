// Package tools defines the CMS MCP server's tool surface: the four read tools
// of ADR-013 step 6 and the three write tools of step 7.
//
// The count is the point. The CMS router has 32 endpoints and a 1:1 mapping
// would spend the model's judgement on tenant administration it has no business
// doing. But the narrowing here buys UX, not safety — see the package comment
// in upstream. Everything an agent is REFUSED, it is refused by the Domain API,
// to which this server is one client among possible others.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	jwtlib "github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/williamlabdev/saas-forge/apps/cmsmcp/internal/upstream"
	authjwt "github.com/williamlabdev/saas-forge/internal/auth/jwt"
)

// Registry installs the tools onto a server for ONE credential. In stdio mode
// there is exactly one of these for the process lifetime; in HTTP mode there is
// one per request, built from that request's bearer.
type Registry struct {
	up    *upstream.Client
	token string
	// defaultLimit / maxLimit are ADR-013 §7's token budget. Clamping happens
	// here rather than in the service because it is a property of who is
	// reading (a model with a context window), not of what may be read.
	defaultLimit int
	maxLimit     int
}

func NewRegistry(up *upstream.Client, token string, defaultLimit, maxLimit int) *Registry {
	return &Registry{up: up, token: token, defaultLimit: defaultLimit, maxLimit: maxLimit}
}

type describeArgs struct {
	Type string `json:"type,omitempty" jsonschema:"one content type to describe; omit to describe every type this credential may touch"`
}

type listEntriesArgs struct {
	Type   string   `json:"type" jsonschema:"the content type to list"`
	Filter []string `json:"filter,omitempty" jsonschema:"filters as key:op:value; the operators a field accepts are in cms_describe's supported list"`
	Sort   string   `json:"sort,omitempty" jsonschema:"key:asc or key:desc"`
	Fields []string `json:"fields,omitempty" jsonschema:"narrow each entry's payload to these keys"`
	Status string   `json:"status,omitempty" jsonschema:"draft or published; omit for both"`
	Locale string   `json:"locale,omitempty" jsonschema:"restrict to one locale; omit for every locale"`
	Limit  int      `json:"limit,omitempty" jsonschema:"page size"`
	Offset int      `json:"offset,omitempty" jsonschema:"rows to skip"`
}

type entryArgs struct {
	Type string `json:"type" jsonschema:"the entry's content type"`
	ID   string `json:"id" jsonschema:"the entry id"`
}

type createEntryArgs struct {
	Type          string         `json:"type" jsonschema:"the content type to create an entry of"`
	Payload       map[string]any `json:"payload" jsonschema:"the entry's fields as a JSON object; cms_describe lists which keys this credential may write"`
	Locale        string         `json:"locale,omitempty" jsonschema:"language tag; omit for the default locale"`
	TranslationOf string         `json:"translation_of,omitempty" jsonschema:"an existing entry id whose translation group this entry joins"`
	// Optional, and the schema says why rather than just what: a model that
	// treats it as decoration is exactly the caller the column protects.
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"8-128 chars of A-Za-z0-9_-; send the SAME key when retrying so a retry returns the entry you already created instead of a duplicate"`
}

type updateEntryArgs struct {
	Type    string         `json:"type" jsonschema:"the entry's content type"`
	ID      string         `json:"id" jsonschema:"the entry id"`
	Payload map[string]any `json:"payload" jsonschema:"the keys to change, as a JSON object; keys you omit are left alone"`
	Version int            `json:"version" jsonschema:"the version you last read for this entry; the write is refused if it has changed since"`
}

type setStatusArgs struct {
	Type string `json:"type" jsonschema:"the entry's content type"`
	ID   string `json:"id" jsonschema:"the entry id"`
	// Present so a model that names the status it wants gets an answer in its
	// own vocabulary. Omitting it means unpublish, the only thing this does.
	Status string `json:"status,omitempty" jsonschema:"only \"unpublished\" is accepted; publishing is a person's decision"`
}

// Install registers the seven tools.
func (r *Registry) Install(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "cms_describe",
		Description: "Describe the content types this credential may work with: every field's key, type, " +
			"whether it is required or multi-valued, its enum values, whether this credential may read or " +
			"write it, and the filter operators it supports. Returns declarations, never content.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args describeArgs) (*mcp.CallToolResult, any, error) {
		return r.describe(ctx, args)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "cms_list_entries",
		Description: "List entries of one content type. Supports filtering, sorting, locale and status " +
			"narrowing, and field projection. Pages are small by design — ask for the fields you need.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args listEntriesArgs) (*mcp.CallToolResult, any, error) {
		return r.call(ctx, func(ctx context.Context) (json.RawMessage, error) {
			return r.up.ListEntries(ctx, r.token, upstream.ListEntriesParams{
				Type:    args.Type,
				Filters: args.Filter,
				Sort:    args.Sort,
				Fields:  args.Fields,
				Status:  args.Status,
				Locale:  args.Locale,
				Limit:   r.clampLimit(args.Limit),
				Offset:  args.Offset,
			})
		})
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "cms_get_entry",
		Description: "Fetch one entry by id, including its full payload, version and editorial status.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args entryArgs) (*mcp.CallToolResult, any, error) {
		return r.call(ctx, func(ctx context.Context) (json.RawMessage, error) {
			return r.up.GetEntry(ctx, r.token, args.Type, args.ID)
		})
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "cms_list_translations",
		Description: "List the sibling entries that are translations of one entry — every locale in its translation group.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args entryArgs) (*mcp.CallToolResult, any, error) {
		return r.call(ctx, func(ctx context.Context) (json.RawMessage, error) {
			return r.up.ListTranslations(ctx, r.token, args.Type, args.ID)
		})
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "cms_create_entry",
		Description: "Create a new entry. It is created as a DRAFT and is not visible to the public until " +
			"a person publishes it. Pass idempotency_key and reuse it if you retry: the same key with the " +
			"same content returns the entry you already created instead of a second one, and the same key " +
			"with different content is refused so a retry cannot silently overwrite what you meant to send.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args createEntryArgs) (*mcp.CallToolResult, any, error) {
		payload, err := marshalPayload(args.Payload)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return r.call(ctx, func(ctx context.Context) (json.RawMessage, error) {
			return r.up.CreateEntry(ctx, r.token, upstream.CreateEntryParams{
				Type:           args.Type,
				Payload:        payload,
				Locale:         args.Locale,
				TranslationOf:  args.TranslationOf,
				IdempotencyKey: args.IdempotencyKey,
			})
		})
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "cms_update_entry",
		Description: "Change an entry's working copy. Requires the version you last read: if someone else " +
			"has written since, the call is refused rather than overwriting them — re-read the entry with " +
			"cms_get_entry and decide again. Changes to an already-published entry do NOT reach the public " +
			"until a person publishes them.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args updateEntryArgs) (*mcp.CallToolResult, any, error) {
		if args.Version <= 0 {
			// The schema marks version required, but "required" is the SDK's
			// answer to a missing key, not to a zero. A zero reaching the CMS
			// means no version check at all, which is the one outcome this tool
			// must never produce quietly — so it is refused here, with the repair
			// named rather than left as a 400 about a header the model never set.
			return errorResult(&upstream.APIError{
				Code:    "CMS_MCP_VERSION_REQUIRED",
				Message: "version is required and must be the version you last read for this entry",
				Details: map[string]any{"hint": "call cms_get_entry first and pass the version it returns"},
			}), nil, nil
		}
		payload, err := marshalPayload(args.Payload)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return r.call(ctx, func(ctx context.Context) (json.RawMessage, error) {
			return r.up.UpdateEntry(ctx, r.token, upstream.UpdateEntryParams{
				Type:    args.Type,
				ID:      args.ID,
				Payload: payload,
				Version: args.Version,
			})
		})
	})

	// cms_set_status carries the name from ADR-013 §5's table and only half of
	// what that name suggests: ADR-014 §1 took publish back, so the one status
	// this reaches is "not published".
	//
	// The name is kept rather than narrowed to cms_unpublish because the tool
	// list is UX, not authorization (ADR-013's dominating rule) — renaming it
	// would suggest the gate lives in this file's vocabulary. It lives in
	// agent_gate.go, which refuses content:publish to an agent calling the HTTP
	// endpoint directly with no tool in the path at all.
	mcp.AddTool(s, &mcp.Tool{
		Name: "cms_set_status",
		Description: "Take a published entry off the public site. The content is kept — this retracts what " +
			"the public can see, it does not delete anything. You cannot publish: releasing content is a " +
			"person's decision, so leave finished work as a draft and it will appear in their review queue.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args setStatusArgs) (*mcp.CallToolResult, any, error) {
		if args.Status != "" && args.Status != statusUnpublished {
			// Answered here rather than by letting the call go out, because what
			// comes back otherwise is a 403 on content:publish — correct, but it
			// reads as "you got the permission wrong" when the actual repair is
			// "leave it as a draft for a person". The refusal is still the
			// server's; this only says the same thing in the words that help.
			return errorResult(&upstream.APIError{
				Code:    "CMS_MCP_STATUS_UNSUPPORTED",
				Message: "this tool can only set status to " + statusUnpublished,
				Details: map[string]any{
					"requested": args.Status,
					"allowed":   []string{statusUnpublished},
					"hint":      "publishing is a person's decision; finished drafts wait in their review queue",
				},
			}), nil, nil
		}
		return r.call(ctx, func(ctx context.Context) (json.RawMessage, error) {
			return r.up.Unpublish(ctx, r.token, args.Type, args.ID)
		})
	})
}

// statusUnpublished is the only status cms_set_status accepts.
const statusUnpublished = "unpublished"

// marshalPayload turns the tool argument into the request body.
//
// The argument is a map rather than json.RawMessage, and that is not a style
// choice: the SDK infers the tool's JSON schema from this struct, and
// json.RawMessage is a []byte, which it publishes as {"type": ["null",
// "array"]}. Every model sending the object the description asks for would have
// its call rejected by the SDK's own validator before any of this code ran —
// found by the tools tests, which drive the real protocol rather than calling
// the handlers directly.
//
// A nil map marshals to "null", which the CMS rejects as a non-object. Sent as
// an empty object instead so the refusal comes from the content rules — a
// create with no fields fails on the required ones, and names them.
func marshalPayload(p map[string]any) (json.RawMessage, *upstream.APIError) {
	if p == nil {
		return json.RawMessage(`{}`), nil
	}
	body, err := json.Marshal(p)
	if err != nil {
		return nil, &upstream.APIError{
			Code:    "CMS_MCP_PAYLOAD_UNENCODABLE",
			Message: "the payload could not be encoded as JSON: " + err.Error(),
		}
	}
	return body, nil
}

// clampLimit applies the token budget. A caller that asks for more than the cap
// gets the cap, not the default: the Domain API's own clamp turns an oversized
// limit into 20, so silently returning the DEFAULT here would mean asking for
// 1000 hands back fewer rows than asking for 50 — a result no agent can reason
// about.
func (r *Registry) clampLimit(n int) int {
	switch {
	case n <= 0:
		return r.defaultLimit
	case n > r.maxLimit:
		return r.maxLimit
	default:
		return n
	}
}

// describe walks the credential's own AllowedTypes and fetches types ONE AT A
// TIME. It must not reach for GET /types: that endpoint names no single content
// type, and an agent credential is shut out of every such path by construction
// (content_service.go refuseOutsideAgentScope). Widening that refusal to make a
// type list work would silently reopen media, webhooks, usage and whole-schema
// export along with it — ADR-013 §A settled that the caller changes, not the
// refusal.
func (r *Registry) describe(ctx context.Context, args describeArgs) (*mcp.CallToolResult, any, error) {
	names := []string{args.Type}
	if args.Type == "" {
		var err error
		names, err = allowedTypes(r.token)
		if err != nil {
			return errorResult(&upstream.APIError{
				Code:    "CMS_MCP_SCOPE_UNREADABLE",
				Message: "cannot tell which content types this credential may touch: " + err.Error(),
				Details: map[string]any{"hint": "pass the type argument explicitly"},
			}), nil, nil
		}
	}

	out := make([]json.RawMessage, 0, len(names))
	for _, name := range names {
		doc, err := r.up.GetType(ctx, r.token, name)
		if err != nil {
			// One unreachable type fails the whole call rather than being
			// dropped from the list. A describe that silently omits a type
			// tells the agent that type does not exist, which is a different
			// and worse answer than an error it can act on.
			return toolError(err)
		}
		out = append(out, doc)
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(body), nil, nil
}

// call runs one upstream read and turns the outcome into a tool result.
func (r *Registry) call(ctx context.Context, fn func(context.Context) (json.RawMessage, error)) (*mcp.CallToolResult, any, error) {
	body, err := fn(ctx)
	if err != nil {
		return toolError(err)
	}
	return jsonResult(body), nil, nil
}

// toolError reports a failure INSIDE the result with IsError set, not as a Go
// error: a protocol-level error is invisible to the model, and the whole reason
// §8 insists on the structured body is that the model is the one who has to
// repair the call.
func toolError(err error) (*mcp.CallToolResult, any, error) {
	if se, ok := errors.AsType[*upstream.StatusError](err); ok {
		return errorResult(se.Body()), nil, nil
	}
	// Transport failures never reached the CMS, so there is no envelope to
	// forward and inventing a CMS-looking code would be a lie about where the
	// failure was.
	return errorResult(&upstream.APIError{
		Code:    "CMS_MCP_UPSTREAM_UNREACHABLE",
		Message: err.Error(),
	}), nil, nil
}

func errorResult(body *upstream.APIError) *mcp.CallToolResult {
	// Marshalling the error body is what makes "structured, not flattened"
	// observable to the agent; if this ever fails, the code and message still
	// travel rather than being replaced by a marshalling complaint.
	raw, err := json.Marshal(body)
	if err != nil {
		raw = fmt.Appendf(nil, `{"code":%q,"message":%q}`, body.Code, body.Message)
	}
	return &mcp.CallToolResult{
		IsError:           true,
		Content:           []mcp.Content{&mcp.TextContent{Text: string(raw)}},
		StructuredContent: body,
	}
}

func jsonResult(body json.RawMessage) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}
}

// allowedTypes reads the whitelist out of the credential WITHOUT verifying its
// signature, and that is deliberate rather than an oversight.
//
// This process holds no signing key — verifying here is impossible, and adding
// the key so it could would hand the thinnest process in the platform the
// ability to mint. Nothing is decided on the strength of what this returns: it
// only chooses which types to ASK about, and every answer is authorized again
// by the Domain API against the same claim it verified itself. A tampered token
// gets a longer list of 403s, not a wider view.
func allowedTypes(token string) ([]string, error) {
	var claims authjwt.Claims
	if _, _, err := jwtlib.NewParser().ParseUnverified(token, &claims); err != nil {
		return nil, fmt.Errorf("the credential is not a readable JWT")
	}
	if len(claims.AllowedTypes) == 0 {
		// Either a human credential (which has no whitelist and would make
		// every ADR-013 narrowing inert — see config.AgentToken) or an agent
		// credential minted without a scope, which the Domain API refuses
		// outright. Both need the operator, not a guess.
		return nil, errors.New("it names no allowed content types")
	}
	return claims.AllowedTypes, nil
}
