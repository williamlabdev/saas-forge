package mediatransform

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fixtures ----------------------------------------------------------------

// solid returns a w×h RGBA image with a red top-left pixel and a green pixel
// at (1,0): enough to tell every one of the eight orientations apart.
func marked(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{0, 0, 255, 255})
		}
	}
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	img.Set(1, 0, color.RGBA{0, 255, 0, 255})
	return img
}

func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 100}))
	return buf.Bytes()
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// withOrientation splices an APP1/Exif segment carrying Orientation=o right
// after SOI, the way a camera writes it. Little-endian TIFF, one IFD0 entry.
func withOrientation(jpg []byte, o int) []byte {
	tiff := []byte{'I', 'I', 0x2A, 0x00, 8, 0, 0, 0, // header, IFD0 at offset 8
		1, 0, // one entry
		0x12, 0x01, // tag 0x0112
		3, 0, // SHORT
		1, 0, 0, 0, // count 1
		byte(o), 0, 0, 0, // value
		0, 0, 0, 0} // next IFD: none
	payload := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2))
	seg = append(seg, payload...)
	out := append([]byte{}, jpg[:2]...)
	out = append(out, seg...)
	return append(out, jpg[2:]...)
}

// pixelBombPNG is a syntactically valid PNG whose IHDR declares w×h and which
// contains no pixel data at all: 60-odd bytes claiming, say, ten gigapixels.
func pixelBombPNG(w, h uint32) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 2 // 8-bit RGB
	chunk := func(typ string, data []byte) []byte {
		var b []byte
		b = binary.BigEndian.AppendUint32(b, uint32(len(data)))
		b = append(b, typ...)
		b = append(b, data...)
		return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(append([]byte(typ), data...)))
	}
	out := []byte("\x89PNG\r\n\x1a\n")
	out = append(out, chunk("IHDR", ihdr)...)
	return append(out, chunk("IEND", nil)...)
}

func at(img image.Image, x, y int) color.RGBA {
	r, g, b, a := img.At(x, y).RGBA()
	return color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
}

func isRed(c color.RGBA) bool   { return c.R > 200 && c.G < 80 && c.B < 80 }
func isGreen(c color.RGBA) bool { return c.G > 200 && c.R < 80 && c.B < 80 }

// --- EXIF --------------------------------------------------------------------

func TestJPEGOrientation_ReadsAllEightAndDefaultsToOne(t *testing.T) {
	plain := encodeJPEG(t, marked(4, 2))
	assert.Equal(t, 1, jpegOrientation(plain), "no APP1 → 1")
	for o := 1; o <= 8; o++ {
		assert.Equal(t, o, jpegOrientation(withOrientation(plain, o)), "orientation %d", o)
	}
	// Big-endian TIFF is read too.
	be := withOrientation(plain, 6)
	i := bytes.Index(be, []byte("II*\x00"))
	require.Positive(t, i)
	be[i], be[i+1], be[i+2], be[i+3] = 'M', 'M', 0x00, 0x2A
	// swap the remaining 16-bit/32-bit fields to big-endian
	tiff := be[i:]
	binary.BigEndian.PutUint32(tiff[4:], 8)
	binary.BigEndian.PutUint16(tiff[8:], 1)
	binary.BigEndian.PutUint16(tiff[10:], 0x0112)
	binary.BigEndian.PutUint16(tiff[12:], 3)
	binary.BigEndian.PutUint32(tiff[14:], 1)
	binary.BigEndian.PutUint16(tiff[18:], 6)
	assert.Equal(t, 6, jpegOrientation(be))
}

func TestJPEGOrientation_MalformedNeverPanics(t *testing.T) {
	plain := encodeJPEG(t, marked(4, 2))
	full := withOrientation(plain, 6)
	// Every truncation of the file, and a few corruptions, must yield 1 or the
	// real value — never a panic. This is the property that lets a crafted
	// upload sit in a queue every tenant shares.
	for n := 0; n < len(full); n++ {
		v := jpegOrientation(full[:n])
		assert.True(t, v == 1 || v == 6, "truncated at %d: %d", n, v)
	}
	for _, bad := range [][]byte{
		nil, {0xFF}, {0xFF, 0xD8}, {0xFF, 0xD8, 0xFF, 0xE1, 0xFF, 0xFF},
		append([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x10}, []byte("Exif\x00\x00XX*\x00")...),
	} {
		assert.Equal(t, 1, jpegOrientation(bad))
	}
	// An orientation value outside 1..8 is treated as absent.
	weird := withOrientation(plain, 9)
	assert.Equal(t, 1, jpegOrientation(weird))
	// A huge IFD offset pointing past the segment is refused, not chased.
	huge := withOrientation(plain, 6)
	j := bytes.Index(huge, []byte("II*\x00"))
	binary.LittleEndian.PutUint32(huge[j+4:], 0x7FFFFFF0)
	assert.Equal(t, 1, jpegOrientation(huge))
}

// --- orientation transforms --------------------------------------------------

// Each case states where the red (0,0) and green (1,0) markers of a 3×2
// source end up, from the EXIF spec's pictures — not from the implementation.
func TestOrient_AllEightCases(t *testing.T) {
	cases := []struct {
		o          int
		w, h       int
		red, green image.Point
	}{
		{1, 3, 2, image.Pt(0, 0), image.Pt(1, 0)},
		{2, 3, 2, image.Pt(2, 0), image.Pt(1, 0)}, // mirror horizontal
		{3, 3, 2, image.Pt(2, 1), image.Pt(1, 1)}, // rotate 180
		{4, 3, 2, image.Pt(0, 1), image.Pt(1, 1)}, // mirror vertical
		{5, 2, 3, image.Pt(0, 0), image.Pt(0, 1)}, // transpose
		{6, 2, 3, image.Pt(1, 0), image.Pt(1, 1)}, // rotate 90 CW
		{7, 2, 3, image.Pt(1, 2), image.Pt(1, 1)}, // transverse
		{8, 2, 3, image.Pt(0, 2), image.Pt(0, 1)}, // rotate 90 CCW
	}
	for _, c := range cases {
		out := orient(marked(3, 2), c.o)
		b := out.Bounds()
		require.Equal(t, image.Pt(c.w, c.h), image.Pt(b.Dx(), b.Dy()), "orientation %d dims", c.o)
		assert.True(t, isRed(at(out, c.red.X, c.red.Y)), "orientation %d: red at %v, got %v", c.o, c.red, at(out, c.red.X, c.red.Y))
		assert.True(t, isGreen(at(out, c.green.X, c.green.Y)), "orientation %d: green at %v", c.o, c.green)
	}
}

// End to end through the real decoder: a JPEG tagged 6 comes out rotated,
// and its recorded dimensions are the rotated ones.
func TestDecodeSource_AppliesEXIFOrientation(t *testing.T) {
	// Chroma subsampling smears a 1px marker, so the marker here is a whole
	// 8×8 block: the red patch fills the top-left block of a 64×32 source.
	img := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 64; x++ {
			if x < 8 && y < 8 {
				img.Set(x, y, color.RGBA{255, 0, 0, 255})
			} else {
				img.Set(x, y, color.RGBA{0, 0, 255, 255})
			}
		}
	}
	data := withOrientation(encodeJPEG(t, img), 6)
	p, err := probeSource(data, "image/jpeg")
	require.NoError(t, err)
	assert.Equal(t, 6, p.orientation)
	assert.Equal(t, [2]int{32, 64}, [2]int{p.width, p.height}, "probe reports post-rotation dimensions")

	src, err := decodeSource(data, p)
	require.NoError(t, err)
	assert.Equal(t, [2]int{32, 64}, [2]int{src.width, src.height})
	assert.True(t, isRed(at(src.img, 28, 3)), "rotate 90° CW puts the top-left block top-right; got %v", at(src.img, 28, 3))
	assert.False(t, isRed(at(src.img, 3, 3)), "and nothing red remains top-left")
}

// --- probe / limits ----------------------------------------------------------

func TestProbeSource_RefusesAPixelBombFromTheHeader(t *testing.T) {
	bomb := pixelBombPNG(100_000, 100_000)
	require.Less(t, len(bomb), 100, "the fixture must be tiny — that is the point of the attack")
	p, err := probeSource(bomb, "image/png")
	require.ErrorIs(t, err, errTooLarge)
	assert.Equal(t, [2]int{100_000, 100_000}, [2]int{p.width, p.height}, "the header's dimensions are still reported for the original row")

	// Exactly at the limit is allowed; one past it is not.
	_, err = probeSource(pixelBombPNG(10_000, 5_000), "image/png")
	assert.NoError(t, err, "50,000,000 pixels is the ceiling, inclusive")
	_, err = probeSource(pixelBombPNG(10_000, 5_001), "image/png")
	assert.ErrorIs(t, err, errTooLarge)
}

func TestProbeSource_UnsupportedAndCorrupt(t *testing.T) {
	_, err := probeSource([]byte("whatever"), "image/avif")
	assert.ErrorIs(t, err, errUnsupportedFormat, "AVIF is refused before the bytes are looked at")

	_, err = probeSource([]byte("not an image at all"), "image/png")
	require.Error(t, err)
	assert.NotErrorIs(t, err, errTooLarge)
	assert.NotErrorIs(t, err, errUnsupportedFormat)
}

// --- resize + encode ---------------------------------------------------------

func TestFitWidth(t *testing.T) {
	cases := []struct{ w, h, max, ew, eh int }{
		{2000, 1000, 320, 320, 160},
		{1920, 1080, 1024, 1024, 576},
		{3000, 2000, 1920, 1920, 1280},
		{1000, 1, 320, 320, 1},    // never a zero height
		{300, 200, 320, 300, 200}, // never upscale
		{320, 200, 320, 320, 200}, // equal width: untouched
		{1001, 1000, 320, 320, 320},
	}
	for _, c := range cases {
		w, h := fitWidth(c.w, c.h, c.max)
		assert.Equal(t, [2]int{c.ew, c.eh}, [2]int{w, h}, "%dx%d @ %d", c.w, c.h, c.max)
	}
}

func TestRender_FormatRouting(t *testing.T) {
	big := marked(640, 320)
	var gifBuf bytes.Buffer
	require.NoError(t, gif.Encode(&gifBuf, big, nil))

	cases := []struct {
		name, ct, wantCT, wantExt string
		data                      []byte
	}{
		{"jpeg stays jpeg", "image/jpeg", "image/jpeg", "jpg", encodeJPEG(t, big)},
		{"png stays png", "image/png", "image/png", "png", encodePNG(t, big)},
		{"gif becomes png (first frame)", "image/gif", "image/png", "png", gifBuf.Bytes()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := probeSource(c.data, c.ct)
			require.NoError(t, err)
			src, err := decodeSource(c.data, p)
			require.NoError(t, err)
			out, err := render(src, 320)
			require.NoError(t, err)
			assert.Equal(t, c.wantCT, out.contentType)
			assert.Equal(t, c.wantExt, out.ext)
			assert.Equal(t, [2]int{320, 160}, [2]int{out.width, out.height})
			// The bytes decode back to what was promised.
			cfg, format, err := image.DecodeConfig(bytes.NewReader(out.data))
			require.NoError(t, err)
			assert.Equal(t, [2]int{320, 160}, [2]int{cfg.Width, cfg.Height})
			assert.Equal(t, map[string]string{"image/jpeg": "jpeg", "image/png": "png"}[c.wantCT], format)
		})
	}

	// WebP has a decoder and no encoder in pure Go, so it leaves as PNG. The
	// routing is on the probed format name, which is what this pins.
	src := &source{img: big, width: 640, height: 320, format: "webp"}
	out, err := render(src, 320)
	require.NoError(t, err)
	assert.Equal(t, "image/png", out.contentType)
}

func TestRender_StripsEXIF(t *testing.T) {
	data := withOrientation(encodeJPEG(t, marked(640, 320)), 6)
	p, err := probeSource(data, "image/jpeg")
	require.NoError(t, err)
	src, err := decodeSource(data, p)
	require.NoError(t, err)
	out, err := render(src, 320)
	require.NoError(t, err)
	assert.Equal(t, 1, jpegOrientation(out.data), "the rendition carries no EXIF; its pixels are already upright")
	assert.NotContains(t, string(out.data), "Exif", "no APP1 block survives re-encoding — the privacy property in ADR-019 §1")
}

func TestVariantKey(t *testing.T) {
	assert.Equal(t, "t1/abc-deadbeef.thumb.jpg", variantKey("t1/abc-deadbeef", "thumb", "jpg"))
}
