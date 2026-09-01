package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// EntryRevision is one entry's working copy as it stood at one version
// (ADR-014 §5). It is the "what the content became" half of the division of
// labour Activity's doc comment states: that type names keys and never values,
// this one carries the values and nothing about intent.
//
// STORED, NOT RESTORABLE. §5 keeps the storing and refuses the restoring, and
// the refusal is not squeamishness: a restore replays a whole old payload
// through UpdateEntry, which REFUSES keys the caller may not write rather than
// dropping them, so a restricted role could never restore an entry holding a
// restricted field. What these rows buy is that the decision remains open at
// the moment it is first actually needed, with the data already in hand.
//
// Nothing above the repository reads these yet, and adding a reader is not a
// small change: this type holds every restricted field's value in full, so
// §6's field-level masking applies to it before it can reach a response.
type EntryRevision struct {
	EntryID uuid.UUID
	// Version is the entry version this payload was written AT — the value the
	// row held after that write, not before it. Version numbers are sparse
	// here: publish and unpublish bump the entry's version without changing its
	// payload and write no revision, so "the content at version N" means the
	// newest revision with Version <= N. See migration 000034's header for
	// which writes produce a row and which deliberately do not.
	Version int
	// Payload is the working copy verbatim. There is no published-copy
	// equivalent because there need not be: a snapshot is a copy of the working
	// copy at some version, so the revision that version resolves to already
	// holds it.
	Payload  json.RawMessage
	TenantID string
	// AuthorKind / AuthorUserID / AuthorAgentID are §4's trio, copied from the
	// entry row the write just produced rather than recorded first-hand. They
	// are all nil-able for that reason — the source column is nullable by
	// 000031's ruling, and copying "not recorded" forward is honest where
	// substituting a person is not. A reader renders the nil case as unknown
	// and never falls back to naming AuthorUserID's human.
	AuthorKind    *string
	AuthorUserID  *uuid.UUID
	AuthorAgentID *string
	// CreatedAt is the entry's updated_at for that write, copied rather than
	// generated so a revision and its write share one instant.
	CreatedAt time.Time
}
