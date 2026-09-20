package platform

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/pkg/config"
)

// TKT-R2 at the composition root: BuildApp must refuse a production config
// with dev headers before touching any dependency (no DB needed — validation
// runs first). Pins the guard against wiring refactors.
func TestBuildApp_ProductionDevHeadersFailFast(t *testing.T) {
	cfg := config.User{
		DatabaseURL:      "postgres://invalid-host-never-dialed/db",
		EncryptionKey:    make([]byte, 32),
		BlindIndexPepper: make([]byte, 32),
	}
	rt := config.Runtime{
		AppEnv:         "production",
		AuthzMode:      "opa",
		AuthDevHeaders: true,
		JWTSecret:      make([]byte, 32),
	}
	_, err := BuildApp(context.Background(), cfg, rt)
	require.Error(t, err)
	require.Contains(t, err.Error(), "AUTH_DEV_HEADERS")

	rt.AuthDevHeaders = false
	rt.AuthzMode = "allow"
	_, err = BuildApp(context.Background(), cfg, rt)
	require.Error(t, err)
	require.Contains(t, err.Error(), "AUTHZ_MODE")
}
