package identity

import "strings"

// NormalizeEmail applies arch-user email normalization.
func NormalizeEmail(email string) string {
	return strings.TrimSpace(strings.ToLower(email))
}
