package service

import (
	"io"
	"net/http"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// sniffWindow is the sniff vocabulary size: http.DetectContentType looks at
// at most the first 512 bytes, so reading more would only spend memory.
const sniffWindow = 512

// readSniffWindow reads up to sniffWindow bytes for type detection. Short
// reads are success — a truncated file still has a signature worth checking;
// only a hard read failure is an error.
func readSniffWindow(r io.Reader) ([]byte, error) {
	buf := make([]byte, sniffWindow)
	n, err := io.ReadFull(r, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return buf[:n], nil
}

// sniffSkippedTypes are whitelisted types the stdlib sniffer cannot see.
// image/avif sniffs as application/octet-stream (probed 2026-09-14), so
// enforcing byte equality on it would refuse every legitimate AVIF upload.
// Skipped types keep the declared-type check only; adding one needs a probe
// test like TestSniff_AVIFProbe naming what the stdlib answers instead.
var sniffSkippedTypes = map[string]struct{}{
	"image/avif": {},
}

// sniffEnforced reports whether declared gets byte-checked at completion.
// Non-images are never sniffed (spec P2: container formats keep declared-only
// checks); images are, except the formats in sniffSkippedTypes.
func sniffEnforced(declared string) bool {
	if _, skip := sniffSkippedTypes[declared]; skip {
		return false
	}
	return domain.IsImageContentType(declared)
}

// sniffContentType reports what the leading bytes are, in the same normalized
// form as the allowedUploadTypes keys ("image/png": bare, lowercase, no
// parameters). It is http.DetectContentType plus the normalization the
// reservation path already applies to declared types, so the two answers are
// comparable with ==.
func sniffContentType(head []byte) string {
	return normalizeContentType(http.DetectContentType(head))
}

// bytesMatchDeclared reports whether head is what declared claims to be.
//
// Empty input never matches: DetectContentType of nothing answers text/plain,
// and an object with no signature is not an image of any kind. An empty
// object is therefore refused, not waved through.
func bytesMatchDeclared(head []byte, declared string) bool {
	if len(head) == 0 {
		return false
	}
	return sniffContentType(head) == declared
}
