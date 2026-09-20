package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func extract(t *testing.T, fields []Field, payload any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return ExtractSearchText(fields, raw)
}

func TestExtractSearchText_StringAndText(t *testing.T) {
	fields := []Field{
		{Key: "title", Type: FieldTypeString},
		{Key: "body", Type: FieldTypeText},
	}
	got := extract(t, fields, map[string]any{"title": "Hello 世界", "body": "some text"})
	want := "Hello 世界\nsome text"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExtractSearchText_MultipleString(t *testing.T) {
	fields := []Field{{Key: "tags", Type: FieldTypeString, Multiple: true}}
	got := extract(t, fields, map[string]any{"tags": []any{"ai", "native", "中文"}})
	// Each element of a Multiple string field is its own "part", so elements
	// join the same way separate top-level fields do ("\n"), not with a space
	// — "ai-native" as two tags must not become indistinguishable from one
	// tag spelled "ai native".
	want := "ai\nnative\n中文"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExtractSearchText_SkippedTypes(t *testing.T) {
	fields := []Field{
		{Key: "n", Type: FieldTypeNumber},
		{Key: "b", Type: FieldTypeBoolean},
		{Key: "e", Type: FieldTypeEnum},
		{Key: "d", Type: FieldTypeDate},
		{Key: "dt", Type: FieldTypeDateTime},
		{Key: "f", Type: FieldTypeFile},
		{Key: "r", Type: FieldTypeRelation},
		{Key: "keep", Type: FieldTypeString},
	}
	got := extract(t, fields, map[string]any{
		"n": 42, "b": true, "e": "draft", "d": "2026-01-01", "dt": "2026-01-01T00:00:00Z",
		"f": "11111111-1111-1111-1111-111111111111", "r": "22222222-2222-2222-2222-222222222222",
		"keep": "only me",
	})
	if got != "only me" {
		t.Fatalf("got %q, want %q", got, "only me")
	}
}

func TestExtractSearchText_RichText_AllBlockTypes(t *testing.T) {
	fields := []Field{{Key: "body", Type: FieldTypeRichText}}
	blocks := []any{
		map[string]any{"type": RichTextBlockParagraph, "children": []any{
			map[string]any{"text": "para one"},
			map[string]any{"text": "中文段落", "marks": []any{"strong"}},
		}},
		map[string]any{"type": RichTextBlockHeading, "level": float64(2), "children": []any{
			map[string]any{"text": "A Heading"},
		}},
		map[string]any{"type": RichTextBlockQuote, "children": []any{
			map[string]any{"text": "a quote"},
		}},
		map[string]any{"type": RichTextBlockCode, "code": "fmt.Println(\"hi\")", "language": "go"},
		map[string]any{"type": RichTextBlockList, "style": "bullet", "items": []any{
			[]any{map[string]any{"text": "item one"}},
			[]any{map[string]any{"text": "item two"}, map[string]any{"text": "cont"}},
		}},
		map[string]any{"type": RichTextBlockImage, "media_id": "33333333-3333-3333-3333-333333333333", "alt": "a photo of 貓"},
		map[string]any{"type": RichTextBlockDivider},
	}
	got := extract(t, fields, map[string]any{"body": blocks})
	for _, want := range []string{
		"para one", "中文段落", "A Heading", "a quote", "fmt.Println(\"hi\")",
		"item one", "item two", "cont", "a photo of 貓",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected extracted text to contain %q; got %q", want, got)
		}
	}
	if strings.Contains(got, "language") {
		t.Fatalf("code block's `language` key must not leak into extracted text: %q", got)
	}
}

func TestExtractSearchText_RichText_UnknownBlockIgnored(t *testing.T) {
	fields := []Field{{Key: "body", Type: FieldTypeRichText}}
	blocks := []any{
		map[string]any{"type": "totally-unknown", "whatever": "nope"},
		map[string]any{"type": RichTextBlockParagraph, "children": []any{
			map[string]any{"text": "kept"},
		}},
	}
	got := extract(t, fields, map[string]any{"body": blocks})
	if got != "kept" {
		t.Fatalf("got %q, want %q", got, "kept")
	}
}

func TestExtractSearchText_Component_Single(t *testing.T) {
	fields := []Field{
		{
			Key:  "hero",
			Type: FieldTypeComponent,
			ComponentFields: []Field{
				{Key: "headline", Type: FieldTypeString},
				{Key: "count", Type: FieldTypeNumber},
			},
		},
	}
	got := extract(t, fields, map[string]any{
		"hero": map[string]any{"headline": "Big Sale 大特價", "count": float64(3)},
	})
	if got != "Big Sale 大特價" {
		t.Fatalf("got %q, want %q", got, "Big Sale 大特價")
	}
}

func TestExtractSearchText_Component_Multiple(t *testing.T) {
	fields := []Field{
		{
			Key:      "cards",
			Type:     FieldTypeComponent,
			Multiple: true,
			ComponentFields: []Field{
				{Key: "label", Type: FieldTypeString},
			},
		},
	}
	got := extract(t, fields, map[string]any{
		"cards": []any{
			map[string]any{"label": "first 一"},
			map[string]any{"label": "second 二"},
		},
	})
	want := "first 一\nsecond 二"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExtractSearchText_DynamicZone(t *testing.T) {
	fields := []Field{
		{
			Key:            "sections",
			Type:           FieldTypeDynamicZone,
			ZoneComponents: []string{"text_block", "quote_block"},
			ZoneFields: map[string][]Field{
				"text_block":  {{Key: "body", Type: FieldTypeText}},
				"quote_block": {{Key: "quote", Type: FieldTypeString}, {Key: "author", Type: FieldTypeString}},
			},
		},
	}
	got := extract(t, fields, map[string]any{
		"sections": []any{
			map[string]any{ZoneDiscriminator: "text_block", "body": "first section 第一段"},
			map[string]any{ZoneDiscriminator: "quote_block", "quote": "carpe diem", "author": "Horace 賀拉斯"},
		},
	})
	want := "first section 第一段\ncarpe diem\nHorace 賀拉斯"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExtractSearchText_DynamicZone_UnknownComponentIgnored(t *testing.T) {
	fields := []Field{
		{
			Key:  "sections",
			Type: FieldTypeDynamicZone,
			ZoneFields: map[string][]Field{
				"text_block": {{Key: "body", Type: FieldTypeText}},
			},
		},
	}
	got := extract(t, fields, map[string]any{
		"sections": []any{
			map[string]any{ZoneDiscriminator: "not_declared", "body": "should be skipped since no fields known"},
		},
	})
	if got != "" {
		t.Fatalf("got %q, want empty (unknown zone component has no known sub-fields)", got)
	}
}

func TestExtractSearchText_NestedComponentInsideZone(t *testing.T) {
	fields := []Field{
		{
			Key:  "sections",
			Type: FieldTypeDynamicZone,
			ZoneFields: map[string][]Field{
				"gallery": {
					{
						Key:      "items",
						Type:     FieldTypeComponent,
						Multiple: true,
						ComponentFields: []Field{
							{Key: "caption", Type: FieldTypeString},
						},
					},
				},
			},
		},
	}
	got := extract(t, fields, map[string]any{
		"sections": []any{
			map[string]any{
				ZoneDiscriminator: "gallery",
				"items": []any{
					map[string]any{"caption": "one"},
					map[string]any{"caption": "two 二"},
				},
			},
		},
	})
	want := "one\ntwo 二"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExtractSearchText_MultiFieldJoin(t *testing.T) {
	fields := []Field{
		{Key: "a", Type: FieldTypeString},
		{Key: "b", Type: FieldTypeString},
		{Key: "c", Type: FieldTypeString},
	}
	got := extract(t, fields, map[string]any{"a": "one", "b": "two", "c": "three"})
	want := "one\ntwo\nthree"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExtractSearchText_MissingAndEmptyFieldsSkipped(t *testing.T) {
	fields := []Field{
		{Key: "a", Type: FieldTypeString},
		{Key: "b", Type: FieldTypeString},
		{Key: "c", Type: FieldTypeString},
	}
	got := extract(t, fields, map[string]any{"a": "", "c": "kept"})
	if got != "kept" {
		t.Fatalf("got %q, want %q", got, "kept")
	}
}

func TestExtractSearchText_WhitespaceCollapsed(t *testing.T) {
	fields := []Field{{Key: "a", Type: FieldTypeString}}
	got := extract(t, fields, map[string]any{"a": "  lots   of\t\tspace  \n\n and newlines  "})
	want := "lots of space \nand newlines"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExtractSearchText_EmptyPayload(t *testing.T) {
	fields := []Field{{Key: "a", Type: FieldTypeString}}
	if got := ExtractSearchText(fields, nil); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
	if got := ExtractSearchText(fields, json.RawMessage("{}")); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestExtractSearchText_TruncatesAtByteCapOnRuneBoundary(t *testing.T) {
	fields := []Field{{Key: "a", Type: FieldTypeString}}
	// Each "貓" is 3 bytes in UTF-8; comfortably overflow the 64KB cap.
	long := strings.Repeat("貓", MaxSearchTextBytes)
	got := extract(t, fields, map[string]any{"a": long})
	if len(got) > MaxSearchTextBytes {
		t.Fatalf("got %d bytes, want <= %d", len(got), MaxSearchTextBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncated output is not valid UTF-8 (rune split)")
	}
	if len(got) == 0 {
		t.Fatalf("expected non-empty truncated output")
	}
}
