package domain

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/williamlabdev/saas-forge/internal/pkg/mediapreset"
)

// Media presets (ADR-019). Fixed, platform-wide, width-bound. A preset names
// the LONGEST width a variant may have; the worker never upscales, and a
// source narrower than or equal to the preset gets a MediaVariantSkipped row
// with MediaSkipSourceSmaller instead of a copy of itself.
//
// The table itself lives in internal/pkg/mediapreset (stdlib-only) because the
// delivery edge needs it too and must not import this package (ADR-004). The
// names below are aliases so the rest of the cms code keeps its vocabulary;
// the rationale for the table being fixed is documented there.
const (
	MediaPresetThumb  = mediapreset.Thumb
	MediaPresetSmall  = mediapreset.Small
	MediaPresetMedium = mediapreset.Medium
	MediaPresetLarge  = mediapreset.Large

	// MediaPresetOriginal is the row that describes the uploaded bytes
	// themselves: the worker measures the source once and records what it saw,
	// so the admin DTO can show width/height that the platform has actually
	// observed rather than what the client declared (see MediaAsset.WidthPx).
	// It is never a resize target and never a `?preset=` value.
	MediaPresetOriginal = mediapreset.Original
)

// MediaPresets maps each resize preset to its maximum width in pixels. It is
// the mediapreset.Widths table, kept in lockstep with
// media_variants_preset_check (migration 000043). The `original` row is not
// here on purpose — it has no width bound.
var MediaPresets = mediapreset.Widths

// MediaPresetNames returns the resize presets in ascending width order. Stable
// ordering matters because the worker inserts rows and the DTO lists them; a
// map walk would make both nondeterministic.
func MediaPresetNames() []string {
	return mediapreset.Names()
}

// ValidMediaPreset reports whether p names a RESIZE preset. `original` is
// intentionally false here: the delivery path must not be able to ask for it
// by name (it already gets the original by asking for nothing), and the
// enqueue path must not try to resize it.
func ValidMediaPreset(p string) bool {
	return mediapreset.Valid(p)
}

// Variant states. Kept in lockstep with media_variants_state_check.
//
// There is no `processing` state on purpose. The worker claims rows with
// SELECT ... FOR UPDATE SKIP LOCKED inside the transaction that also writes
// the result, exactly as the scheduler does (ADR-017 §3): a crash mid-work
// rolls the claim back and the row is simply pending again. A `processing`
// column would need a reaper for the case where the process died, and that
// reaper is the bug factory this design avoids.
const (
	MediaVariantPending = "pending"
	MediaVariantDone    = "done"
	MediaVariantSkipped = "skipped"
	MediaVariantFailed  = "failed"
)

// Skip reasons. A skipped variant is not an error: the platform looked and
// decided there was nothing useful to produce. The reason tells the editor
// which of the deliberate limits applied.
const (
	// MediaSkipSourceSmaller — the source is no wider than the preset; the
	// original serves that preset already.
	MediaSkipSourceSmaller = "source_smaller"
	// MediaSkipUnsupportedFormat — the platform accepts the upload type but has
	// no pure-Go decoder for it (AVIF today; ADR-019 trigger (b)).
	MediaSkipUnsupportedFormat = "unsupported_format"
	// MediaSkipTooLarge — the header declares more pixels than MaxSourcePixels;
	// refused before a full decode so a pixel bomb cannot exhaust the worker.
	MediaSkipTooLarge = "too_large"
)

// MaxMediaVariantAttempts is how many times the worker tries a variant before
// marking it failed for good. Three, with backoff, covers the transient cases
// (bucket blip, pod restart mid-tick); anything that fails three times in a
// row is a property of the bytes and retrying forever would only burn CPU.
const MaxMediaVariantAttempts = 3

// MediaVariant is one derived rendition of a MediaAsset (or the measured
// `original`). Rows exist iff the asset has been marked uploaded and its
// content type is an image; both are decided in one transaction so a reader
// never sees an uploaded image with no variant rows (ADR-019 §3).
type MediaVariant struct {
	AssetID  uuid.UUID
	TenantID string
	Preset   string
	State    string

	// StorageKey is where the bytes are once State is done. For the `original`
	// row it equals the asset's own key from the start. Kept across a re-enqueue
	// so a regenerated variant overwrites its previous object instead of
	// leaking one.
	StorageKey  string
	ContentType string
	SizeBytes   int64
	WidthPx     int
	HeightPx    int

	// Attempts and NextAttemptAt drive the retry schedule; Error carries the
	// last failure's message (truncated by the worker), SkipReason one of the
	// MediaSkip* constants. Empty unless the state says otherwise.
	Attempts      int
	NextAttemptAt time.Time
	Error         string
	SkipReason    string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsImageContentType reports whether ct is a raster image the platform accepts
// for upload. It decides whether an asset gets variant rows at all — a PDF or
// an MP4 has no presets and no `original` row, so its DTO carries an empty
// list rather than four rows that would never leave pending.
//
// It keys on the normalized media type, so a stray parameter such as
// `image/jpeg; charset=binary` still counts. Matching by prefix would also
// accept image/svg+xml and image/heic, which are not in allowedUploadTypes
// today but would silently become "enqueue and then fail three times" the day
// someone adds them; the explicit set keeps that decision in this file.
func IsImageContentType(ct string) bool {
	mt := strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	switch mt {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/avif":
		return true
	}
	return false
}
