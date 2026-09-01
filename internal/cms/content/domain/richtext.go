package domain

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Rich text is stored as STRUCTURED BLOCK JSON — an array of typed blocks —
// not as a string in any markup language (ADR-010). The grammar is owned here,
// in the domain, because it is a value shape exactly like "a date is
// YYYY-MM-DD": the contract names the type, this file says what a legal value
// is, and every writer goes through ValidateRichText before a byte lands.
//
// The grammar is deliberately FLAT: a document is a sequence of blocks, and no
// block contains another block. Lists carry their items as span sequences, not
// as nested blocks, so v1 cannot express a list inside a quote or a table —
// those are grammar EXTENSIONS (a new block type in the whitelist), not
// reinterpretations of stored values, which is what keeps every extension
// backward-compatible.
//
// Unknown block types, unknown marks and unknown keys are all REFUSED, not
// skipped. Tolerating them would let two writers with different vocabularies
// both "succeed" against the same field and leave the renderer to decide what
// the stored bytes mean — the exact situation a validated grammar exists to
// prevent. Forward compatibility happens by widening the whitelist in a
// deploy, never by storing what the current deploy cannot read.

// Block type names, the v1 whitelist.
const (
	RichTextBlockParagraph = "paragraph"
	RichTextBlockHeading   = "heading"
	RichTextBlockQuote     = "quote"
	RichTextBlockCode      = "code"
	RichTextBlockList      = "list"
	RichTextBlockImage     = "image"
	RichTextBlockDivider   = "divider"
)

// AllowedRichTextBlocks is the legal block-type set, in declaration order.
func AllowedRichTextBlocks() []string {
	return []string{
		RichTextBlockParagraph,
		RichTextBlockHeading,
		RichTextBlockQuote,
		RichTextBlockCode,
		RichTextBlockList,
		RichTextBlockImage,
		RichTextBlockDivider,
	}
}

// AllowedRichTextMarks is the legal span-mark set. `link` is NOT a mark: a link
// carries a target, and a bare string in this list cannot, so it is the span's
// `href` attribute instead — one span, at most one link, which is also the only
// reading a renderer can honour without inventing precedence rules.
func AllowedRichTextMarks() []string {
	return []string{"strong", "em", "underline", "strike", "code"}
}

const (
	// MaxRichTextBlocks bounds STRUCTURE, not bytes — MaxEntryBytes already
	// bounds bytes. 256 KiB of one-character paragraphs is ~4000 blocks, each of
	// which becomes DOM nodes in every renderer and rows in every walker; the cap
	// keeps a pathological document from being a denial of service against the
	// admin form rather than against Postgres.
	MaxRichTextBlocks = 500
	// MaxRichTextImageBlocks exists for the same reason MaxMultipleElements
	// does on relation fields: every image block is an existence check against
	// the media store at write time, so the cap prices the write, not the read.
	MaxRichTextImageBlocks = 50
	// MaxRichTextHrefLen matches the common intermediary limit (2083 rounded
	// down); a longer value is almost always a data: URI or an injection
	// attempt, and both are refused by the scheme whitelist anyway.
	MaxRichTextHrefLen = 2048
	// MaxRichTextCodeLanguageLen bounds a value that is a HINT for syntax
	// highlighting, not content. Nothing legitimate is longer than "objective-c".
	MaxRichTextCodeLanguageLen = 40
)

// MaxRichTextHeadingLevel mirrors HTML's h1–h6; a level the target vocabulary
// cannot express would silently render as something else.
const MaxRichTextHeadingLevel = 6

// RichTextViolation names the first illegal thing found in a rich text value.
// Path is a JSON-pointer-ish locator into the VALUE (e.g. "[3].children[0]"),
// so a 400-line document names the offending node instead of shrugging.
type RichTextViolation struct {
	Path   string
	Reason string
}

func vio(path, format string, args ...any) *RichTextViolation {
	return &RichTextViolation{Path: path, Reason: fmt.Sprintf(format, args...)}
}

// ValidateRichText checks v against the block grammar. nil means legal.
// It validates SHAPE only; whether an image's media_id names a live, uploaded
// asset in the caller's tenant is a stateful question the service answers,
// exactly as it does for relation values.
func ValidateRichText(v any) *RichTextViolation {
	blocks, ok := v.([]any)
	if !ok {
		return vio("", "value must be an array of blocks")
	}
	if len(blocks) > MaxRichTextBlocks {
		return vio("", "too many blocks: %d > %d", len(blocks), MaxRichTextBlocks)
	}
	images := 0
	for i, b := range blocks {
		path := fmt.Sprintf("[%d]", i)
		block, ok := b.(map[string]any)
		if !ok {
			return vio(path, "block must be an object")
		}
		typ, _ := block["type"].(string)
		if typ == RichTextBlockImage {
			images++
			if images > MaxRichTextImageBlocks {
				return vio(path, "too many image blocks: limit %d", MaxRichTextImageBlocks)
			}
		}
		if v := validateBlock(path, typ, block); v != nil {
			return v
		}
	}
	return nil
}

// allowedKeys refuses any key outside the block type's vocabulary. The check
// runs AFTER the per-key validations so the error for a present-but-broken key
// names the real problem, not merely "unknown key".
func allowedKeys(path string, block map[string]any, keys ...string) *RichTextViolation {
	for k := range block {
		legal := k == "type"
		for _, a := range keys {
			if k == a {
				legal = true
				break
			}
		}
		if !legal {
			return vio(path+"."+k, "unknown key for %q block", block["type"])
		}
	}
	return nil
}

func validateBlock(path, typ string, block map[string]any) *RichTextViolation {
	switch typ {
	case RichTextBlockParagraph, RichTextBlockQuote:
		if v := validateChildren(path, block); v != nil {
			return v
		}
		return allowedKeys(path, block, "children")
	case RichTextBlockHeading:
		lvl, ok := block["level"].(float64)
		if !ok || lvl != float64(int(lvl)) || lvl < 1 || lvl > MaxRichTextHeadingLevel {
			return vio(path+".level", "heading level must be an integer 1..%d", MaxRichTextHeadingLevel)
		}
		if v := validateChildren(path, block); v != nil {
			return v
		}
		return allowedKeys(path, block, "level", "children")
	case RichTextBlockCode:
		if _, ok := block["code"].(string); !ok {
			return vio(path+".code", "code block needs a string `code`")
		}
		if lang, present := block["language"]; present {
			s, ok := lang.(string)
			if !ok || s == "" || len(s) > MaxRichTextCodeLanguageLen || strings.ContainsAny(s, " \t\n") {
				return vio(path+".language", "language must be a short token")
			}
		}
		return allowedKeys(path, block, "code", "language")
	case RichTextBlockList:
		style, _ := block["style"].(string)
		if style != "bullet" && style != "ordered" {
			return vio(path+".style", "list style must be bullet or ordered")
		}
		items, ok := block["items"].([]any)
		if !ok {
			return vio(path+".items", "list needs an array of items")
		}
		for j, it := range items {
			spans, ok := it.([]any)
			if !ok {
				return vio(fmt.Sprintf("%s.items[%d]", path, j), "list item must be an array of spans")
			}
			for k, sp := range spans {
				if v := validateSpan(fmt.Sprintf("%s.items[%d][%d]", path, j, k), sp); v != nil {
					return v
				}
			}
		}
		return allowedKeys(path, block, "style", "items")
	case RichTextBlockImage:
		id, _ := block["media_id"].(string)
		if _, err := uuid.Parse(id); err != nil {
			return vio(path+".media_id", "image needs a media asset id")
		}
		if alt, present := block["alt"]; present {
			s, ok := alt.(string)
			if !ok || len(s) > MaxAltTextLen {
				return vio(path+".alt", "alt must be a string of at most %d chars", MaxAltTextLen)
			}
		}
		return allowedKeys(path, block, "media_id", "alt")
	case RichTextBlockDivider:
		return allowedKeys(path, block)
	default:
		return vio(path+".type", "unknown block type %q", typ)
	}
}

func validateChildren(path string, block map[string]any) *RichTextViolation {
	children, ok := block["children"].([]any)
	if !ok {
		return vio(path+".children", "block needs an array of spans")
	}
	for i, sp := range children {
		if v := validateSpan(fmt.Sprintf("%s.children[%d]", path, i), sp); v != nil {
			return v
		}
	}
	return nil
}

func validateSpan(path string, v any) *RichTextViolation {
	span, ok := v.(map[string]any)
	if !ok {
		return vio(path, "span must be an object")
	}
	if _, ok := span["text"].(string); !ok {
		return vio(path+".text", "span needs a string `text`")
	}
	if marks, present := span["marks"]; present {
		xs, ok := marks.([]any)
		if !ok {
			return vio(path+".marks", "marks must be an array")
		}
		seen := make(map[string]struct{}, len(xs))
		for i, m := range xs {
			s, ok := m.(string)
			if !ok || !legalMark(s) {
				return vio(fmt.Sprintf("%s.marks[%d]", path, i), "unknown mark")
			}
			if _, dup := seen[s]; dup {
				return vio(fmt.Sprintf("%s.marks[%d]", path, i), "duplicate mark %q", s)
			}
			seen[s] = struct{}{}
		}
	}
	if href, present := span["href"]; present {
		s, ok := href.(string)
		if !ok || !legalHref(s) {
			return vio(path+".href", "href must be http(s), mailto, or site-relative")
		}
	}
	for k := range span {
		if k != "text" && k != "marks" && k != "href" {
			return vio(path+"."+k, "unknown key for span")
		}
	}
	return nil
}

func legalMark(s string) bool {
	for _, m := range AllowedRichTextMarks() {
		if m == s {
			return true
		}
	}
	return false
}

// legalHref is a WHITELIST of link shapes, not a blacklist of bad ones —
// `javascript:` is refused because it is not on the list, and so is every
// scheme nobody has thought of yet. Site-relative ("/…") and fragment ("#…")
// targets are legal because a document that may only link off-site cannot
// link to its own site's pages.
func legalHref(s string) bool {
	if s == "" || len(s) > MaxRichTextHrefLen {
		return false
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "#") {
		return true
	}
	lower := strings.ToLower(s)
	return strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "mailto:")
}

// CollectRichTextMediaIDs walks a VALIDATED rich text value and returns the
// media ids its image blocks name, deduplicated in first-appearance order.
// Deduplicated because the caller does one existence check and one entry_media
// link per ASSET, and the same hero image pasted twice is one asset, not two.
// It must only run after ValidateRichText — it trusts the shape and ignores
// anything that is not an image block.
func CollectRichTextMediaIDs(v any) []uuid.UUID {
	blocks, ok := v.([]any)
	if !ok {
		return nil
	}
	var ids []uuid.UUID
	seen := make(map[uuid.UUID]struct{})
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok || block["type"] != RichTextBlockImage {
			continue
		}
		raw, _ := block["media_id"].(string)
		id, err := uuid.Parse(raw)
		if err != nil {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}
