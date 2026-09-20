package mediapreset

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The preset table is a contract with every `?preset=` ever written into a
// template and with the CHECK constraint in cms migration 000043. These tests
// pin the values and the lookups so a "harmless" edit shows up here first.
func TestWidths_TableIsFixed(t *testing.T) {
	assert.Equal(t, map[string]int{"thumb": 320, "small": 640, "medium": 1024, "large": 1920}, Widths)
	assert.Equal(t, []string{"thumb", "small", "medium", "large"}, Names(),
		"names must come back in ascending width order — the DTO and the worker both rely on it")
	prev := 0
	for _, n := range Names() {
		require.Greater(t, Widths[n], prev, "preset %s must be wider than the one before it", n)
		prev = Widths[n]
	}
}

func TestValid(t *testing.T) {
	for _, n := range Names() {
		assert.True(t, Valid(n), n)
	}
	for _, bad := range []string{"", Original, "Thumb", "huge", "thumb "} {
		assert.False(t, Valid(bad), "%q must not validate", bad)
	}
}
