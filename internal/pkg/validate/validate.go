package validate

import (
	"github.com/go-playground/validator/v10"
)

var v = validator.New()

// Struct validates a struct using validator tags.
func Struct(s any) error {
	return v.Struct(s)
}
