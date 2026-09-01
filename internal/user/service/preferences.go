package service

import (
	"encoding/json"

	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/user/domain"
)

const maxPreferencesBytes = 16 * 1024

var allowedPreferenceKeys = map[string]struct{}{
	"locale":        {},
	"theme":         {},
	"timezone":      {},
	"notifications": {},
}

var allowedThemes = map[string]struct{}{
	"light":  {},
	"dark":   {},
	"system": {},
}

func validatePreferences(prefs domain.Preferences) error {
	if prefs == nil {
		return nil
	}
	raw, err := json.Marshal(prefs)
	if err != nil {
		return apperrors.Wrap(apperrors.ErrInvalidPreferences.Code, apperrors.ErrInvalidPreferences.Message,
			apperrors.ErrInvalidPreferences.HTTPStatus, err)
	}
	if len(raw) > maxPreferencesBytes {
		return apperrors.ErrInvalidPreferences
	}
	for key, val := range prefs {
		if _, ok := allowedPreferenceKeys[key]; !ok {
			return apperrors.ErrInvalidPreferences
		}
		switch key {
		case "theme":
			str, ok := val.(string)
			if !ok {
				return apperrors.ErrInvalidPreferences
			}
			if _, ok := allowedThemes[str]; !ok {
				return apperrors.ErrInvalidPreferences
			}
		case "locale":
			str, ok := val.(string)
			if !ok || str == "" {
				return apperrors.ErrInvalidPreferences
			}
		case "timezone":
			str, ok := val.(string)
			if !ok || str == "" {
				return apperrors.ErrInvalidPreferences
			}
		case "notifications":
			if _, ok := val.(map[string]any); !ok {
				return apperrors.ErrInvalidPreferences
			}
		}
	}
	return nil
}
