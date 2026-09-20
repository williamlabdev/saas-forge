// Package mediapreset is the platform-wide table of media resize presets
// (ADR-019): the names a template may write into `?preset=` and the maximum
// width each one stands for.
//
// It lives under internal/pkg rather than in the cms domain because two
// modules need it and only one of them is the CMS: the delivery edge has to
// canonicalise a `?preset=` value before forwarding it upstream (the 302 target
// must not be derived from request input, gosec G710), and ADR-004 keeps that
// edge thin — it imports auth/jwt, pkg/ratelimit and this package, never
// internal/cms. The package therefore depends on the standard library only.
//
// The table is deliberately not per-tenant configuration: a preset name is a
// contract between the delivery edge, the CDN cache and whoever wrote
// `?preset=thumb` into a template. Letting tenants rename or resize them turns
// a cache key into a moving target. Widths are chosen to cover the common
// responsive breakpoints; changing one is an ADR-019 amendment, not a config
// edit, because every already-generated variant would be wrong. The CHECK
// constraint in cms migration 000043 is kept in lockstep with this file.
package mediapreset

// Resize preset names. A preset names the LONGEST width a variant may have;
// the worker never upscales.
const (
	Thumb  = "thumb"
	Small  = "small"
	Medium = "medium"
	Large  = "large"

	// Original is the row that describes the uploaded bytes themselves. It is
	// never a resize target and never a `?preset=` value — Valid rejects it.
	Original = "original"
)

// Widths maps each resize preset to its maximum width in pixels. The
// `original` row is not here on purpose — it has no width bound.
var Widths = map[string]int{
	Thumb:  320,
	Small:  640,
	Medium: 1024,
	Large:  1920,
}

// Names returns the resize presets in ascending width order. Stable ordering
// matters because the worker inserts rows and the DTO lists them; a map walk
// would make both nondeterministic.
func Names() []string {
	return []string{Thumb, Small, Medium, Large}
}

// Valid reports whether p names a RESIZE preset. `original` is intentionally
// false here: the delivery path must not be able to ask for it by name (it
// already gets the original by asking for nothing), and the enqueue path must
// not try to resize it. Matching is exact — no trimming, no case folding — so
// a cache key is one spelling.
//
// There is deliberately no "canonicalise and return the table's own string"
// helper here: gosec's taint analysis (G710) treats a value returned from a
// function that took request input as still tainted, so a caller that must
// feed a redirect target has to walk Names() itself and return the table's
// element — see apps/delivery's mediaPreset.
func Valid(p string) bool {
	_, ok := Widths[p]
	return ok
}
