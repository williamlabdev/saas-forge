package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decode goes through encoding/json rather than hand-built map literals so
// every value carries exactly the types production sees (float64 numbers,
// []any arrays) — a hand literal with an int level would test a shape the
// unmarshaller never produces.
func decode(t *testing.T, s string) any {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal([]byte(s), &v))
	return v
}

// fullDocument exercises every block type and every legal span attribute once.
func fullDocument() string {
	return `[
		{"type":"heading","level":2,"children":[{"text":"Title"}]},
		{"type":"paragraph","children":[
			{"text":"plain "},
			{"text":"bold","marks":["strong"]},
			{"text":"linked","marks":["em","code"],"href":"https://example.com/a"},
			{"text":"relative","href":"/about"},
			{"text":"fragment","href":"#top"},
			{"text":"mail","href":"mailto:x@example.com"}
		]},
		{"type":"quote","children":[{"text":"said"}]},
		{"type":"code","code":"fmt.Println(1)","language":"go"},
		{"type":"code","code":"no language"},
		{"type":"list","style":"bullet","items":[[{"text":"one"}],[{"text":"two","marks":["strike"]}]]},
		{"type":"list","style":"ordered","items":[]},
		{"type":"image","media_id":"7be9f7d8-6d4e-4d38-9e3f-1f1c37a1a111","alt":"a hero"},
		{"type":"image","media_id":"7be9f7d8-6d4e-4d38-9e3f-1f1c37a1a111"},
		{"type":"divider"},
		{"type":"paragraph","children":[]}
	]`
}

func TestValidateRichText_AcceptsTheFullGrammar(t *testing.T) {
	assert.Nil(t, ValidateRichText(decode(t, fullDocument())))
}

func TestValidateRichText_EmptyDocumentIsLegal(t *testing.T) {
	// Emptiness is `required`'s business (the service layer's), not the grammar's.
	assert.Nil(t, ValidateRichText(decode(t, `[]`)))
}

func TestValidateRichText_Refusals(t *testing.T) {
	cases := []struct {
		name     string
		doc      string
		wantPath string
	}{
		{"not an array", `{"type":"paragraph"}`, ""},
		{"block not an object", `["hello"]`, "[0]"},
		{"unknown block type", `[{"type":"table"}]`, "[0].type"},
		{"missing type entirely", `[{"children":[]}]`, "[0].type"},
		{"unknown key on paragraph", `[{"type":"paragraph","children":[],"style":"x"}]`, "[0].style"},
		{"paragraph without children", `[{"type":"paragraph"}]`, "[0].children"},
		{"heading level missing", `[{"type":"heading","children":[]}]`, "[0].level"},
		{"heading level zero", `[{"type":"heading","level":0,"children":[]}]`, "[0].level"},
		{"heading level seven", `[{"type":"heading","level":7,"children":[]}]`, "[0].level"},
		{"heading level fractional", `[{"type":"heading","level":2.5,"children":[]}]`, "[0].level"},
		{"code without code", `[{"type":"code","language":"go"}]`, "[0].code"},
		{"code language with spaces", `[{"type":"code","code":"x","language":"go lang"}]`, "[0].language"},
		{"list style unknown", `[{"type":"list","style":"loose","items":[]}]`, "[0].style"},
		{"list without items", `[{"type":"list","style":"bullet"}]`, "[0].items"},
		{"list item not an array", `[{"type":"list","style":"bullet","items":["one"]}]`, "[0].items[0]"},
		{"image without media_id", `[{"type":"image"}]`, "[0].media_id"},
		{"image media_id not a uuid", `[{"type":"image","media_id":"not-a-uuid"}]`, "[0].media_id"},
		{"image alt not a string", `[{"type":"image","media_id":"7be9f7d8-6d4e-4d38-9e3f-1f1c37a1a111","alt":3}]`, "[0].alt"},
		{"divider with extra key", `[{"type":"divider","children":[]}]`, "[0].children"},
		{"span not an object", `[{"type":"paragraph","children":["x"]}]`, "[0].children[0]"},
		{"span without text", `[{"type":"paragraph","children":[{"marks":[]}]}]`, "[0].children[0].text"},
		{"span with unknown key", `[{"type":"paragraph","children":[{"text":"x","bold":true}]}]`, "[0].children[0].bold"},
		{"unknown mark", `[{"type":"paragraph","children":[{"text":"x","marks":["blink"]}]}]`, "[0].children[0].marks[0]"},
		{"duplicate mark", `[{"type":"paragraph","children":[{"text":"x","marks":["strong","strong"]}]}]`, "[0].children[0].marks[1]"},
		{"marks not an array", `[{"type":"paragraph","children":[{"text":"x","marks":"strong"}]}]`, "[0].children[0].marks"},
		{"javascript href", `[{"type":"paragraph","children":[{"text":"x","href":"javascript:alert(1)"}]}]`, "[0].children[0].href"},
		{"data href", `[{"type":"paragraph","children":[{"text":"x","href":"data:text/html,x"}]}]`, "[0].children[0].href"},
		{"empty href", `[{"type":"paragraph","children":[{"text":"x","href":""}]}]`, "[0].children[0].href"},
		{"span violation inside a list item", `[{"type":"list","style":"bullet","items":[[{"text":1}]]}]`, "[0].items[0][0].text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := ValidateRichText(decode(t, tc.doc))
			require.NotNil(t, v, "expected a violation")
			assert.Equal(t, tc.wantPath, v.Path, "violation must name the offending node (got reason %q)", v.Reason)
		})
	}
}

func TestValidateRichText_BlockCap(t *testing.T) {
	blocks := make([]string, MaxRichTextBlocks+1)
	for i := range blocks {
		blocks[i] = `{"type":"divider"}`
	}
	v := ValidateRichText(decode(t, "["+strings.Join(blocks, ",")+"]"))
	require.NotNil(t, v)
	assert.Contains(t, v.Reason, "too many blocks")

	// Exactly at the cap is legal — the cap is a ceiling, not a strict bound.
	assert.Nil(t, ValidateRichText(decode(t, "["+strings.Join(blocks[:MaxRichTextBlocks], ",")+"]")))
}

func TestValidateRichText_ImageCap(t *testing.T) {
	img := func(i int) string {
		return fmt.Sprintf(`{"type":"image","media_id":"%s"}`, uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprint(i))))
	}
	var blocks []string
	for i := 0; i < MaxRichTextImageBlocks+1; i++ {
		blocks = append(blocks, img(i))
	}
	v := ValidateRichText(decode(t, "["+strings.Join(blocks, ",")+"]"))
	require.NotNil(t, v)
	assert.Contains(t, v.Reason, "too many image blocks")
}

func TestCollectRichTextMediaIDs_DedupesInFirstAppearanceOrder(t *testing.T) {
	a := "7be9f7d8-6d4e-4d38-9e3f-1f1c37a1a111"
	b := "9c0f0e1a-2222-4333-8444-555566667777"
	doc := decode(t, fmt.Sprintf(`[
		{"type":"image","media_id":"%s"},
		{"type":"paragraph","children":[{"text":"x"}]},
		{"type":"image","media_id":"%s"},
		{"type":"image","media_id":"%s"}
	]`, a, b, a))
	ids := CollectRichTextMediaIDs(doc)
	require.Len(t, ids, 2, "the repeated asset must appear once")
	assert.Equal(t, a, ids[0].String())
	assert.Equal(t, b, ids[1].String())
}
