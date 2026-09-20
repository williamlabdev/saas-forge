package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// Everything except the media half fires in buildField / validatePayload /
// parseFilter / parseSort — before any repository call — so none of it depends
// on the fake repo mirroring Postgres (see multivalue_test.go for the rule).
// The media half exercises collectMediaRefs against memRepo's asset store,
// which is the same seam every `file`-field test already trusts.

func articleWithBody(required bool) CreateTypeInput {
	return CreateTypeInput{
		Name:  "article",
		Label: "Article",
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "body", Type: domain.FieldTypeRichText, Required: required},
		},
	}
}

func rtDoc(t *testing.T, s string) any {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal([]byte(s), &v))
	return v
}

func TestRichText_RoundTripsThroughAnEntry(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, articleWithBody(false))
	require.NoError(t, err)

	body := `[{"type":"heading","level":1,"children":[{"text":"T"}]},{"type":"paragraph","children":[{"text":"b","marks":["strong"]}]}]`
	got, err := svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{
		"title": "T", "body": rtDoc(t, body),
	}))
	require.NoError(t, err)

	var data map[string]any
	require.NoError(t, json.Unmarshal(got.Data, &data))
	assert.Equal(t, rtDoc(t, body), data["body"], "the stored document must be byte-equivalent to what was sent")
}

func TestRichText_GrammarViolationNamesThePath(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, articleWithBody(false))
	require.NoError(t, err)

	_, err = svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{
		"title": "T",
		"body":  rtDoc(t, `[{"type":"paragraph","children":[{"text":"x","marks":["blink"]}]}]`),
	}))
	assert.Equal(t, "CONTENT_RICHTEXT_INVALID", codeOf(t, err))
	d := detailsOf(t, err)
	assert.Equal(t, "body", d["field"])
	assert.Equal(t, "[0].children[0].marks[0]", d["path"], "the error must name the offending node, not just the field")
}

func TestRichText_AStringValueIsRefused(t *testing.T) {
	// The one shape people WILL send: markdown-in-a-string. The refusal must be
	// the rich-text error (which says "array of blocks"), because a bare
	// TYPE_MISMATCH invites re-sending the same string with different quoting.
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, articleWithBody(false))
	require.NoError(t, err)

	_, err = svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{
		"title": "T", "body": "# not blocks",
	}))
	assert.Equal(t, "CONTENT_RICHTEXT_INVALID", codeOf(t, err))
}

func TestRichText_RequiredRefusesTheEmptyDocument(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, articleWithBody(true))
	require.NoError(t, err)

	_, err = svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{
		"title": "T", "body": []any{},
	}))
	assert.Equal(t, "CONTENT_FIELD_REQUIRED", codeOf(t, err),
		"an empty document must not satisfy `required` — same rule as an empty multi-value array")
}

func TestRichText_OptionalEmptyDocumentRoundTripsAsEmpty(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, articleWithBody(false))
	require.NoError(t, err)

	got, err := svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{
		"title": "T", "body": []any{},
	}))
	require.NoError(t, err)
	assert.Contains(t, string(got.Data), `"body":[]`)
}

func TestRichText_MultipleIsRefused(t *testing.T) {
	svc, _ := newSvc()
	_, err := svc.CreateContentType(ctxTenant("tenant-a"), CreateTypeInput{
		Name:   "article",
		Fields: []FieldInput{{Key: "body", Type: domain.FieldTypeRichText, Multiple: true}},
	})
	assert.Equal(t, "CONTENT_FIELD_MULTIPLE_UNSUPPORTED", codeOf(t, err),
		"a rich text value is already a sequence; `multiple` has no meaning on it")
}

func TestRichText_EveryFilterOpIsRefused(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, articleWithBody(false))
	require.NoError(t, err)

	for _, op := range []string{"eq", "neq", "gt", "gte", "lt", "lte", "in", "contains", "has", "nhas"} {
		t.Run(op, func(t *testing.T) {
			_, err := svc.ListEntries(ctx, "article", ListEntriesInput{Filters: []string{"body:" + op + ":x"}})
			assert.Equal(t, "CONTENT_FILTER_OP_UNSUPPORTED_FOR_FIELD", codeOf(t, err),
				"`contains` especially: ILIKE over raw block JSON matches markup, not content")
		})
	}
}

func TestRichText_SortIsRefused(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, articleWithBody(false))
	require.NoError(t, err)

	_, err = svc.ListEntries(ctx, "article", ListEntriesInput{Sort: "body:asc"})
	assert.Equal(t, "CONTENT_SORT_FIELD_UNSORTABLE", codeOf(t, err))
	assert.Equal(t, "richtext", detailsOf(t, err)["reason"])
}

// --- image blocks are media references ---------------------------------------

func TestRichText_ImageBlockRequiresALiveUploadedAsset(t *testing.T) {
	svc, repo, store := newMediaSvc()
	ctx := ctxTenant("t1")
	_, err := svc.CreateContentType(ctx, articleWithBody(false))
	require.NoError(t, err)

	doc := func(id string) any {
		return rtDoc(t, `[{"type":"image","media_id":"`+id+`","alt":"hero"}]`)
	}

	t.Run("an unknown asset is refused", func(t *testing.T) {
		_, err := svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{
			"title": "T", "body": doc("7be9f7d8-6d4e-4d38-9e3f-1f1c37a1a111"),
		}))
		assert.Equal(t, "CONTENT_MEDIA_NOT_FOUND", codeOf(t, err))
		assert.Equal(t, "body", detailsOf(t, err)["field"], "the error must name the rich text field holding the block")
	})

	t.Run("a reserved-but-never-uploaded asset is refused", func(t *testing.T) {
		up, err := svc.CreateMediaUpload(ctx, CreateMediaUploadInput{ContentType: "image/png"})
		require.NoError(t, err)
		_, err = svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{
			"title": "T", "body": doc(up.AssetID.String()),
		}))
		assert.Equal(t, "CONTENT_MEDIA_NOT_UPLOADED", codeOf(t, err))
	})

	t.Run("an uploaded asset is accepted and linked through entry_media", func(t *testing.T) {
		assetID := uploadAsset(t, svc, repo, store, ctx)
		got, err := svc.CreateEntry(ctx, "article", mustJSON(t, map[string]any{
			"title": "T", "body": doc(assetID.String()),
		}))
		require.NoError(t, err)
		require.Contains(t, repo.links, got.ID,
			"the image reference must land in entry_media, or asset GC would reap a referenced file")
		assert.Contains(t, repo.links[got.ID], assetID)
	})

	t.Run("another tenant's asset is invisible", func(t *testing.T) {
		assetID := uploadAsset(t, svc, repo, store, ctx)
		_, err := svc.CreateEntry(ctxTenant("t2"), "article", mustJSON(t, map[string]any{
			"title": "T", "body": doc(assetID.String()),
		}))
		// t2 has no `article` type; create one so the refusal is about the ASSET.
		require.Error(t, err)
		_, err = svc.CreateContentType(ctxTenant("t2"), articleWithBody(false))
		require.NoError(t, err)
		_, err = svc.CreateEntry(ctxTenant("t2"), "article", mustJSON(t, map[string]any{
			"title": "T", "body": doc(assetID.String()),
		}))
		assert.Equal(t, "CONTENT_MEDIA_NOT_FOUND", codeOf(t, err),
			"a cross-tenant reference must read as nonexistent, not as forbidden")
	})
}
