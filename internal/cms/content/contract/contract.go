// Package contract holds the vendored frontend admin-app schema, which is the
// single source of truth for the legal field_type set (EntityField.type).
//
// The canonical file lives in the spec-generator repo (docs/schemas/) and is
// pinned here verbatim by that repo's tools/sync_specs.py PINS mechanism — NOT
// by SYNC_MAP, so this copy has no specs/ directory behind it and survived the
// console's move out of this repo (ADR-016). A parity test (../service) reads this embedded
// copy and asserts the content domain's field_type set matches the schema's
// enum — so the two can never silently drift, and there is no hand-maintained
// second copy of the list.
//
// TODO(encapsulation): replace vendoring with a shared published contract once
// the content domain is extracted into its own product repo.
package contract

import _ "embed"

//go:embed admin-app.schema.json
var AdminAppSchemaJSON []byte
