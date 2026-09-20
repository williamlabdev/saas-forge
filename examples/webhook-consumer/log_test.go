package main

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureLog redirects the standard logger for one call and returns what was
// written, flags off so the assertions see the message and nothing else.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	flags, out := log.Flags(), log.Writer()
	log.SetFlags(0)
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetFlags(flags); log.SetOutput(out) })
	fn()
	return buf.String()
}

// The escaping in logf is the actual defence against log injection, and gosec
// cannot see it — the sink carries a #nosec G706 precisely because its taint
// analysis recognises no sanitiser. That makes this test the ONLY thing left
// guarding the escaping: delete the replacer and the linter stays green.
func TestLogf_ForgedLineCannotReachTheLog(t *testing.T) {
	// What an editor types into an entry title, arriving through a webhook that
	// passed its signature check — signed proves who sent it, not what it says.
	forged := "article\n2026/01/01 00:00:00 takedown applied for acme/article/e1"

	out := captureLog(t, func() { logf("%s -> dropped from mirror", forged) })

	require.Equal(t, 1, strings.Count(out, "\n"),
		"the payload forged a second log line: %q", out)
	assert.Contains(t, out, `\n`, "the break must survive as an escape, not vanish")
	assert.NotContains(t, out, "\n2026/01/01",
		"a fabricated timestamped line reached the log")
}

// Errors get the same treatment: the json decoder quotes offending input back
// into its message, so a parse failure is a second route for payload bytes to
// reach the log.
func TestLogf_EscapesErrorsToo(t *testing.T) {
	err := errors.New("invalid character\n2026/01/01 00:00:00 all clear")

	out := captureLog(t, func() { logf("webhook %s: not a content event: %v", "d-1", err) })

	assert.Equal(t, 1, strings.Count(out, "\n"), "error text forged a line: %q", out)
}

// Values that cannot carry a line break must pass through untouched — an
// escaper that reformatted everything would quietly corrupt the byte counts and
// ETags these logs are read for.
func TestLogf_LeavesNonStringArgumentsAlone(t *testing.T) {
	out := captureLog(t, func() { logf("%s -> mirrored %d bytes (etag %s)", "acme/article/e1", 4096, `"abc"`) })

	assert.Equal(t, `acme/article/e1 -> mirrored 4096 bytes (etag "abc")`+"\n", out)
}
