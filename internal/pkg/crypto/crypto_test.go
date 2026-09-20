package crypto

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func key32() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func TestAESGCM_RoundTrip(t *testing.T) {
	enc, err := NewAESGCMEncryptor(key32())
	require.NoError(t, err)

	plain := []byte("sensitive-pii@example.com")
	ct, nonce, err := enc.Encrypt(plain)
	require.NoError(t, err)
	assert.NotEqual(t, plain, ct)

	got, err := enc.Decrypt(ct, nonce)
	require.NoError(t, err)
	assert.Equal(t, plain, got)
}

func TestAESGCM_RejectsBadKeySize(t *testing.T) {
	_, err := NewAESGCMEncryptor(make([]byte, 16))
	require.Error(t, err)
}

func TestAESGCM_NonceIsUniquePerEncrypt(t *testing.T) {
	enc, err := NewAESGCMEncryptor(key32())
	require.NoError(t, err)
	plain := []byte("same input")

	ct1, n1, err := enc.Encrypt(plain)
	require.NoError(t, err)
	ct2, n2, err := enc.Encrypt(plain)
	require.NoError(t, err)

	assert.False(t, bytes.Equal(n1, n2), "nonce must differ per call")
	assert.False(t, bytes.Equal(ct1, ct2), "ciphertext must differ per call")
}

func TestAESGCM_DecryptRejectsWrongNonceSize(t *testing.T) {
	enc, err := NewAESGCMEncryptor(key32())
	require.NoError(t, err)
	ct, _, err := enc.Encrypt([]byte("x"))
	require.NoError(t, err)

	_, err = enc.Decrypt(ct, []byte("short"))
	require.Error(t, err)
}

func TestAESGCM_DecryptRejectsTamperedCiphertext(t *testing.T) {
	enc, err := NewAESGCMEncryptor(key32())
	require.NoError(t, err)
	ct, nonce, err := enc.Encrypt([]byte("authenticated"))
	require.NoError(t, err)

	ct[0] ^= 0xFF // flip a byte; GCM auth tag must reject
	_, err = enc.Decrypt(ct, nonce)
	require.Error(t, err)
}

func TestAESGCM_DecryptRejectsWrongKey(t *testing.T) {
	enc, err := NewAESGCMEncryptor(key32())
	require.NoError(t, err)
	ct, nonce, err := enc.Encrypt([]byte("secret"))
	require.NoError(t, err)

	otherKey := key32()
	otherKey[0] ^= 0xFF
	other, err := NewAESGCMEncryptor(otherKey)
	require.NoError(t, err)
	_, err = other.Decrypt(ct, nonce)
	require.Error(t, err)
}

func TestBlindIndexer_DeterministicAndKeyed(t *testing.T) {
	pepper := bytes.Repeat([]byte("p"), 32)
	idx, err := NewHMACBlindIndexer(pepper)
	require.NoError(t, err)

	a, err := idx.Index("user@example.com")
	require.NoError(t, err)
	b, err := idx.Index("user@example.com")
	require.NoError(t, err)
	assert.Equal(t, a, b, "same input must yield same index")

	c, err := idx.Index("other@example.com")
	require.NoError(t, err)
	assert.NotEqual(t, a, c, "different input must yield different index")

	// A different pepper must change the index for the same input.
	pepper2 := bytes.Repeat([]byte("q"), 32)
	idx2, err := NewHMACBlindIndexer(pepper2)
	require.NoError(t, err)
	d, err := idx2.Index("user@example.com")
	require.NoError(t, err)
	assert.NotEqual(t, a, d, "different pepper must yield different index")
}

func TestBlindIndexer_RejectsShortPepper(t *testing.T) {
	_, err := NewHMACBlindIndexer(make([]byte, 8))
	require.Error(t, err)
}
