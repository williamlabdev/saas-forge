package service

import (
	"encoding/json"
	"sort"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Enforcing the field permission declared in domain/field_permission.go.
//
// There are three enforcement points and they are not interchangeable:
//
//   - WRITE — refused BEFORE the PATCH merge, naming the field. The order is the
//     whole ruling. Dropping unauthorised keys silently is the tempting shape and
//     it is wrong twice over: the caller is told the write succeeded when their
//     value was discarded, and because validatePayload checks the WHOLE merged
//     document, a dropped key can surface later as a REQUIRED error naming a
//     field the caller never sent. Refusing before the merge means the response
//     names exactly the key the caller typed.
//
//   - READ — stripped at serialisation, from the audience-aware projector.
//
//   - QUERY — filter and sort refused on unreadable fields. Without this the read
//     projection is decorative: `?filter=salary:gte:100000` is a binary search
//     over a value the caller was just refused, one request per bit.
//
// The write gate deliberately does NOT re-derive who may write entries at all —
// authorize() already ran, and these lists only narrow it. A viewer named in
// write_roles still cannot write, because the content verb refused first.

// errFieldWriteForbidden is the named 403 the write gate raises. It carries the
// field and the roles that MAY write it — a caller who is told only "forbidden"
// has to guess whether they hit a field rule or a verb rule, and the two have
// completely different fixes (ask for the field, versus ask for the role).
func errFieldWriteForbidden(key string, allowed []string) error {
	return apperrors.New("CONTENT_FIELD_WRITE_FORBIDDEN", "your role may not write this field", 403).
		WithDetails(map[string]any{"field": key, "allowed_roles": allowed})
}

// errFieldRequiredNotWritable is the create-time dead end: the type demands a
// value for a field this caller may not send, so no create can ever succeed.
//
// It is its own code rather than the generic write refusal because the remedy is
// different and neither one is "try again without that key". The caller needs
// the role, or the field needs to stop being required — and hearing
// CONTENT_FIELD_REQUIRED instead (which is what happens if this check is
// omitted) points them at a key they are not allowed to type.
func errFieldRequiredNotWritable(key string, allowed []string) error {
	return apperrors.New("CONTENT_FIELD_REQUIRED_NOT_WRITABLE",
		"this content type requires a field your role may not write", 403).
		WithDetails(map[string]any{"field": key, "allowed_roles": allowed})
}

// errFieldQueryForbidden refuses a filter or sort on an unreadable field.
//
// 403 and NAMED, not a pretend "unknown field", because field DEFINITIONS are
// not secret in this model — every audience that can query can also read
// GET /types. What is secret is the VALUE. Pretending the field does not exist
// would buy no confidentiality and would send an operator hunting for a typo in
// a key that is right there in the schema.
func errFieldQueryForbidden(key, clause string) error {
	return apperrors.New("CONTENT_FIELD_QUERY_FORBIDDEN", "your role may not query this field", 403).
		WithDetails(map[string]any{"field": key, "clause": clause})
}

// errFieldRoleUnknown refuses a permission list naming a role that does not
// exist. Fail closed at DEFINITION time: a typo'd "editors" in write_roles is a
// list nobody matches, so the field silently becomes unwritable by everyone and
// the operator discovers it when an editor files a bug.
func errFieldRoleUnknown(key, role string) error {
	return apperrors.New("CONTENT_FIELD_ROLE_UNKNOWN", "unknown tenant role in a field permission list", 422).
		WithDetails(map[string]any{"field": key, "role": role, "allowed": domain.AllowedFieldRoles()})
}

func errFieldRoleDuplicate(key, role string) error {
	return apperrors.New("CONTENT_FIELD_ROLE_DUPLICATE", "a field permission list repeats a role", 422).
		WithDetails(map[string]any{"field": key, "role": role})
}

// normalizeRoles validates one FIELD permission list and returns it in a
// canonical form: trimmed, sorted, and nil when empty.
//
// The canonicalisation itself lives in normalizeRoleList (type_permission.go),
// which the content type's three lists share. Only the ERRORS are here, because
// only the errors differ: a field-level refusal has to name the field, and a
// type-level one has to name which of three lists went wrong. One shared error
// would name whichever caller was written first.
//
// SORTED, unlike enum_values. Enum order is part of a field's meaning (it is
// what an admin form renders), so ADR-007 lets a reorder be a real change. A
// permission list is a SET — ["admin","editor"] and ["editor","admin"] grant
// identically — so leaving the order to the caller would make two artifacts that
// say the same thing compare unequal, and the whole point of the artifact format
// is that comparing equal means equal.
//
// nil rather than []string{} for the empty case, so an omitted list, an empty
// list and a list of nothing but blanks converge on one representation. The
// repository writes nil as a SQL empty array (the column is NOT NULL DEFAULT
// '{}'), and NewArtifact renders both as an absent key.
func normalizeRoles(key string, in []string) ([]string, error) {
	out, unknown, dup := normalizeRoleList(in)
	if unknown != "" {
		return nil, errFieldRoleUnknown(key, unknown)
	}
	if dup != "" {
		return nil, errFieldRoleDuplicate(key, dup)
	}
	return out, nil
}

// canReadField answers the READ question for one subject, and is the only place
// the delivery rule lives.
//
// A public delivery credential is refused every restricted field OUTRIGHT rather
// than by failing to match the list. The two are the same answer today — that
// credential carries no tenant role — and they stop being the same the moment
// anyone mints a delivery token with a role on it, which is precisely the kind
// of change nobody would connect to this file. authorize() already treats
// PublicDelivery as a narrowing that overrides whatever the token claims; this
// matches it.
func canReadField(f domain.Field, sub authn.Subject) bool {
	if len(f.ReadRoles) == 0 {
		return true
	}
	if sub.PublicDelivery {
		return false
	}
	return f.ReadableBy(sub.TenantRole)
}

// canWriteField answers the WRITE question. PublicDelivery needs no branch: it
// is refused every write verb at authorize(), so it never reaches a write gate.
//
// A CALLER WHO CANNOT READ THE FIELD CANNOT WRITE IT. That is not a tidy
// symmetry, it is the fix for a silent data-loss path found by running this
// against a real client:
//
// The read gate hands the caller a document with a hole in it. Any client that
// renders a form FROM THE SCHEMA and posts the form back — which is what the
// admin app does, and what any generated client would do — fills that hole with
// whatever an empty control produces, and PATCH semantics say the keys you send
// are the keys you set. Measured, not theorised: with read_roles: ["owner"] and
// no write restriction, an editor opening and saving an entry sent
// `"salary": 0` (the app's number control renders "" and Number("") is 0) and
// turned 100000 into 0, HTTP 200, no error, on a field they had never been shown.
//
// Fixing only the client would leave the trap armed for the next one, which is
// this repo's established failure mode — a new path that forgets. So it is
// closed here, at the same chokepoint the read gate uses. A caller cannot have
// MEANT to write a value they were never shown.
//
// What this makes inexpressible is a write-only field: submit but never read
// back (a rating, a sealed bid). Nobody has asked for one, and it needs its own
// decision anyway — a blind write cannot be a PATCH over a document, because the
// document is exactly what the writer must not see.
func canWriteField(f domain.Field, sub authn.Subject) bool {
	if !canReadField(f, sub) {
		return false
	}
	return f.WritableBy(sub.TenantRole)
}

// effectiveWriteRoles is the roles that may ACTUALLY write the field, which is
// what a refusal has to report.
//
// Reporting f.WriteRoles alone was wrong the moment reading became a
// precondition for writing: a field with read_roles ["owner"] and no write
// restriction would refuse an editor while reporting `allowed_roles: []` —
// which reads as "nobody may write this" when the truth is "only the owner may".
// An error that misdescribes the rule sends the caller to ask for the wrong
// grant.
//
// An EMPTY result here is a real answer, not a missing one: read ["owner"] with
// write ["editor"] is a field nobody can write, because no role satisfies both.
// That is almost certainly an operator mistake, and it is reported rather than
// refused at definition time — refusing it would be a new rule, and "frozen
// field" is a coherent thing to have meant.
func effectiveWriteRoles(f domain.Field) []string {
	switch {
	case len(f.ReadRoles) == 0:
		return f.WriteRoles
	case len(f.WriteRoles) == 0:
		return f.ReadRoles
	}
	readable := make(map[string]struct{}, len(f.ReadRoles))
	for _, r := range f.ReadRoles {
		readable[r] = struct{}{}
	}
	// Built from WriteRoles so the result keeps the canonical (sorted) order
	// both lists already carry.
	out := make([]string, 0, len(f.WriteRoles))
	for _, r := range f.WriteRoles {
		if _, ok := readable[r]; ok {
			out = append(out, r)
		}
	}
	return out
}

// unreadableKeys lists the field keys this subject may not see. nil when the
// type declares no read restriction at all, which is the case for every type
// predating migration 000026 — and is what lets the projector leave the stored
// payload bytes completely untouched in the common case.
func unreadableKeys(fields []domain.Field, sub authn.Subject) []string {
	var out []string
	for _, f := range fields {
		if !canReadField(f, sub) {
			out = append(out, f.Key)
		}
	}
	return out
}

// stripKeys removes keys from a payload document.
//
// It re-encodes, so the output bytes are Go's rendering rather than the stored
// JSONB text. That is why the caller only invokes it when something is actually
// hidden: an unconditional round-trip would rewrite every payload on every read
// for no gain. On a malformed payload it returns an empty object rather than the
// input — the fail-closed answer, since the input is the document that was meant
// to have keys removed from it.
func stripKeys(raw json.RawMessage, keys []string) json.RawMessage {
	if len(keys) == 0 || len(raw) == 0 {
		return raw
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return json.RawMessage(`{}`)
	}
	for _, k := range keys {
		delete(doc, k)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return out
}

// payloadKeys returns the top-level keys of a patch document, so the write gate
// can name what the CALLER sent rather than what the merge produced.
func payloadKeys(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, apperrors.Wrap("CONTENT_PAYLOAD_INVALID", "payload must be a JSON object", 400, err)
	}
	out := make([]string, 0, len(doc))
	for k := range doc {
		out = append(out, k)
	}
	// Sorted so a document touching two forbidden fields always names the same
	// one. Map iteration order would make the error message a coin flip, and a
	// test pinning it would be flaky rather than wrong.
	sort.Strings(out)
	return out, nil
}

// guardWritableKeys refuses a write that touches a field the subject may not
// write. It runs BEFORE the merge and before validation — see the file comment.
//
// Keys the schema does not define are passed over, not refused here: they are
// not a permission problem, and validatePayload already answers them with
// CONTENT_FIELD_UNKNOWN. Answering 403 for an undefined key would also turn this
// gate into an oracle for which fields exist, for the one caller who cannot read
// the schema.
func guardWritableKeys(ct *domain.ContentType, sub authn.Subject, payload json.RawMessage) error {
	if !anyRestricted(ct.Fields) {
		return nil
	}
	keys, err := payloadKeys(payload)
	if err != nil {
		return err
	}
	for _, k := range keys {
		f, ok := ct.FieldByKey(k)
		if !ok {
			continue
		}
		if !canWriteField(f, sub) {
			return errFieldWriteForbidden(k, effectiveWriteRoles(f))
		}
	}
	return nil
}

// guardRequiredWritable refuses a CREATE the caller could never complete: the
// type requires a field their role may not write.
//
// Create-only. On an update the stored document already carries the value, and
// the merge preserves it — a caller who may not write a required field can still
// PATCH the fields they do own.
func guardRequiredWritable(ct *domain.ContentType, sub authn.Subject) error {
	for _, f := range ct.Fields {
		if f.Required && !canWriteField(f, sub) {
			return errFieldRequiredNotWritable(f.Key, effectiveWriteRoles(f))
		}
	}
	return nil
}

// anyRestricted reports whether any field on the type declares a permission.
// The gates use it to short-circuit, so a tenant that never touches this feature
// pays nothing — not even an extra unmarshal of the patch.
func anyRestricted(fields []domain.Field) bool {
	for _, f := range fields {
		if f.Restricted() {
			return true
		}
	}
	return false
}
