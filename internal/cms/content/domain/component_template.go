package domain

// ComponentTemplate is a curated component definition an app can adopt as a
// real, tenant-owned Component in one call (ADR-020 Amendment 2).
//
// It is CODE, declared here, not a row anywhere — the same "definitions are
// code, values are data" line ADR-020 already draws for Field types
// (AllowedFieldTypes) and this package draws for everything else in this
// file. Installing a template runs it through the ordinary component-create
// path, so the result is an ordinary Component: renamable, extensible,
// deletable, indistinguishable from one a tenant typed in by hand.
type ComponentTemplate struct {
	Name        string
	Label       string
	Description string
	Fields      []Field
}

// componentTemplateURLBound is the one name ComponentTemplates() must never
// use: chi resolves the static "GET/POST /components/templates" ahead of the
// param routes "/components/{name}" regardless of registration order, which
// is what makes GET /components/templates reachable at all — but it also
// means a component actually named "templates" could never be reached
// through GET /components/{name} again. The service refuses the name at
// component create/rename time (CONTENT_COMPONENT_NAME_RESERVED) rather than
// letting it become a component nobody can read back.
const componentTemplateURLBound = "templates"

// ReservedComponentName reports whether name is refused as a component name
// because it collides with the /components/templates route.
func ReservedComponentName(name string) bool { return name == componentTemplateURLBound }

// componentTemplates is authored once, in declaration order; ComponentTemplates
// and ComponentTemplateByName both return copies so a caller can never mutate
// the shared definition through its Fields slice.
var componentTemplates = []ComponentTemplate{seoComponentTemplate}

// seoCanonicalURLPattern is a loose http(s) URL check, not a full RFC 3986
// parse — FieldConstraints.Pattern has no separate "format: url" the way it
// has FieldFormatSlug, so this template expresses the same intent as a
// Pattern instead. Good enough to catch "not a URL at all"; anything more is
// tenant-specific enough it belongs in a tenant's own field, not a built-in.
const seoCanonicalURLPattern = `https?://\S+`

func templateBound(v float64) *float64 { return &v }

var seoComponentTemplate = ComponentTemplate{
	Name:  "seo",
	Label: "SEO",
	Description: "Search and social metadata a page needs: title, description, " +
		"share image, canonical URL, index flag.",
	Fields: []Field{
		{
			Key:      "meta_title",
			Type:     FieldTypeString,
			Label:    "Meta Title",
			Required: false,
			Description: "The title search engines and social previews show for " +
				"this page.",
			FieldConstraints: FieldConstraints{Max: templateBound(70)},
		},
		{
			Key:   "meta_description",
			Type:  FieldTypeText,
			Label: "Meta Description",
			Description: "The one- or two-sentence summary shown under the title " +
				"in search results.",
			FieldConstraints: FieldConstraints{Max: templateBound(160)},
		},
		{
			Key:   "og_image",
			Type:  FieldTypeFile,
			Label: "Share Image",
			Description: "The image used when this page is shared on social " +
				"platforms (Open Graph).",
		},
		{
			Key:   "canonical_url",
			Type:  FieldTypeString,
			Label: "Canonical URL",
			Description: "The preferred absolute URL for this page; validated as " +
				"an http(s) address. Leave blank if this page has none.",
			FieldConstraints: FieldConstraints{Pattern: seoCanonicalURLPattern},
		},
		{
			Key:   "no_index",
			Type:  FieldTypeBoolean,
			Label: "No Index",
			Description: "When set, tells search engines not to index this page.",
		},
	},
}

// ComponentTemplates returns the built-in component templates, in declaration
// order. Each call returns a fresh copy — of the slice and of every
// template's Fields slice — so a caller cannot mutate the shared definition.
func ComponentTemplates() []ComponentTemplate {
	out := make([]ComponentTemplate, len(componentTemplates))
	for i, t := range componentTemplates {
		out[i] = t
		out[i].Fields = append([]Field(nil), t.Fields...)
	}
	return out
}

// ComponentTemplateByName returns the built-in template with the given name,
// and false if no template has that name.
func ComponentTemplateByName(name string) (ComponentTemplate, bool) {
	for _, t := range componentTemplates {
		if t.Name == name {
			t.Fields = append([]Field(nil), t.Fields...)
			return t, true
		}
	}
	return ComponentTemplate{}, false
}
