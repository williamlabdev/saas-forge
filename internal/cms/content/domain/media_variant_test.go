package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The preset table is a contract with every `?preset=` ever written into a
// template and with the CHECK constraint in migration 000043. These tests pin
// the values and the two lookups so a "harmless" edit shows up here first.
func TestMediaPresets_TableIsFixed(t *testing.T) {
	assert.Equal(t, map[string]int{"thumb": 320, "small": 640, "medium": 1024, "large": 1920}, MediaPresets)
	assert.Equal(t, []string{"thumb", "small", "medium", "large"}, MediaPresetNames(),
		"names must come back in ascending width order — the DTO and the worker both rely on it")
	prev := 0
	for _, n := range MediaPresetNames() {
		require.Greater(t, MediaPresets[n], prev, "preset %s must be wider than the one before it", n)
		prev = MediaPresets[n]
	}
}

func TestValidMediaPreset(t *testing.T) {
	for _, n := range MediaPresetNames() {
		assert.True(t, ValidMediaPreset(n), n)
	}
	for _, bad := range []string{"", "original", "Thumb", "huge", "thumb "} {
		assert.False(t, ValidMediaPreset(bad), "%q must not validate", bad)
	}
}

func TestIsImageContentType(t *testing.T) {
	for _, ct := range []string{"image/png", "image/jpeg", "image/gif", "image/webp", "image/avif", "IMAGE/JPEG", "image/jpeg; charset=binary"} {
		assert.True(t, IsImageContentType(ct), ct)
	}
	for _, ct := range []string{"application/pdf", "video/mp4", "image/svg+xml", "image/heic", "", "image"} {
		assert.False(t, IsImageContentType(ct), ct)
	}
}
