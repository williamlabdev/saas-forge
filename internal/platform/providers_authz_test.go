package platform

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	"github.com/williamlabdev/saas-forge/internal/pkg/config"
)

// TestProvideAuthorizer_WrapsDecoratedAuthorizer pins the fix for a real
// drift: this composition root (BuildApp, used by test/e2e/platform_test.go,
// apps/cmsmcp/test/e2e and apps/delivery/test/e2e) used to build the bare
// OPA/RBAC/AllowAll authorizer directly, without cmd/server's decision-log
// wrap — every decision made through BuildApp went unlogged regardless of
// AUTHZ_DECISION_LOG. provideAuthorizer here now calls the same
// authz.WrapWithDecisionLog helper cmd/server/providers.go calls, so this
// test asserts the returned value really is a *authz.DecoratedAuthorizer for
// every AUTHZ_MODE, not just that construction succeeds.
func TestProvideAuthorizer_WrapsDecoratedAuthorizer(t *testing.T) {
	for _, mode := range []string{"allow", "rbac", "opa"} {
		t.Run(mode, func(t *testing.T) {
			rt := config.Runtime{AuthzMode: mode, AuthzDecisionLog: "deny"}
			auth, err := provideAuthorizer(rt, nil)
			require.NoError(t, err)
			_, ok := auth.(*authz.DecoratedAuthorizer)
			assert.True(t, ok, "provideAuthorizer must return a *authz.DecoratedAuthorizer, got %T", auth)
		})
	}
}

// TestProvideAuthorizer_UnknownDecisionLogFails mirrors
// cmd/server/providers_test.go's TestProvideAuthorizer_UnknownDecisionLogFails
// — this composition root must reject an unrecognised AUTHZ_DECISION_LOG the
// same way, not silently fall back to a default.
func TestProvideAuthorizer_UnknownDecisionLogFails(t *testing.T) {
	rt := config.Runtime{AuthzMode: "rbac", AuthzDecisionLog: "not-a-real-policy"}
	_, err := provideAuthorizer(rt, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AUTHZ_DECISION_LOG")
}

// TestBuildApp_UnknownDecisionLogFailsFast proves the guard added to
// BuildApp itself (alongside validateAuthzMode) rejects an unrecognised
// AUTHZ_DECISION_LOG before touching any dependency — same shape as
// TestBuildApp_ProductionDevHeadersFailFast in app_guard_test.go.
func TestBuildApp_UnknownDecisionLogFailsFast(t *testing.T) {
	cfg := config.User{
		DatabaseURL:      "postgres://invalid-host-never-dialed/db",
		EncryptionKey:    make([]byte, 32),
		BlindIndexPepper: make([]byte, 32),
	}
	rt := config.Runtime{
		AuthzMode:        "rbac",
		JWTSecret:        make([]byte, 32),
		AuthzDecisionLog: "bogus",
	}
	_, err := BuildApp(t.Context(), cfg, rt)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "AUTHZ_DECISION_LOG"))
}
