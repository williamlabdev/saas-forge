package service

import (
	"regexp"
	"strings"

	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

var usernamePattern = regexp.MustCompile(`^[a-z0-9_]+$`)

func normalizeEmail(email string) (string, error) {
	n := strings.TrimSpace(strings.ToLower(email))
	if n == "" {
		return "", apperrors.ErrInvalidEmail
	}
	return n, nil
}

func normalizeUsername(username string) (string, error) {
	n := strings.TrimSpace(strings.ToLower(username))
	if len(n) < 3 || len(n) > 64 {
		return "", apperrors.ErrInvalidUsername
	}
	if !usernamePattern.MatchString(n) {
		return "", apperrors.ErrInvalidUsername
	}
	return n, nil
}
