// Package openapi embeds the delivery edge's public API description.
//
// The YAML file is the one source; JSON is derived from it at serve time
// rather than hand-maintained a second time (see JSON below) — two committed
// copies invite exactly the drift this package's honesty test
// (apps/delivery/internal/handler/openapi_test.go) exists to catch.
package openapi

import (
	_ "embed"
	"encoding/json"
	"fmt"

	yaml "go.yaml.in/yaml/v3"
)

// YAML is the OpenAPI 3.1 document, verbatim.
//
//go:embed delivery.yaml
var YAML []byte

// JSON renders YAML as JSON. Computed once at package init — the document is
// small, fixed at build time, and every request should pay decode cost zero
// times, not once each.
var JSON []byte

func init() {
	var doc any
	if err := yaml.Unmarshal(YAML, &doc); err != nil {
		panic(fmt.Sprintf("openapi: embedded delivery.yaml does not parse: %v", err))
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(fmt.Sprintf("openapi: embedded delivery.yaml does not convert to JSON: %v", err))
	}
	JSON = b
}
