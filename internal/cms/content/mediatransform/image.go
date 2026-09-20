package mediatransform

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"

	// Decoders register themselves; the worker never names them. GIF decodes
	// to its first frame, which is what image.Decode returns for it.
	_ "image/gif"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// MaxSourcePixels is the largest image the worker will fully decode. A
// 50-megapixel RGBA frame is 200 MiB in memory; anything larger is a source
// that belongs in a dedicated pipeline, not in the API process. The check is
// made from the header (image.DecodeConfig) BEFORE any pixel is allocated,
// which is what makes a pixel bomb — a 200-byte PNG declaring 100000×100000 —
// cost nothing (ADR-019 §5).
const MaxSourcePixels = 50_000_000

// JPEGQuality is the encoder quality for JPEG variants. 82 is the point on
// libjpeg's curve where further quality is mostly bytes: visually
// indistinguishable from 90 at half to two-thirds the size.
const JPEGQuality = 82

// errUnsupportedFormat is returned by decodeSource when the header names a
// format that is accepted for upload but has no pure-Go decoder here.
var errUnsupportedFormat = errors.New("unsupported image format")

// errTooLarge is returned when the header declares more than MaxSourcePixels.
var errTooLarge = errors.New("source exceeds MaxSourcePixels")

// source is one decoded, already-oriented image ready to be rendered at
// several widths. Width and Height are post-orientation — an EXIF-rotated
// portrait photo reports as portrait, which is what every consumer of the
// dimensions (CSS aspect boxes, the `original` row) wants.
type source struct {
	img    image.Image
	width  int
	height int
	// format is what image.DecodeConfig reported: "jpeg", "png", "gif", "webp".
	format string
}

// probe reads only the header. It is the cheap first pass whose verdict
// decides whether the full decode may run at all.
type probe struct {
	format string
	width  int
	height int
	// orientation is the EXIF tag for JPEG, 1 for everything else.
	orientation int
}

func probeSource(data []byte, contentType string) (probe, error) {
	if !decodable(contentType) {
		return probe{}, errUnsupportedFormat
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return probe{}, fmt.Errorf("decode header: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return probe{}, fmt.Errorf("decode header: %dx%d", cfg.Width, cfg.Height)
	}
	p := probe{format: format, width: cfg.Width, height: cfg.Height, orientation: 1}
	if format == "jpeg" {
		p.orientation = jpegOrientation(data)
	}
	if p.orientation >= 5 {
		p.width, p.height = p.height, p.width
	}
	// Multiply as int64: two int32-range dimensions can overflow an int
	// product on 32-bit builds, and an overflowed check is a bypassed check.
	if int64(cfg.Width)*int64(cfg.Height) > MaxSourcePixels {
		return p, errTooLarge
	}
	return p, nil
}

// decodable lists the upload types with a decoder in this binary. AVIF is
// accepted for upload (ADR-005) and refused here — ADR-019 trigger (b).
func decodable(contentType string) bool {
	switch contentType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	}
	return false
}

// decodeSource does the full decode and applies the EXIF orientation, so
// that every rendition — and the recorded dimensions — describe the picture
// the way a viewer sees it. Re-encoding strips the EXIF block itself, which
// is deliberate: the output carries no GPS fix, no camera serial, no
// timestamp. That is a privacy property of variants, stated in ADR-019 §1.
func decodeSource(data []byte, p probe) (*source, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	img = orient(img, p.orientation)
	b := img.Bounds()
	return &source{img: img, width: b.Dx(), height: b.Dy(), format: p.format}, nil
}

// orient applies EXIF orientation o (1–8) and returns an image whose pixels
// are in display order. 1 is the identity and returns img untouched.
//
// Implemented over an RGBA copy with index arithmetic rather than through
// image.At per pixel: an interface call per pixel on a 50-megapixel frame is
// the difference between a second and a minute, and the per-asset timeout
// is sixty seconds.
func orient(img image.Image, o int) image.Image {
	if o <= 1 || o > 8 {
		return img
	}
	src := toRGBA(img)
	w, h := src.Rect.Dx(), src.Rect.Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2: // mirrored horizontally
				dx, dy = w-1-x, y
			case 3: // rotated 180°
				dx, dy = w-1-x, h-1-y
			case 4: // mirrored vertically
				dx, dy = x, h-1-y
			case 5: // transposed (mirrored across the main diagonal)
				dx, dy = y, x
			case 6: // rotated 90° clockwise
				dx, dy = h-1-y, x
			case 7: // transversed (mirrored across the anti-diagonal)
				dx, dy = h-1-y, w-1-x
			case 8: // rotated 90° counter-clockwise
				dx, dy = y, w-1-x
			}
			si := y*src.Stride + x*4
			di := dy*dst.Stride + dx*4
			copy(dst.Pix[di:di+4], src.Pix[si:si+4])
		}
	}
	return dst
}

func toRGBA(img image.Image) *image.RGBA {
	if r, ok := img.(*image.RGBA); ok && r.Rect.Min == (image.Point{}) {
		return r
	}
	b := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Rect, img, b.Min, draw.Src)
	return dst
}

// fitWidth returns the (width, height) of a rendition bounded to maxWidth,
// preserving aspect ratio. The caller has already decided the source is
// wider than maxWidth; this never upscales and never returns a zero height.
func fitWidth(w, h, maxWidth int) (int, int) {
	if w <= maxWidth {
		return w, h
	}
	// Round to nearest rather than truncate: 1919.6 → 1920 keeps a 16:9 source
	// at 16:9 instead of drifting a pixel on every preset.
	nh := (int64(h)*int64(maxWidth) + int64(w)/2) / int64(w)
	if nh < 1 {
		nh = 1
	}
	return maxWidth, int(nh)
}

// rendition is one encoded variant, ready for the bucket.
type rendition struct {
	data        []byte
	contentType string
	ext         string
	width       int
	height      int
}

// render scales src to fit maxWidth and encodes it in the output format for
// its source format: JPEG stays JPEG (photos; alpha-free; quality-bounded),
// everything else becomes PNG (lossless, keeps transparency). No WebP or
// AVIF output — that is ADR-019 trigger (a).
func render(src *source, maxWidth int) (rendition, error) {
	w, h := fitWidth(src.width, src.height, maxWidth)
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	// CatmullRom for downscaling: a proper reconstruction kernel, so a 6000px
	// photo scaled to 320px does not alias into moiré the way nearest or
	// bilinear does. It is the slowest of the four x/image kernels and still
	// well inside the budget for a 50-megapixel ceiling.
	xdraw.CatmullRom.Scale(dst, dst.Rect, src.img, src.img.Bounds(), xdraw.Src, nil)

	var buf bytes.Buffer
	r := rendition{width: w, height: h}
	switch src.format {
	case "jpeg":
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: JPEGQuality}); err != nil {
			return rendition{}, fmt.Errorf("encode jpeg: %w", err)
		}
		r.contentType, r.ext = "image/jpeg", "jpg"
	default:
		if err := png.Encode(&buf, dst); err != nil {
			return rendition{}, fmt.Errorf("encode png: %w", err)
		}
		r.contentType, r.ext = "image/png", "png"
	}
	r.data = buf.Bytes()
	return r, nil
}
