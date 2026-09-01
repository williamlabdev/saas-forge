package idempotency

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeKey(t *testing.T) {
	_, err := NormalizeKey("short")
	require.Error(t, err)

	key, err := NormalizeKey("  valid-key_01  ")
	require.NoError(t, err)
	assert.Equal(t, "valid-key_01", key)

	empty, err := NormalizeKey("   ")
	require.NoError(t, err)
	assert.Empty(t, empty)
}
