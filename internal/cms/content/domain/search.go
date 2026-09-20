package domain

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxSearchTextBytes bounds entries.search_text / entries.published_search_text
// (migration 000047). It exists for the same reason MaxRichTextBlocks does: a
// pathological document should not make one row's trigram index entry
// unbounded, and 64 KiB of plain text is already far more than any admin list
// snippet or `q` match needs to work against.
const MaxSearchTextBytes = 64 * 1024

// ExtractSearchText is the PRECISE, schema-aware counterpart to the SQL
// approximation migration 000047 uses for its initial backfill and for the
// bulk field-delete/component-rewrite statements that touch every entry of a
// type in one UPDATE (see that migration's comment for why the two are
// allowed to disagree transiently — every per-row Go write through
// CreateLocalizedEntry, UpdateEntry, RestoreEntryRevision and the publish path
// overwrites the approximation with this).
//
// It walks fields in schema order and pulls text from exactly the field types
// that carry free-form prose: string, text and richtext. Everything else —
// number, boolean, enum, date, datetime, file, relation — carries an
// identifier or a scalar, not something a person searches for by substring,
// so it is skipped rather than stringified. component and dynamiczone fields
// are not text-bearing themselves; ExtractSearchText recurses ONE level into
// their sub-fields (ADR-020 §3, Amendment 1 §2 — neither shape nests further)
// and folds the sub-field text in.
//
// Multiple top-level fields join with "\n"; a richtext field's own blocks join
// with " " (see richTextPlainText) since a paragraph break inside one field is
// not the same kind of boundary as the gap between two different fields.
// Whitespace is then collapsed and the result capped at MaxSearchTextBytes on
// a rune boundary.
//
// payload is the entry's normalised JSON (what validateAndNormalize returns,
// i.e. Entry.Payload / the value about to become it) — a pure function of
// (schema, document), with no I/O and no dependency on anything stateful, so
// it is safe to call from any layer that already holds both.
func ExtractSearchText(fields []Field, payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		// Not this function's job to validate; an unparsable payload simply
		// contributes no search text rather than panicking a write path.
		return ""
	}
	parts := textFromFields(fields, data)
	return normalizeSearchText(strings.Join(parts, "\n"))
}

// textFromFields is ExtractSearchText over one field list and one object; it
// calls itself for a component's or a dynamic zone item's sub-fields, exactly
// as mediaRefsIn (service/content_service.go) recurses over the same two
// shapes for a different extraction.
func textFromFields(fields []Field, data map[string]any) []string {
	var parts []string
	for _, f := range fields {
		v, ok := data[f.Key]
		if !ok || v == nil {
			continue
		}
		switch f.Type {
		case FieldTypeString, FieldTypeText:
			parts = append(parts, stringValues(v)...)
		case FieldTypeRichText:
			if s := richTextPlainText(v); s != "" {
				parts = append(parts, s)
			}
		case FieldTypeComponent:
			for _, item := range componentItems(v) {
				parts = append(parts, textFromFields(f.ComponentFields, item)...)
			}
		case FieldTypeDynamicZone:
			for _, item := range zoneItems(v) {
				name, _ := item[ZoneDiscriminator].(string)
				parts = append(parts, textFromFields(f.ZoneFields[name], item)...)
			}
		}
		// number, boolean, enum, date, datetime, file, relation: no text.
	}
	return parts
}

// stringValues normalises a string/text field's value: a scalar for a plain
// field, or an array of scalars for a Multiple string field (text cannot be
// Multiple — AllowedMultipleTypes — but a stray array is simply ignored
// rather than trusted, since this function does not validate).
func stringValues(v any) []string {
	switch x := v.(type) {
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	case []any:
		out := make([]string, 0, len(x))
		for _, it := range x {
			if s, ok := it.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// componentItems normalises a component field's value to its item objects:
// one object for a plain component field, an array of objects when the field
// is Multiple (ADR-020 §4).
func componentItems(v any) []map[string]any {
	switch x := v.(type) {
	case map[string]any:
		return []map[string]any{x}
	case []any:
		out := make([]map[string]any, 0, len(x))
		for _, it := range x {
			if m, ok := it.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

// zoneItems normalises a dynamic zone field's value — always an array
// (Amendment 1 §2) — to its item objects.
func zoneItems(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, it := range arr {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// richTextPlainText walks a richtext field's block array (richtext.go's
// grammar) and returns every span's text, every code block's code, and every
// image's alt text, in block order, space-joined. It is deliberately as
// permissive as CollectRichTextMediaIDs about shape — it trusts the value has
// already passed ValidateRichText and simply skips anything that does not
// match, rather than erroring — because a search-text extractor that could
// fail a write for a reason unrelated to the value's legality would be a
// second, undocumented validator.
func richTextPlainText(v any) string {
	blocks, ok := v.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case RichTextBlockParagraph, RichTextBlockHeading, RichTextBlockQuote:
			parts = append(parts, spanTexts(block["children"])...)
		case RichTextBlockCode:
			if s, ok := block["code"].(string); ok && s != "" {
				parts = append(parts, s)
			}
		case RichTextBlockList:
			items, _ := block["items"].([]any)
			for _, it := range items {
				spans, _ := it.([]any)
				parts = append(parts, spanTexts(spans)...)
			}
		case RichTextBlockImage:
			if s, ok := block["alt"].(string); ok && s != "" {
				parts = append(parts, s)
			}
			// RichTextBlockDivider and any unrecognised type: no text.
		}
	}
	return strings.Join(parts, " ")
}

// spanTexts pulls the `text` out of a span array (a block's `children`, or
// one list item's spans).
func spanTexts(v any) []string {
	spans, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, sp := range spans {
		span, ok := sp.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := span["text"].(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// reHorizontalSpace collapses runs of spaces/tabs; reMultiNewline collapses
// runs of the "\n" ExtractSearchText inserts between top-level fields (and any
// stray newline inside a value) to one. The column is a match target for
// ILIKE and a trigram index, not a rendering, so the exact whitespace shape
// does not matter beyond staying stable and finite.
var (
	reHorizontalSpace = regexp.MustCompile(`[ \t\r\f\v]+`)
	reMultiNewline    = regexp.MustCompile(`\n[ \t]*(\n[ \t]*)+`)
)

// normalizeSearchText collapses whitespace, trims the ends, and truncates to
// MaxSearchTextBytes on a rune boundary.
func normalizeSearchText(s string) string {
	s = reHorizontalSpace.ReplaceAllString(s, " ")
	s = reMultiNewline.ReplaceAllString(s, "\n")
	s = strings.TrimSpace(s)
	return truncateRunesToBytes(s, MaxSearchTextBytes)
}

// truncateRunesToBytes returns the longest prefix of s whose byte length is
// at most max, cut on a rune boundary so truncation can never split a
// multi-byte character (load-bearing for CJK, where every character is 3
// bytes in UTF-8).
func truncateRunesToBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	total := 0
	for i, r := range s {
		rl := utf8.RuneLen(r)
		if rl < 0 {
			rl = 1
		}
		if total+rl > max {
			return s[:i]
		}
		total += rl
	}
	return s
}
