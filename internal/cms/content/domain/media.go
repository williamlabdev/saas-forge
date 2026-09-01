package domain

import (
	"time"

	"github.com/google/uuid"
)

// MediaAsset is one stored file's metadata. The bytes live in object storage
// under StorageKey; this row is the only thing that knows the mapping.
//
// An asset is created in a RESERVED state (UploadedAt nil) when the client asks
// for an upload URL, and becomes usable only once the upload is confirmed. That
// ordering matters: a reservation that is never completed must not be
// referenceable, or an entry could point at bytes that do not exist.
type MediaAsset struct {
	ID          uuid.UUID
	TenantID    string
	StorageKey  string
	ContentType string
	SizeBytes   int64
	UploadedAt  *time.Time
	CreatedAt   time.Time

	// Filename, AltText, WidthPx and HeightPx are CLIENT-DECLARED and unverified,
	// unlike every field above them. The platform observes size and content type
	// against the bucket; it has nothing to observe these from, because the
	// storage key is random and the bytes never reach the API (migration 000022
	// argues the full case). Treating them as authenticated facts is the mistake
	// this comment exists to prevent.
	//
	// Pointers, not zero values, because absence is a distinct answer from every
	// legal value — most sharply for AltText, where nil means "nobody has
	// described this image" and a pointer to "" means "an editor said it is
	// decorative". Collapsing those makes a renderer assert the second on the
	// editor's behalf.
	Filename *string
	AltText  *string
	// WidthPx and HeightPx move together or not at all: a lone dimension reserves
	// no layout space, which is the only thing they are for. The DB enforces the
	// biconditional; see MaxImageDimension.
	WidthPx  *int
	HeightPx *int
}

// IsUploaded reports whether the bytes actually landed.
func (m *MediaAsset) IsUploaded() bool { return m.UploadedAt != nil }

// Limits on the client-declared metadata above. These MIRROR the CHECK
// constraints in migration 000022 — the database is the authority, because it
// refuses the write whatever Go believes. They exist in Go only so a violation
// comes back as a 422 naming the offending field instead of a driver error that
// renders as a 500. TestMediaMetadataLimitsParity reads the migration and pins
// the two together, so drift fails a test rather than shipping.
const (
	MaxFilenameLen    = 255
	MaxAltTextLen     = 1000
	MaxImageDimension = 65535
)
