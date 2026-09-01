package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/user/domain"
)

func TestValidatePreferences(t *testing.T) {
	tests := []struct {
		name    string
		prefs   domain.Preferences
		wantErr bool
	}{
		{name: "valid", prefs: domain.Preferences{"locale": "en-US", "theme": "dark"}, wantErr: false},
		{name: "unknown key", prefs: domain.Preferences{"foo": "bar"}, wantErr: true},
		{name: "invalid theme", prefs: domain.Preferences{"theme": "neon"}, wantErr: true},
		{name: "nil ok", prefs: nil, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePreferences(tt.prefs)
			if tt.wantErr {
				require.Error(t, err)
				ae, ok := apperrors.As(err)
				require.True(t, ok)
				assert.Equal(t, apperrors.ErrInvalidPreferences.Code, ae.Code)
				return
			}
			require.NoError(t, err)
		})
	}
}
