package identity

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeEmail(t *testing.T) {
	cases := map[string]string{
		"  User@Example.COM  ": "user@example.com",
		"already@lower.com":    "already@lower.com",
		"\tMixed@Case.Io\n":    "mixed@case.io",
		"":                     "",
	}
	for in, want := range cases {
		assert.Equalf(t, want, NormalizeEmail(in), "NormalizeEmail(%q)", in)
	}
}
