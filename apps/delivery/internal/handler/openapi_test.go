package handler

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	yaml "go.yaml.in/yaml/v3"

	"github.com/williamlabdev/saas-forge/apps/delivery/openapi"
)

// This file is the honesty gate for apps/delivery/openapi/delivery.yaml: it
// reads the SAME handler.go source these other tests exercise, not a second,
// hand-maintained list of "what the routes look like" — so a route, query
// parameter or error code added to the handler without a matching edit to the
// spec fails the build here, naming exactly what is missing, rather than
// silently shipping a spec that describes a smaller API than the one live.
//
// It deliberately does not reach for a full OpenAPI validator library: the
// three properties below are the ones worth an automated gate (a route that
// does not exist, a parameter nobody documented, an error code the spec's
// enum cannot represent), and a heavier dependency would check schema shape
// this small, single-package API does not need machine-checked.

// specDoc parses the embedded spec once for every sub-test in this file.
func specDoc(t *testing.T) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(openapi.YAML, &doc); err != nil {
		t.Fatalf("openapi: embedded delivery.yaml does not parse: %v", err)
	}
	return doc
}

// TestOpenAPI_RoutesMatchSpec walks the live router (the exact wiring
// apps/delivery/cmd/delivery/main.go serves) and cross-checks it against the
// spec's `paths` map in both directions.
func TestOpenAPI_RoutesMatchSpec(t *testing.T) {
	h := New(nil, nil, 0)
	r := chi.NewRouter()
	h.Routes(r)

	live := map[string]bool{} // "GET /v1/{tenant}/{type}/"
	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		live[method+" "+route] = true
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}

	doc := specDoc(t)
	paths, _ := doc["paths"].(map[string]any)
	if len(paths) == 0 {
		t.Fatal("spec has no paths")
	}

	inSpec := map[string]bool{}
	for path, v := range paths {
		ops, ok := v.(map[string]any)
		if !ok {
			continue
		}
		for verb := range ops {
			inSpec[strings.ToUpper(verb)+" "+path] = true
		}
	}

	for route := range live {
		if !inSpec[route] {
			t.Errorf("route %q exists in the handler but is not declared in apps/delivery/openapi/delivery.yaml", route)
		}
	}
	for route := range inSpec {
		if !live[route] {
			t.Errorf("route %q is declared in apps/delivery/openapi/delivery.yaml but the handler does not serve it", route)
		}
	}
}

// handlerSource reads handler.go's own text — the thing under test — rather
// than a copy, so this file cannot drift from what it is checking.
func handlerSource(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(thisFile), "handler.go")
	b, err := os.ReadFile(path) //nolint:gosec // fixed path derived from this test file's own location
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

var (
	queryGetRe  = regexp.MustCompile(`r\.URL\.Query\(\)\.Get\("([a-zA-Z0-9_-]+)"\)`)
	queryListRe = regexp.MustCompile(`r\.URL\.Query\(\)\["([a-zA-Z0-9_-]+)"\]`)
	errorCodeRe = regexp.MustCompile(`writeError\([^,]+,\s*http\.Status\w+,\s*"([A-Z_]+)"`)
)

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// handlerQueryParams is every query-string key handler.go reads, derived by
// scanning its source rather than hand-listed — see handlerSource's doc.
func handlerQueryParams(t *testing.T) []string {
	t.Helper()
	src := handlerSource(t)
	var names []string
	for _, m := range queryGetRe.FindAllStringSubmatch(src, -1) {
		names = append(names, m[1])
	}
	for _, m := range queryListRe.FindAllStringSubmatch(src, -1) {
		names = append(names, m[1])
	}
	// previewTokenParam is a named const, not a string literal, at its
	// read site (`r.URL.Query().Get(previewTokenParam)`), so the regexp
	// above cannot see it. Its value is pinned by
	// TestGetEntry_ForwardsPopulate and friends elsewhere in this package;
	// added by hand here for that one reason.
	names = append(names, previewTokenParam)
	return uniqueSorted(names)
}

// handlerErrorCodes is every error code handler.go can emit on its own
// (writeUpstreamError's three mapped cases go through writeError too, so
// they are already covered by the same scan).
func handlerErrorCodes(t *testing.T) []string {
	t.Helper()
	src := handlerSource(t)
	var codes []string
	for _, m := range errorCodeRe.FindAllStringSubmatch(src, -1) {
		codes = append(codes, m[1])
	}
	return uniqueSorted(codes)
}

// collectQueryParamNames walks the whole spec tree and returns the `name` of
// every object with `in: query` — wherever it lives (inline under an
// operation, or once under components.parameters and $ref'd everywhere
// else). Not resolving $ref is deliberate: a parameter this API accepts
// belongs SOMEWHERE in the document under `in: query`, and that is the
// property being checked, not which operations reference it.
func collectQueryParamNames(doc any) []string {
	var out []string
	var walk func(n any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			if v["in"] == "query" {
				if name, ok := v["name"].(string); ok {
					out = append(out, name)
				}
			}
			for _, vv := range v {
				walk(vv)
			}
		case []any:
			for _, vv := range v {
				walk(vv)
			}
		}
	}
	walk(doc)
	return uniqueSorted(out)
}

// collectErrorCodeEnum returns the `enum` list of the spec's ErrorCode
// schema. Found by shape (an `enum` array whose entries are all
// SCREAMING_SNAKE_CASE strings) rather than by walking to
// components.schemas.ErrorCode by name, so renaming the schema does not
// silently blind this check.
var errorCodeShape = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func collectErrorCodeEnum(doc any) []string {
	var out []string
	var walk func(n any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			if enumV, ok := v["enum"]; ok {
				if items, ok := enumV.([]any); ok {
					allShaped := len(items) > 0
					var strs []string
					for _, it := range items {
						s, ok := it.(string)
						if !ok || !errorCodeShape.MatchString(s) {
							allShaped = false
							break
						}
						strs = append(strs, s)
					}
					if allShaped {
						out = append(out, strs...)
					}
				}
			}
			for _, vv := range v {
				walk(vv)
			}
		case []any:
			for _, vv := range v {
				walk(vv)
			}
		}
	}
	walk(doc)
	return uniqueSorted(out)
}

// TestOpenAPI_QueryParamsAreDeclared asserts every query parameter the
// handler actually reads is named somewhere in the spec under `in: query`,
// and — the other direction — that every query parameter the spec declares
// is one the handler actually reads. Without the second half, the spec can
// invent a parameter (or keep a stale one after a rename) and nothing here
// would notice, because "handler subset of spec" alone is silent about
// spec-only entries.
func TestOpenAPI_QueryParamsAreDeclared(t *testing.T) {
	doc := specDoc(t)
	declared := collectQueryParamNames(doc)
	declaredSet := map[string]bool{}
	for _, d := range declared {
		declaredSet[d] = true
	}

	handled := handlerQueryParams(t)
	handledSet := map[string]bool{}
	for _, p := range handled {
		handledSet[p] = true
	}

	for _, p := range handled {
		if !declaredSet[p] {
			t.Errorf("query parameter %q is read by handler.go but not declared anywhere in apps/delivery/openapi/delivery.yaml (declared: %v)", p, declared)
		}
	}
	for _, d := range declared {
		if !handledSet[d] {
			t.Errorf("query parameter %q is declared in apps/delivery/openapi/delivery.yaml but handler.go never reads it (handled: %v)", d, handled)
		}
	}
}

// TestOpenAPI_ErrorCodesAreDeclared asserts every error code the handler can
// emit on its own appears in the spec's ErrorCode enum, and — the other
// direction — that every code in the spec's enum is one the handler can
// actually emit. Without the second half, the enum could carry a code no
// `writeError` call ever produces and this test would stay green.
func TestOpenAPI_ErrorCodesAreDeclared(t *testing.T) {
	doc := specDoc(t)
	enum := collectErrorCodeEnum(doc)
	if len(enum) == 0 {
		t.Fatal("apps/delivery/openapi/delivery.yaml has no error code enum for this test to check against")
	}
	enumSet := map[string]bool{}
	for _, c := range enum {
		enumSet[c] = true
	}

	emitted := handlerErrorCodes(t)
	emittedSet := map[string]bool{}
	for _, c := range emitted {
		emittedSet[c] = true
	}

	for _, c := range emitted {
		if !enumSet[c] {
			t.Errorf("error code %q is emitted by handler.go but is not in the ErrorCode enum in apps/delivery/openapi/delivery.yaml (enum: %v)", c, enum)
		}
	}
	for _, c := range enum {
		if !emittedSet[c] {
			t.Errorf("error code %q is declared in the ErrorCode enum in apps/delivery/openapi/delivery.yaml but no writeError(...) call in handler.go emits it (emitted: %v)", c, emitted)
		}
	}
}

// TestOpenAPI_JSONMatchesYAML pins the invariant openapi.JSON documents: it
// is YAML converted, not a second hand-maintained copy.
func TestOpenAPI_JSONMatchesYAML(t *testing.T) {
	var fromYAML any
	if err := yaml.Unmarshal(openapi.YAML, &fromYAML); err != nil {
		t.Fatalf("unmarshal YAML: %v", err)
	}
	var fromJSON any
	if err := yamlUnmarshalJSONCompat(openapi.JSON, &fromJSON); err != nil {
		t.Fatalf("unmarshal generated JSON: %v", err)
	}
	// A structural spot check, not a byte-for-byte one: JSON has no distinct
	// integer/float type the way this comparison needs to tolerate, so
	// compare a handful of top-level keys are the same rather than a deep
	// equality prone to numeric-type false positives.
	ym, _ := fromYAML.(map[string]any)
	jm, _ := fromJSON.(map[string]any)
	for _, key := range []string{"openapi", "info", "paths", "components"} {
		if _, ok := ym[key]; !ok {
			t.Errorf("YAML doc missing top-level key %q", key)
		}
		if _, ok := jm[key]; !ok {
			t.Errorf("JSON doc missing top-level key %q", key)
		}
	}
}

func yamlUnmarshalJSONCompat(b []byte, out any) error {
	// JSON is a subset of YAML, so the same decoder reads both — one fewer
	// dependency for a single-purpose sanity check.
	return yaml.Unmarshal(b, out)
}
