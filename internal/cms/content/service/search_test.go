package service

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestSnippetFor_EmptyText(t *testing.T) {
	require.Equal(t, "", SnippetFor("", "hello"))
}

func TestSnippetFor_NoTermFallsBackToPrefix(t *testing.T) {
	text := strings.Repeat("a", 200)
	got := SnippetFor(text, "")
	require.Equal(t, strings.Repeat("a", snippetFallbackWidth), got)
}

func TestSnippetFor_NoMatchFallsBackToPrefix(t *testing.T) {
	text := strings.Repeat("a", 200)
	got := SnippetFor(text, "zzz")
	require.Equal(t, strings.Repeat("a", snippetFallbackWidth), got)
}

func TestSnippetFor_ShortTextReturnedWhole(t *testing.T) {
	require.Equal(t, "hello world", SnippetFor("hello world", "world"))
}

func TestSnippetFor_CaseInsensitiveMatch(t *testing.T) {
	got := SnippetFor("Hello World", "world")
	require.Contains(t, strings.ToLower(got), "world")
}

func TestSnippetFor_WindowAroundMatch(t *testing.T) {
	// Match sits far enough from both ends that the full ±60-rune window fits.
	prefix := strings.Repeat("x", 100)
	suffix := strings.Repeat("y", 100)
	text := prefix + "NEEDLE" + suffix
	got := SnippetFor(text, "NEEDLE")
	require.Contains(t, got, "NEEDLE")
	// Window should be roughly 60 (before) + 6 (match) + 60 (after) = 126 runes.
	require.LessOrEqual(t, utf8.RuneCountInString(got), snippetHalfWidth*2+len("NEEDLE")+1)
	require.True(t, strings.HasSuffix(got, strings.Repeat("y", snippetHalfWidth)))
}

func TestSnippetFor_MatchNearStart(t *testing.T) {
	text := "NEEDLE" + strings.Repeat("y", 200)
	got := SnippetFor(text, "NEEDLE")
	require.True(t, strings.HasPrefix(got, "NEEDLE"))
}

func TestSnippetFor_RuneSafeAroundCJK(t *testing.T) {
	// A CJK-heavy text where a byte-index slice would split a multi-byte rune;
	// SnippetFor must return valid UTF-8 either way.
	text := strings.Repeat("貓", 80) + "關鍵字" + strings.Repeat("狗", 80)
	got := SnippetFor(text, "關鍵字")
	require.True(t, utf8.ValidString(got))
	require.Contains(t, got, "關鍵字")
}

func TestSnippetFor_MatchNearEndClampsWindow(t *testing.T) {
	text := strings.Repeat("x", 10) + "NEEDLE"
	got := SnippetFor(text, "NEEDLE")
	require.True(t, strings.HasSuffix(got, "NEEDLE"))
	require.True(t, utf8.ValidString(got))
}
