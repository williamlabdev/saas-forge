package domain

import (
	tenant "github.com/williamlabdev/saas-forge/internal/tenant/domain"
)

// Field-level permission: which tenant roles may READ a field's value and which
// may WRITE it. Declared on the field definition, so it travels with the schema
// — into GET /types, into the artifact, into git — rather than living in a
// policy file that describes fields nobody can see from the schema.
//
// The unit is the tenant role (D3), not the user, because per-user grants need a
// grant table, an admin UI to manage it, and an answer to what happens when the
// user leaves. Roles are what the content verbs already decide on
// (allowContentByTenantRole), so this narrows an existing decision instead of
// introducing a second, differently-shaped one.
//
// This is a DATA rule, not a VERB rule. `content:update` still says whether the
// caller may write entries of this type at all; these lists say which keys of
// the document that write may touch. Both have to pass — the field lists can
// only narrow, never widen, which is why a viewer does not gain write access by
// being named in write_roles.

// AllowedFieldRoles is the legal membership of read_roles / write_roles: the
// tenant-scoped role set, taken from its owner rather than copied. A fifth
// hand-synced copy of a fixed set is exactly the drift the vendored contract
// already demonstrated; a parity test pins this against migration 000026's
// CHECK, which is in turn the same set as memberships' (000012).
func AllowedFieldRoles() []string {
	return []string{tenant.RoleOwner, tenant.RoleAdmin, tenant.RoleEditor, tenant.RoleViewer}
}

// ValidFieldRole reports whether r may appear in a permission list.
func ValidFieldRole(r string) bool { return tenant.ValidRole(r) }

// ReadableBy reports whether a caller holding tenantRole may see this field's
// value. An EMPTY list means unrestricted — see roleAllowed.
func (f Field) ReadableBy(tenantRole string) bool { return roleAllowed(f.ReadRoles, tenantRole) }

// WritableBy reports whether a caller holding tenantRole may send a value for
// this field. It answers only the FIELD question; the caller must already hold
// the content verb.
func (f Field) WritableBy(tenantRole string) bool { return roleAllowed(f.WriteRoles, tenantRole) }

// Restricted reports whether the field carries any permission declaration at
// all. Read paths use it to skip the whole projection when nothing is
// restricted, which is what keeps the common case byte-for-byte unchanged.
func (f Field) Restricted() bool { return len(f.ReadRoles) > 0 || len(f.WriteRoles) > 0 }

// roleAllowed is the one place the empty-list convention is decided.
//
// EMPTY MEANS UNRESTRICTED. The opposite convention — empty means nobody —
// reads as the safer default and is not: every field that exists today has an
// empty list, so it would turn migration 000026 into a total outage, and "no
// declaration" is not the same statement as "denied to all". The cost is that a
// field nobody may touch is inexpressible, which is fine, because that field is
// one you delete.
//
// A caller with NO tenant role (the public delivery credential) therefore
// matches every unrestricted field and no restricted one. That fall-through is
// load-bearing and deliberately not special-cased into a bypass: the moment an
// operator names any role, the open internet is off the list.
func roleAllowed(list []string, tenantRole string) bool {
	if len(list) == 0 {
		return true
	}
	for _, r := range list {
		if r == tenantRole {
			return true
		}
	}
	return false
}

// There is NO owner/admin bypass, and that is a ruling rather than an omission.
//
// A bypass would make the artifact lie: a file saying write_roles: ["editor"]
// would describe a field two other roles can also write, and the reader of that
// file has no way to know. The lockout it would prevent is not a lockout —
// owner and admin hold content:schema:write, so they can always rewrite the
// declaration and grant themselves back. A recoverable footgun beats a document
// that cannot be trusted.
//
// The one sharp edge this leaves is REQUIRED + unwritable: a role that cannot
// write a required field cannot create entries of that type at all. The service
// refuses that up front with its own named error rather than letting it surface
// as a confusing "missing required field" naming a key the caller is not allowed
// to send.
