package service

import (
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Enforcing the data-level permission declared in domain/type_permission.go.
//
// The enforcement points are not interchangeable, and the split between them is
// the whole design:
//
//   - TYPE gate (read/write roles) — a decision about a COLLECTION, refused with
//     a named 403. The type's existence is not secret: every audience that may
//     call GET /types can already see its name, so pretending it is absent would
//     buy nothing and would send an operator hunting for a typo.
//
//   - CONFINEMENT (own_only_roles) — a decision about a ROW, and therefore a 404
//     rather than a 403. This is the same ruling GetEntry already applies to a
//     delivery credential fetching a draft: a distinguishable "exists but
//     forbidden" turns the endpoint into an oracle for ids, and here the id in
//     question is a colleague's unpublished draft.
//
//   - LIST — confinement becomes a WHERE clause, never a filter in Go. Filtering
//     a page after the database produced it makes `total` count rows the caller
//     may not see and leaves short pages with holes in them, so the caller can
//     recover the hidden count by subtraction. The gate has to be inside the
//     same query as COUNT(*) or it is not a gate.
//
// Confinement resolves against created_by, which has been the recorded author
// since 000021. A row with NO recorded author matches nobody, which is the
// fail-closed direction and also an invisible one — so enabling confinement is
// guarded against those rows up front rather than letting a collection appear to
// empty itself.

// errTypeReadForbidden refuses a read of a collection the caller's role is not
// on. NAMED and 403, not 404: see the file header.
func errTypeReadForbidden(typeName string, allowed []string) error {
	return apperrors.New("CONTENT_TYPE_READ_FORBIDDEN", "your role may not read entries of this type", 403).
		WithDetails(map[string]any{"type": typeName, "allowed_roles": allowed})
}

// errTypeWriteForbidden refuses a create, update, publish or delete on a
// collection the caller's role may not write.
//
// It reports the EFFECTIVE writers, for the reason effectiveWriteRoles exists at
// the field level: once reading is a precondition for writing, a type with
// read_roles ["owner"] and no write restriction refuses an editor while its
// write_roles say nothing at all. An error that misdescribes the rule sends the
// caller to ask for the wrong grant.
func errTypeWriteForbidden(typeName string, allowed []string) error {
	return apperrors.New("CONTENT_TYPE_WRITE_FORBIDDEN", "your role may not write entries of this type", 403).
		WithDetails(map[string]any{"type": typeName, "allowed_roles": allowed})
}

// errTypeRoleUnknown refuses a permission list naming a role that does not
// exist. Fail closed at DEFINITION time, exactly as the field lists do: a
// typo'd "editors" is a list nobody matches, so the collection silently becomes
// unreadable by everyone and the operator finds out from a support ticket.
func errTypeRoleUnknown(typeName, list, role string) error {
	return apperrors.New("CONTENT_TYPE_ROLE_UNKNOWN", "unknown tenant role in a content type permission list", 422).
		WithDetails(map[string]any{"type": typeName, "list": list, "role": role, "allowed": domain.AllowedFieldRoles()})
}

func errTypeRoleDuplicate(typeName, list, role string) error {
	return apperrors.New("CONTENT_TYPE_ROLE_DUPLICATE", "a content type permission list repeats a role", 422).
		WithDetails(map[string]any{"type": typeName, "list": list, "role": role})
}

// errOwnOnlyBackfill refuses TIGHTENING confinement while the type holds entries
// with no recorded author.
//
// Those rows match no author, so a confined role would simply stop seeing them —
// and an entry that vanishes reads as data loss, not as a permission. The other
// two lists need no such guard because refusing a read is legible: the caller is
// told they were refused. This one is the only permission in the stack whose
// denial is indistinguishable from absence, so it is the only one checked
// against stored data.
//
// 409 with the count, matching CONTENT_FIELD_REQUIRED_BACKFILL — the same shape
// of answer to the same shape of question: the declaration is fine, the data is
// not ready for it.
func errOwnOnlyBackfill(typeName string, roles []string, n int) error {
	return apperrors.New("CONTENT_ENTRY_AUTHOR_MISSING",
		"entries of this type have no recorded author and would become invisible to a confined role", 409).
		WithDetails(map[string]any{"type": typeName, "roles": roles, "entries": n})
}

// normalizeRoleList is the shared core of the field and type permission lists:
// trim, reject the empty string, and return nil for an empty result so an
// omitted list, an empty list and a list of blanks converge on one
// representation.
//
// It returns the offending role rather than an error, because the two callers
// must report DIFFERENT errors — a field-level refusal names a field and a
// type-level one names a list — and a shared error would name whichever one was
// written first.
//
// SORTED, for the reason normalizeRoles gives: a permission list is a SET, so
// leaving the order to the caller makes two artifacts that say the same thing
// compare unequal, and comparing equal is the one capability the format exists
// to give.
func normalizeRoleList(in []string) (out []string, unknown, duplicate string) {
	if len(in) == 0 {
		return nil, "", ""
	}
	seen := make(map[string]struct{}, len(in))
	out = make([]string, 0, len(in))
	for _, r := range in {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !domain.ValidFieldRole(r) {
			return nil, r, ""
		}
		if _, dup := seen[r]; dup {
			return nil, "", r
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, "", ""
	}
	sort.Strings(out)
	return out, "", ""
}

// normalizeTypeRoles validates one of a content type's three permission lists.
// listName is carried into the error so a caller who typo'd a role in
// own_only_roles is not told to look at read_roles.
func normalizeTypeRoles(typeName, listName string, in []string) ([]string, error) {
	out, unknown, dup := normalizeRoleList(in)
	if unknown != "" {
		return nil, errTypeRoleUnknown(typeName, listName, unknown)
	}
	if dup != "" {
		return nil, errTypeRoleDuplicate(typeName, listName, dup)
	}
	return out, nil
}

// canReadType answers the READ question for a collection, and is the only place
// the delivery rule for it lives.
//
// A public delivery credential is refused a restricted type OUTRIGHT rather than
// by failing to match the list — the same explicitness canReadField insists on,
// and for the same reason: the two are the same answer today only because that
// credential carries no tenant role, and they stop being the same the moment
// anyone mints a delivery token with a role on it.
func canReadType(ct *domain.ContentType, sub authn.Subject) bool {
	if len(ct.ReadRoles) == 0 {
		return true
	}
	if sub.PublicDelivery {
		return false
	}
	return ct.EntriesReadableBy(sub.TenantRole)
}

// canWriteType answers the WRITE question for a collection.
//
// A CALLER WHO CANNOT READ THE COLLECTION CANNOT WRITE IT — the ruling
// canWriteField already carries, applied one level up where it binds harder. A
// PATCH is an overlay on a stored document: a writer who has never been served
// that document cannot know what their write changes, and the client that builds
// the request from the schema will happily send a value for every key it can
// see. At the field level that produced measured, silent data loss; at the type
// level the same client would be posting whole entries blind.
//
// What this makes inexpressible is a drop box — submit entries, never read them
// back. Nobody has asked for one, and it needs its own decision anyway, because
// a blind write cannot be a PATCH over a document the writer must not see.
//
// PublicDelivery needs no branch: it is refused every write verb at authorize(),
// so it never reaches a write gate.
func canWriteType(ct *domain.ContentType, sub authn.Subject) bool {
	if !canReadType(ct, sub) {
		return false
	}
	return ct.EntriesWritableBy(sub.TenantRole)
}

// effectiveTypeWriteRoles is the roles that may ACTUALLY write entries of the
// type, which is what a refusal has to report. Mirrors effectiveWriteRoles; an
// empty result is a real answer ("nobody, because no role satisfies both"), not
// a missing one.
func effectiveTypeWriteRoles(ct *domain.ContentType) []string {
	switch {
	case len(ct.ReadRoles) == 0:
		return ct.WriteRoles
	case len(ct.WriteRoles) == 0:
		return ct.ReadRoles
	}
	readable := make(map[string]struct{}, len(ct.ReadRoles))
	for _, r := range ct.ReadRoles {
		readable[r] = struct{}{}
	}
	out := make([]string, 0, len(ct.WriteRoles))
	for _, r := range ct.WriteRoles {
		if _, ok := readable[r]; ok {
			out = append(out, r)
		}
	}
	return out
}

// guardTypeRead refuses a read of a collection this subject's role is not on.
func guardTypeRead(ct *domain.ContentType, sub authn.Subject) error {
	if canReadType(ct, sub) {
		return nil
	}
	return errTypeReadForbidden(ct.Name, ct.ReadRoles)
}

// guardTypeWrite refuses a create, update, publish or delete on a collection
// this subject's role may not write.
func guardTypeWrite(ct *domain.ContentType, sub authn.Subject) error {
	if canWriteType(ct, sub) {
		return nil
	}
	return errTypeWriteForbidden(ct.Name, effectiveTypeWriteRoles(ct))
}

// guardPreviewCollection refuses any read that can return more than the single
// entry a preview credential names.
//
// The scope check in GetEntry narrows exactly one path. Every OTHER delivery
// read is still reachable with a preview token, because a preview token IS a
// delivery token (audienceFor nests one inside the other, deliberately). That is
// safe for what those paths RETURN — they serve the published snapshot, which is
// public anyway — but not for what they COST: every one of them calls
// delivery.Record, so a token that was minted to show one draft to one reviewer
// doubles as a way to burn the tenant's metered read quota. A preview token is
// the only delivery credential that leaves the platform's own edge (ADR-006),
// which is what makes "an outsider holds this" a real case rather than a
// hypothetical one.
//
// 403 and not 404, unlike every other refusal in this file. The anti-oracle
// reasoning does not apply here: this refusal is UNIFORM — it depends only on
// the credential, never on the id or type asked for — so it discriminates
// nothing and there is nothing to enumerate. A 404 would instead tell a
// legitimate preview client that its collection does not exist, which is a lie
// it cannot act on.
func guardPreviewCollection(sub authn.Subject) error {
	if audienceFor(sub) != audiencePreview {
		return nil
	}
	return apperrors.New(
		"CONTENT_PREVIEW_SCOPE_EXCEEDED",
		"a preview credential addresses exactly one entry and may not read collections",
		403,
	)
}

// confinedAuthor returns the author id this subject's view of the type is
// restricted to, or nil when it is not restricted at all.
//
// A DELIVERY CREDENTIAL IS NEVER CONFINED, and that is a rule rather than an
// exception. Confinement means "your rows within a collection you browse"; a
// public reader browses no collection and authors nothing, and what it may see
// is decided by publish state (ADR-004/006). Matching it on created_by would
// hide every published entry from the public the moment any role was confined —
// a live-site outage caused by an editorial setting.
//
// A confined caller with NO user id (uuid.Nil is reachable: the dev-header
// middleware accepts an all-zero X-User-Id) is confined to an author that no
// write path ever records — actor() stores nil for exactly that subject — so it
// matches no row. That is the fail-closed direction, and it is arrived at
// deliberately rather than by an unhandled case: a caller nobody can name has
// authored nothing.
// AN AGENT CREDENTIAL IS CONFINED TO ITS PRINCIPAL, not to itself (ADR-013 §2
// amendment). The predicate binds the same id the writes record, which is what
// makes confinement coherent: an agent can read back what it just wrote.
//
// The cost is stated rather than hidden: Alice's agent also reads everything
// Alice typed herself in that type. This is the ONE place the agent kind widens
// rather than narrows, unlike PublicDelivery and PreviewEntryID, and it was
// chosen because it costs nothing to implement and moves no index, while §1's
// "reaches nothing its minter could not" still holds — Alice's own rows are by
// definition within Alice's reach. If a narrow-purpose bot must not read its
// minter's whole body of work, the rejected alternative (own_only binding the
// principal for writes but adding a created_by_agent predicate for reads) is
// written down in ADR-013 §2 with its index cost; take it from there rather
// than reinventing it.
func confinedAuthor(ct *domain.ContentType, sub authn.Subject) *uuid.UUID {
	if sub.PublicDelivery || !ct.ConfinesToOwn(sub.TenantRole) {
		return nil
	}
	id := sub.ResponsibleUserID()
	return &id
}

// guardOwned refuses a single-entry operation on a row this subject does not
// own, when their role is confined.
//
// apperrors.ErrNotFound, NOT a 403, and not a named error either. A confined
// editor must not be able to tell a colleague's draft from an id that was never
// issued — the moment those two answers differ, iterating ids maps out the
// collection the confinement exists to hide. This is the one refusal in the
// permission stack that deliberately says nothing.
func guardOwned(ct *domain.ContentType, sub authn.Subject, e *domain.Entry) error {
	author := confinedAuthor(ct, sub)
	if author == nil {
		return nil
	}
	if e.CreatedBy != nil && *e.CreatedBy == *author {
		return nil
	}
	return apperrors.ErrNotFound
}
