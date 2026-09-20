package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/pkg/config"
)

func validRuntimeForDecisionLogTests(decisionLog string) config.Runtime {
	return config.Runtime{
		AppEnv:           "development", // skips the production-only guards; JWT length still applies
		AuthzMode:        "rbac",
		AuthzDecisionLog: decisionLog,
		JWTSecret:        []byte(strings.Repeat("a", 32)),
	}
}

// TestValidateRuntime_AuthzDecisionLog mirrors AUTHZ_MODE's own startup
// guard (the switch a few lines above validateRuntime): an unrecognised
// AUTHZ_DECISION_LOG value must fail the process at startup with a message
// naming the variable, exactly like an unrecognised AUTHZ_MODE does — not
// silently fall back to a default the way provideAuthorizer's AUTHZ_MODE
// switch falls back to allow-all.
func TestValidateRuntime_AuthzDecisionLog(t *testing.T) {
	for _, v := range []string{"", "off", "deny", "all", "OFF", " all "} {
		t.Run("valid/"+v, func(t *testing.T) {
			require.NoError(t, validateRuntime(validRuntimeForDecisionLogTests(v)))
		})
	}

	for _, v := range []string{"bogus", "al", "denyy", "ON"} {
		t.Run("invalid/"+v, func(t *testing.T) {
			err := validateRuntime(validRuntimeForDecisionLogTests(v))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "AUTHZ_DECISION_LOG")
		})
	}
}

// TestProvideAuthorizer_UnknownDecisionLogFails proves the same guard is
// reachable through the actual wire provider, not just validateRuntime — if
// AUTHZ_MODE parsing ever moved out of validateRuntime and only lived in
// provideAuthorizer, this still catches an unrecognised AUTHZ_DECISION_LOG
// at InitializeApp time (log.Fatalf in main.go).
func TestProvideAuthorizer_UnknownDecisionLogFails(t *testing.T) {
	rt := validRuntimeForDecisionLogTests("not-a-real-policy")
	_, err := provideAuthorizer(rt, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AUTHZ_DECISION_LOG")
}

// TestProvideAuthorizer_KnownDecisionLogValuesWire proves provideAuthorizer
// constructs successfully — and returns a non-nil, callable Authorizer — for
// every AUTHZ_MODE x AUTHZ_DECISION_LOG combination the server supports.
func TestProvideAuthorizer_KnownDecisionLogValuesWire(t *testing.T) {
	for _, mode := range []string{"allow", "rbac", "opa"} {
		for _, dl := range []string{"off", "deny", "all"} {
			t.Run(mode+"/"+dl, func(t *testing.T) {
				rt := validRuntimeForDecisionLogTests(dl)
				rt.AuthzMode = mode
				auth, err := provideAuthorizer(rt, nil)
				require.NoError(t, err)
				require.NotNil(t, auth)
			})
		}
	}
}
