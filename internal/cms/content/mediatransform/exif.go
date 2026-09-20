package mediatransform

import "encoding/binary"

// jpegOrientation returns the EXIF Orientation tag (1–8) of a JPEG, or 1
// when the file carries none or the metadata is malformed. Only JPEG carries
// EXIF among the formats the worker decodes; PNG, GIF and WebP are returned
// as stored.
//
// This is a purpose-built reader for one tag rather than a general EXIF
// library: the worker needs the orientation and nothing else, the segment
// walk is thirty lines, and every read below is bounds-checked so a crafted
// header cannot panic the process that every tenant's variants run in. It
// reads IFD0 only — the orientation lives there; a thumbnail IFD's copy is
// not the image's.
//
// Layout, for the reader: SOI (FF D8), then segments of (FF marker, 2-byte
// big-endian length including itself). APP1 (FF E1) whose payload starts
// "Exif\0\0" holds a TIFF header: byte order ("II" | "MM"), 0x002A, then the
// offset of IFD0. IFD0 is a 2-byte count followed by 12-byte entries:
// tag(2) type(2) count(4) value(4); Orientation is tag 0x0112, type SHORT,
// value in the first two bytes of the value field. All TIFF offsets are
// relative to the TIFF header, not to the file.
func jpegOrientation(b []byte) int {
	const (
		soi   = 0xD8
		app1  = 0xE1
		sos   = 0xDA
		eoi   = 0xD9
		tagOr = 0x0112
	)
	if len(b) < 4 || b[0] != 0xFF || b[1] != soi {
		return 1
	}
	i := 2
	for i+4 <= len(b) {
		if b[i] != 0xFF {
			return 1
		}
		marker := b[i+1]
		if marker == 0xFF { // padding
			i++
			continue
		}
		if marker == sos || marker == eoi {
			return 1 // image data begins; no APP1 came first
		}
		segLen := int(binary.BigEndian.Uint16(b[i+2:]))
		if segLen < 2 || i+2+segLen > len(b) {
			return 1
		}
		if marker == app1 {
			return orientationFromExif(b[i+4 : i+2+segLen])
		}
		i += 2 + segLen
	}
	return 1
}

func orientationFromExif(p []byte) int {
	if len(p) < 6 || string(p[:6]) != "Exif\x00\x00" {
		return 1
	}
	t := p[6:] // TIFF header; every offset below is relative to t
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	if bo.Uint16(t[2:]) != 0x2A {
		return 1
	}
	ifd := int(bo.Uint32(t[4:]))
	if ifd < 8 || ifd+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[ifd:]))
	for k := 0; k < n; k++ {
		e := ifd + 2 + k*12
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:]) != 0x0112 {
			continue
		}
		if bo.Uint16(t[e+2:]) != 3 { // SHORT
			return 1
		}
		v := int(bo.Uint16(t[e+8:]))
		if v < 1 || v > 8 {
			return 1
		}
		return v
	}
	return 1
}
