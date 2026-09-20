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
	//
	// 43 with the three scheduling methods (ADR-017), which is FOUR call sites,
	// not three: CancelEntrySchedule authorizes twice. Its baseline is
	// content:update, because withdrawing something that has not happened is not
	// itself a publish — but when the pending row is a publish it then demands
	// content:publish as well, or an agent holding only content:update could
	// delete a human's approved release the hour before it went out. All four
	// name their type: a schedule is about one entry, so the same per-type read
	// and write rules that gate publishing gate scheduling it.
	//
	// 44 with publish revisions and restore (ADR-018), which is ONE call site
	// for four endpoints: all four open with resolveRevisionEntry, which
	// authorizes once and takes the verb as a parameter — content:read for the
	// three reads and content:update for the restore, because restoring writes
	// the working copy and does not publish.
	//
	// That one site names its type, and the number moving by one rather than
	// four is exactly the drop this guard's own comment warns about. It is
	// deliberate here: the four endpoints share a guard SEQUENCE whose ORDER is
	// load-bearing (audience refusal before the type lookup, confinement last),
	// and four copies of it would be four chances to reorder one. The property
	// this test protects — that a path with a type in scope forwards it — is
	// still checked, at the helper, which is where the parameter now lives.
	//
	// 45 with EnqueueMediaVariants (ADR-019), which passes the asset id and ""
	// exactly as the other media methods do: an asset belongs to no content
	// type until an entry links it, so there is no type to name, and the
	// content:update verb is what keeps the re-enqueue button off the delivery
	// surface at the same chokepoint as CompleteMediaUpload.
	//
	// 53 with the eight component WRITE verbs (ADR-020 §6 plus Amendment 1's
	// RenameComponent): CreateComponent, UpdateComponent, RenameComponent,
	// DeleteComponent, AddComponentField, UpdateComponentField,
	// RenameComponentField, DeleteComponentField. All eight pass "" and must: a
	// component belongs to no single type — several may embed it — so the ""
	// is the same untyped call ApplySchema makes, and the one §4 refuses every
	// agent. That refusal is the mechanism by which an agent can READ a
	// component (through authorizeComponentRead, whose whitelist check is the
	// referring types') but never change one. The two reads are not counted
	// here for the reason authorizeArtifact's sites are counted apart: they
	// gate through authorizeAgentScope with a closure, not through authorize.
	//
	// 54 with ListMediaAssets (the admin media library). It passes "" for the
	// same reason EnqueueMediaVariants (45) does: a media asset belongs to no
	// content type until an entry links it, so there is no type to forward,
	// and content:list is what keeps the listing off the delivery surface —
	// PublicDelivery is refused separately, inside the method, before the type
	// question even arises.
	//
	// 55 with Search (search.go, ADR-021 §3), the cross-type search endpoint.
	// It passes "" for the same reason ListPendingReview does: the query spans
	// every content type in the tenant, so there is no single type to forward,
	// and the per-type gate is replaced by dataVisibleExpr running inside the
	// SQL the repository builds — see SearchEntriesFilter's doc comment.
	//
	// 56 with ListLocales (locales.go, T2 2.3), the locale inventory endpoint.
	// It passes "" for the same reason Search does: an optional `type` query
	// parameter narrows the COUNT after this gate, but the endpoint itself
	// spans every content type in the tenant (an unfiltered call answers for
	// all of them), so there is no single type this authorize call could name.
	// The per-row data-level gate is the same dataVisibleExpr Search shares.
	//
	// 57 with EntryReferencedBy (ADR-024 §2.8a), the reverse-reference lookup
	// for one entry. It names its type: the call answers about one entry, so
	// the same per-type read rule that gates GetEntry gates who may ask what
	// links to it, and an agent's whitelist then narrows the referrer rows
	// projectReferencedBy returns rather than the entry itself.
	//
	// 58 with MediaReferencedBy (ADR-024 §2.8a), the reverse-reference lookup
	// for one media asset. It passes "" for EnqueueMediaVariants's reason (45):
	// an asset belongs to no content type until an entry links it, so there is
	// no type to forward — and because §4 refuses every agent credential an
	// untyped call, this is also why no agent ever reaches it at all, not only
	// why the ones that do get filtered rows.
	//
	// 59 with InstallComponentTemplate (ADR-020 Amendment 2). It passes ""
	// for CreateComponent's own reason (its own site, not counted twice): a
	// component names no content type, so there is nothing to forward, and
	// content:create is what closes the untyped write to every agent
	// credential exactly as it does for a hand-authored POST /components.
	// ListComponentTemplates is a read and is not counted here for the same
	// reason the two component reads above it are not: it gates through
	// authorizeComponentRead, not authorize.
	//
	// 60 with RequestChanges (ADR-014 Amendment: review decisions). It reuses
	// content:publish, the same verb ScheduleEntry and PublishEntry already
	// gate on — a reviewer sending an entry back is the same human gate as
	// approving it, just the other outcome, so this is not a new verb; it is
	// the existing one's other branch.
	//
	// 61 with ListEntryReviewDecisions (ADR-014 Amendment: review decisions).
	// It names its type for the same reason EntryReferencedBy (57) does: the
	// call answers about one entry, resolved through GetContentTypeByName
	// before the entry is loaded, so the per-type read rule that gates
	// GetEntry gates its decision history the same way.
	//
	// 62 with SweepOrphans (003-orphan-gc). It passes "" for
	// EnqueueMediaVariants's reason (45): the sweep spans every asset in the
	// tenant, so there is no single type to forward — and it reuses
	// content:delete, the same verb DeleteMediaAsset already gates on, so
	// this is not a new verb either; the per-item recheck then applies each
	// asset's own delete gate on top.
	require.Equal(t, 62, seen, "authorize call-site count changed; re-check the new sites and update this number")
	// 2 with ProposeSchema, which gates against the DOCUMENT for PlanSchema's
	// reason (補裁 E): the types are in the artifact, so an agent's whitelist is
	// enforced against them. Passing "" here instead would refuse every agent
	// the propose verb was created for.
	require.Equal(t, 2, seenArtifact,
		"artifact-gated call-site count changed; a new whole-schema path must be checked against the artifact, not against \"\"")
}
