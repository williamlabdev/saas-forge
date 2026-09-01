package domain

// Data-level permission: which tenant roles may touch the ENTRIES of a content
// type, and which of them are confined to the entries they authored.
//
// This is the layer between the two that already existed, and the three answer
// genuinely different questions:
//
//	verb  — may this role write content in this tenant at all (authz)
//	DATA  — which ENTRIES of this type may it read or write (here)
//	field — which KEYS of those entries' documents (field_permission.go)
//
// All three must pass, and each can only NARROW. A viewer named in a type's
// write_roles still cannot write, because the verb refused first; a field's
// read_roles cannot re-open a collection its type closed.
//
// The unit is the tenant role and the declaration lives on the SCHEMA, for the
// three reasons field_permission.go already gives — per-user grants need a grant
// table, a surface to manage it, and an answer to what happens when the user
// leaves. Confinement is the one rule here that is per-USER, and it gets that
// for free: it is a role saying "your own", resolved against created_by at query
// time, so there is nothing to grant and nothing to revoke when someone goes.

// EntriesReadableBy reports whether a caller holding tenantRole may read entries
// of this type AT ALL. It says nothing about WHICH entries — see ConfinesToOwn,
// which narrows this further for a confined role.
func (ct *ContentType) EntriesReadableBy(tenantRole string) bool {
	return roleAllowed(ct.ReadRoles, tenantRole)
}

// EntriesWritableBy reports whether a caller holding tenantRole may create,
// update, publish or delete entries of this type. It answers only the TYPE
// question; the caller must already hold the content verb, and the field lists
// still narrow which keys the write may carry.
func (ct *ContentType) EntriesWritableBy(tenantRole string) bool {
	return roleAllowed(ct.WriteRoles, tenantRole)
}

// ConfinesToOwn reports whether tenantRole sees only the entries it created.
//
// UNLIKE the two above, an EMPTY list here means "nobody is confined" rather
// than "everybody is". That is not an inconsistency in the empty-means-
// unrestricted convention, it is that convention: an empty list restricts
// nobody in both cases. The lists differ in polarity — read_roles names who is
// ALLOWED, own_only_roles names who is CONFINED — so the same rule reads
// backwards if you match them by shape instead of by meaning. That is precisely
// why this is its own function and does not go through roleAllowed.
func (ct *ContentType) ConfinesToOwn(tenantRole string) bool {
	for _, r := range ct.OwnOnlyRoles {
		if r == tenantRole {
			return true
		}
	}
	return false
}

// RestrictedData reports whether the type carries any data-level declaration.
// The gates use it to short-circuit, so a tenant that never touches this feature
// pays nothing — and, more importantly, the list query keeps the plan it has
// today rather than gaining a predicate on a column nobody filtered by before.
func (ct *ContentType) RestrictedData() bool {
	return len(ct.ReadRoles) > 0 || len(ct.WriteRoles) > 0 || len(ct.OwnOnlyRoles) > 0
}

// There is NO owner/admin bypass here either, and it is the same ruling as
// 000026's for the same reason: a bypass makes the artifact lie. A file saying
// write_roles: ["editor"] would describe a collection two other roles can also
// write, and its reader has no way to know. It is not a lockout — owner and
// admin hold content:schema:write, so they can always rewrite the declaration.
//
// Confinement has one sharper edge than the field lists do, and it is worth
// stating rather than discovering: an OWNER listed in own_only_roles sees only
// their own entries, including entries created before anyone was confined. The
// remedy is the same (rewrite the declaration), but the symptom — a collection
// that looks EMPTY rather than forbidden — reads as data loss. That is why
// enabling confinement is guarded against entries with no recorded author
// instead of letting them silently disappear.
