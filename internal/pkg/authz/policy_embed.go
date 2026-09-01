package authz

import _ "embed"

//go:embed policies/authz.rego
var embeddedPolicy string
