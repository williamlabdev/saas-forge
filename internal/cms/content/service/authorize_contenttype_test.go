package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The contentType argument of authorize() is what §4's per-credential
// AllowedTypes check reads. A call site that passes the wrong value is not a
// failing test somewhere — it is a credential reaching a type it was not scoped
// to, or a path that works for nobody.
//
// The invariant is total and structural:
//
//	a method that receives a typeName MUST forward it;
//	a method that does not MUST pass "".
//
// The expectation comes from the FUNCTION SIGNATURE, never from the call being
// checked, so this cannot go green by agreeing with whatever the code happens
// to do. The second half is what makes media, webhooks, usage and whole-schema
// artifacts closed to a type-scoped credential by construction rather than by
// somebody remembering to list them.
func TestAuthorizeForwardsContentTypeWhereverItExists(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		files = append(files, f)
	}
	require.NotEmpty(t, files, "no source files parsed — the walk below would vacuously pass")

	seen, seenArtifact := 0, 0
	{
		for _, file := range files {
			ast.Inspect(file, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok {
					return true
				}
				// Skip authorize's own declaration — it names the parameter, it
				// does not call itself.
				if fn.Name.Name == "authorize" {
					return false
				}
				hasArtifact := false
				if fn.Type.Params != nil {
					for _, p := range fn.Type.Params.List {
						if sel, ok := p.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Artifact" {
							for _, nm := range p.Names {
								if nm.Name == "art" {
									hasArtifact = true
								}
							}
						}
					}
				}
				ast.Inspect(fn.Body, func(m ast.Node) bool {
					call, ok := m.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "authorizeArtifact" {
						return true
					}
					seenArtifact++
					// The artifact half of the same invariant (補裁 E): the
					// document is where this path's content types are, so the
					// document itself must be what reaches the gate. A caller
					// that built a fresh or filtered artifact here would be
					// authorizing something other than what it is about to plan.
					require.Len(t, call.Args, 4,
						"%s: authorizeArtifact takes (ctx, action, resourceID, art)", fn.Name.Name)
					require.True(t, hasArtifact,
						"%s (%s): calls authorizeArtifact without an artifact named art in scope",
						fn.Name.Name, fset.Position(call.Pos()))
					id, ok := call.Args[3].(*ast.Ident)
					require.True(t, ok && id.Name == "art",
						"%s (%s): must gate on the artifact it received, not on a substitute",
						fn.Name.Name, fset.Position(call.Pos()))
					return true
				})

				hasTypeName := false
				if fn.Type.Params != nil {
					for _, p := range fn.Type.Params.List {
						for _, nm := range p.Names {
							if nm.Name == "typeName" {
								hasTypeName = true
							}
						}
					}
				}
				ast.Inspect(fn.Body, func(m ast.Node) bool {
					call, ok := m.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "authorize" {
						return true
					}
					seen++
					require.Len(t, call.Args, 4,
						"%s: authorize takes (ctx, action, resourceID, contentType)", fn.Name.Name)
					arg := call.Args[3]
					pos := fset.Position(call.Pos())
					if hasTypeName {
						id, ok := arg.(*ast.Ident)
						require.True(t, ok && id.Name == "typeName",
							"%s (%s): receives typeName but does not forward it to authorize", fn.Name.Name, pos)
					} else {
						lit, ok := arg.(*ast.BasicLit)
						require.True(t, ok && lit.Value == `""`,
							"%s (%s): has no content type in scope, so authorize must get \"\" — "+
								"anything else claims a type this path cannot know", fn.Name.Name, pos)
					}
					return true
				})
				return false
			})
		}
	}

	// Hard-coded, not asked of the thing under test: 33 call sites when the
	// parameter was introduced, 32 since PlanSchema moved to authorizeArtifact
	// (ADR-013 補裁 E), 33 again with ListActivity (ADR-014 §3), which passes ""
	// on purpose — the stream spans every type so it can name none, and that is
	// what keeps an agent out of it. 34 with EntryFieldAttribution (§6 step 4),
	// which does the OPPOSITE and names its type: it answers about one entry, so
	// an agent reaching it is refused or allowed by that type's own read rule
	// rather than by construction. A bare invariant would also pass over an
	// empty set — if a refactor moves authorize behind a helper, this number
	// drops and the guard stops guarding silently. Which is precisely what it
	// caught that day: the count fell by one and the missing call site was the
	// one being rewritten.
	//
	// 35 with ListPendingReview (§2 step 7), which passes "" for ListActivity's
	// reason and not merely by analogy with it: the release queue spans every
	// type in the tenant, so it can name none, and that is the whole mechanism
	// refusing an agent the list of what its own work is waiting on.
	// 39 with the four approver-side proposal methods (ADR-013 §3 step 8):
	// ListSchemaProposals, GetSchemaProposal, ApproveSchemaProposal and
	// RejectSchemaProposal. All four pass "" and must — a proposal names a whole
	// schema, so there is no single type any of them could claim, and the ""
	// they pass is the same untyped call ApplySchema makes and the same one §4
	// refuses to every agent. That refusal is not incidental here: it is what
	// keeps the queue, and the approve button, away from the credentials that
	// file into it.
	require.Equal(t, 39, seen, "authorize call-site count changed; re-check the new sites and update this number")
	// 2 with ProposeSchema, which gates against the DOCUMENT for PlanSchema's
	// reason (補裁 E): the types are in the artifact, so an agent's whitelist is
	// enforced against them. Passing "" here instead would refuse every agent
	// the propose verb was created for.
	require.Equal(t, 2, seenArtifact,
		"artifact-gated call-site count changed; a new whole-schema path must be checked against the artifact, not against \"\"")
}
