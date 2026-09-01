package repository

import (
	"regexp"
	"testing"
)

// Pure unit test — runs in every environment (no Docker gate), so the D10
// slug invariants are always enforced.
func TestNewSlug_FormatAndUniqueness(t *testing.T) {
	pattern := regexp.MustCompile(`^t_[a-z0-9]{32}$`)
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		slug, err := newSlug()
		if err != nil {
			t.Fatalf("newSlug: %v", err)
		}
		if !pattern.MatchString(slug) {
			t.Fatalf("slug %q violates ^t_[a-z0-9]{32}$ (D10 opaque format)", slug)
		}
		if _, dup := seen[slug]; dup {
			t.Fatalf("slug %q repeated within 100 draws", slug)
		}
		seen[slug] = struct{}{}
	}
}
