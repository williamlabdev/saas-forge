package password

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHasher_HashThenVerify(t *testing.T) {
	h := NewHasher()
	encoded, err := h.Hash("correct horse battery staple")
	require.NoError(t, err)
	assert.Contains(t, encoded, "$argon2id$")
	require.NoError(t, h.Verify(encoded, "correct horse battery staple"))
}

func TestHasher_VerifyRejectsWrongPassword(t *testing.T) {
	h := NewHasher()
	encoded, err := h.Hash("s3cret")
	require.NoError(t, err)
	require.Error(t, h.Verify(encoded, "not-it"))
}

func TestHasher_SaltMakesHashesUnique(t *testing.T) {
	h := NewHasher()
	a, err := h.Hash("same")
	require.NoError(t, err)
	b, err := h.Hash("same")
	require.NoError(t, err)
	assert.NotEqual(t, a, b, "random salt must make identical passwords hash differently")
	// Both must still verify.
	require.NoError(t, h.Verify(a, "same"))
	require.NoError(t, h.Verify(b, "same"))
}

func TestHasher_VerifyRejectsMalformedHash(t *testing.T) {
	h := NewHasher()
	for _, bad := range []string{"", "plain", "$argon2id$only$three", "$a$b$c$d$e"} {
		require.Error(t, h.Verify(bad, "x"), "malformed hash %q must error", bad)
	}
}
