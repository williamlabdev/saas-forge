package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

func TestNormalizeUsername(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "valid", in: "Jane_Doe", want: "jane_doe", wantErr: false},
		{name: "too short", in: "ab", want: "", wantErr: true},
		{name: "invalid chars", in: "user-name", want: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeUsername(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				ae, ok := apperrors.As(err)
				require.True(t, ok)
				assert.Equal(t, apperrors.ErrInvalidUsername.Code, ae.Code)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
