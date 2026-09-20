package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func base() Config {
	return Config{DomainAPIURL: "http://localhost:8080", DefaultLimit: 10, MaxLimit: 50}
}

// stdio is one process acting as one agent, so a missing credential is a
// startup failure. The alternative is a server that connects, lists its tools,
// and 401s on every call — which reads as "no content is available to me".
func TestStdioModeRequiresACredential(t *testing.T) {
	err := Validate(base())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CMS_AGENT_TOKEN")

	cfg := base()
	cfg.AgentToken = "tok"
	assert.NoError(t, Validate(cfg))
}

// The failure this refuses is silent by construction: in HTTP mode a caller
// that forgot its Authorization header would act as whoever the process-wide
// token belongs to, and every write step 7 adds would be attributed to that
// principal. Two answers to "whose credential is this" is one too many.
func TestHTTPModeRefusesAProcessWideCredential(t *testing.T) {
	cfg := base()
	cfg.HTTPAddr = ":9000"
	cfg.AgentToken = "tok"

	err := Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not be set")

	// Without the token the same configuration is fine — each request brings
	// its own bearer.
	cfg.AgentToken = ""
	assert.NoError(t, Validate(cfg))
}

func TestDomainAPIURLIsRequired(t *testing.T) {
	cfg := base()
	cfg.DomainAPIURL = ""
	cfg.AgentToken = "tok"
	require.Error(t, Validate(cfg))
}

// A cap below the default would silently hand back fewer rows than the
// documented default on every unset call.
func TestTheCapMayNotSitBelowTheDefault(t *testing.T) {
	cfg := base()
	cfg.AgentToken = "tok"
	cfg.DefaultLimit = 40
	cfg.MaxLimit = 20
	require.Error(t, Validate(cfg))
}

// ADR-013 §7 names these numbers; they are written out here rather than read
// from Load so a change to the budget has to be made deliberately in both
// places.
func TestTheTokenBudgetDefaults(t *testing.T) {
	t.Setenv("CMS_AGENT_TOKEN", "tok")
	cfg := Load()
	assert.Equal(t, 10, cfg.DefaultLimit)
	assert.Equal(t, 50, cfg.MaxLimit)
	assert.Equal(t, "", cfg.HTTPAddr, "stdio is the default transport")
}
