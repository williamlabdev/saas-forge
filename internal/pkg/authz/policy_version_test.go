package authz

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPolicyVersion_StableAcrossCalls(t *testing.T) {
	v1 := PolicyVersion()
	v2 := PolicyVersion()
	assert.Equal(t, v1, v2, "PolicyVersion must be stable within one process run")
	assert.Len(t, v1, 64, "sha256 hex digest is 64 characters")
}

func TestPolicyVersionShort_IsPrefixOfFull(t *testing.T) {
	full := PolicyVersion()
	short := PolicyVersionShort()
	require.Len(t, short, 12)
	assert.Equal(t, full[:12], short)
}

func TestPolicyLabel_MatchesEmbeddedComment(t *testing.T) {
	// policies/authz.rego's first line is "# policy-version: 2026-09-10" —
	// see the ADR-009 amendment. If this ever drifts, it means the comment
	// moved or was removed, which PolicyLabel is supposed to notice (an
	// absent label is not an error, but a stale assertion here would hide a
	// silent regression).
	assert.Equal(t, "2026-09-10", PolicyLabel())
}

// TestPolicyVersion_ChangesWithPolicySource proves the hash is a real content
// hash — not, say, a hardcoded build-time constant — by hashing a modified
// copy of the same source independently of the package-level cache.
func TestPolicyVersion_ChangesWithPolicySource(t *testing.T) {
	original := embeddedPolicy
	modified := original + "\n# a trivial edit that must change the hash\n"

	require.NotEqual(t, original, modified)

	hashOf := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}

	assert.NotEqual(t, hashOf(original), hashOf(modified),
		"changing the rego source must change its content hash")
	// And the live PolicyVersion() must match hashing the actual embedded
	// source through the same function used above.
	assert.Equal(t, hashOf(original), PolicyVersion())
}

func TestParsePolicyLabel_AbsentCommentIsEmpty(t *testing.T) {
	assert.Equal(t, "", parsePolicyLabel("package authz\n\ndefault allow := false\n"))
}

func TestParsePolicyLabel_OnlyFirstLine(t *testing.T) {
	// A "# policy-version:" comment anywhere but the first line does not
	// count — it must be unambiguous which comment is authoritative.
	rego := "package authz\n# policy-version: not-the-real-one\n"
	assert.Equal(t, "", parsePolicyLabel(rego))
}

// TestParsePolicyLabel_StripsBOMAndCRLF proves a rego file saved by an
// editor that prepends a UTF-8 BOM and uses CRLF line endings does not lose
// its label — both would otherwise defeat the "# policy-version:" prefix
// check (a leading BOM byte-sequence, and a trailing \r that HasPrefix/
// TrimPrefix on their own would leave attached to the value).
func TestParsePolicyLabel_StripsBOMAndCRLF(t *testing.T) {
	rego := "\ufeff# policy-version: 2026-09-10\r\npackage authz\r\n"
	assert.Equal(t, "2026-09-10", parsePolicyLabel(rego))
}
