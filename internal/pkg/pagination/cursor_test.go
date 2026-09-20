package pagination

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeDecodeUserCursor(t *testing.T) {
	id := uuid.New()
	at := time.Now().UTC().Truncate(time.Microsecond)
	raw, err := EncodeUserCursor(UserCursor{CreatedAt: at, ID: id})
	require.NoError(t, err)

	got, err := DecodeUserCursor(raw)
	require.NoError(t, err)
	assert.Equal(t, id, got.ID)
	assert.True(t, at.Equal(got.CreatedAt))
}

func TestClampLimit(t *testing.T) {
	assert.Equal(t, 20, ClampLimit(0))
	assert.Equal(t, 100, ClampLimit(500))
	assert.Equal(t, 10, ClampLimit(10))
}
