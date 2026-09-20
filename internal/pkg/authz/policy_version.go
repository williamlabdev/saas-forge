package authz

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
)

// policyLabelPrefix marks an optional, human-chosen first-line comment in the
// embedded Rego source, e.g. "# policy-version: 2026-09-10". It exists next to
// the content hash (PolicyVersion), not instead of it: the hash is precise but
// opaque in a log line, the label is readable but only as trustworthy as
// whoever last edited the comment — see policies/authz.rego line 1.
const policyLabelPrefix = "# policy-version:"

var (
	policyVersionOnce  sync.Once
	policyVersionFull  string
	policyVersionShort string
	policyLabelValue   string
)

// PolicyVersion returns the full sha256 hex digest of the embedded Rego
// policy bytes (policies/authz.rego, via policy_embed.go). It changes exactly
// when the policy source changes, which is what lets a deny logged with one
// version be correlated back to the rego that produced it (decision log,
// ADR-009 Amendment 2026-09-10).
//
// Computed once, lazily, and cached: the embedded string never changes at
// runtime, so hashing it on every decision would be pure waste.
func PolicyVersion() string {
	computePolicyVersion()
	return policyVersionFull
}

// PolicyVersionShort returns the first 12 hex characters of PolicyVersion —
// enough to disambiguate policy revisions in a log line without spending 64
// characters on it.
func PolicyVersionShort() string {
	computePolicyVersion()
	return policyVersionShort
}

// PolicyLabel returns the optional "# policy-version: X" first-line comment
// from the embedded Rego source, or "" if the source has no such comment (an
// absent label is not an error — the hash alone is still a valid version).
func PolicyLabel() string {
	computePolicyVersion()
	return policyLabelValue
}

func computePolicyVersion() {
	policyVersionOnce.Do(func() {
		sum := sha256.Sum256([]byte(embeddedPolicy))
		policyVersionFull = hex.EncodeToString(sum[:])
		policyVersionShort = policyVersionFull
		if len(policyVersionFull) > 12 {
			policyVersionShort = policyVersionFull[:12]
		}
		policyLabelValue = parsePolicyLabel(embeddedPolicy)
	})
}

// utf8BOM is the UTF-8 byte-order mark (U+FEFF, encoded as the three bytes
// EF BB BF) some editors write as an invisible prefix. Written as an escape
// rather than the literal rune so it stays visible/greppable in source.
// Without stripping it, "<BOM># policy-version: X" fails the HasPrefix check
// below — the label goes silently missing, not erroring — so a rego file
// saved with a BOM would look like it has no label at all.
const utf8BOM = "\ufeff"

func parsePolicyLabel(rego string) string {
	rego = strings.TrimPrefix(rego, utf8BOM)
	firstLine, _, _ := strings.Cut(rego, "\n")
	firstLine = strings.TrimSpace(firstLine)
	if !strings.HasPrefix(firstLine, policyLabelPrefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(firstLine, policyLabelPrefix))
}
