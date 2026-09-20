package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Canned byte heads: only the magic each format needs, not valid files.
// The sniff window never decodes — variants worker owns "is this broken".

var (
	headPNG  = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 13}
	headJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0}
	headGIF  = []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00")
	headWebP = []byte{'R', 'I', 'F', 'F', 0x24, 0, 0, 0, 'W', 'E', 'B', 'P', 'V', 'P', '8', ' '}
	headAVIF = []byte{0, 0, 0, 0x1c, 'f', 't', 'y', 'p', 'a', 'v', 'i', 'f', 0, 0, 0, 0}
	headHTML = []byte("<html><head><script>alert(1)</script></head></html>")
	headPDF  = []byte("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
)

func TestSniff_MatchingPairsPass(t *testing.T) {
	for declared, head := range map[string][]byte{
		"image/png": headPNG, "image/jpeg": headJPEG,
		"image/gif": headGIF, "image/webp": headWebP,
	} {
		require.True(t, bytesMatchDeclared(head, declared), "declared %s", declared)
	}
}

func TestSniff_HTMLPosingAsImageFails(t *testing.T) {
	for _, declared := range []string{"image/png", "image/jpeg", "image/gif", "image/webp"} {
		require.False(t, bytesMatchDeclared(headHTML, declared), "declared %s", declared)
	}
}

func TestSniff_CrossFormatFails(t *testing.T) {
	require.False(t, bytesMatchDeclared(headJPEG, "image/png"), "png-declared jpeg bytes must fail (strict)")
	require.False(t, bytesMatchDeclared(headPNG, "image/jpeg"))
}

func TestSniff_EmptyFails(t *testing.T) {
	require.False(t, bytesMatchDeclared(nil, "image/png"))
	require.False(t, bytesMatchDeclared([]byte{}, "image/png"))
}

func TestSniff_AVIFProbe(t *testing.T) {
	t.Logf("stdlib sees AVIF head as %q", sniffContentType(headAVIF))
}
