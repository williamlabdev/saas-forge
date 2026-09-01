package authn

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/auth/jwt"
)

// The actor kind vocabulary exists in three places (wire, identity, storage)
// because the layers may not import each other. This pins the two halves the
// authn package can see; the storage half is pinned in the CMS service.
//
// The expected strings are written out literally rather than compared only to
// each other: two constants can be changed together and stay equal while the
// tokens already in circulation carry the old spelling.
func TestActorKindWireValuesMatchIdentityValues(t *testing.T) {
	require.Equal(t, "agent", jwt.KindAgent)
	require.Equal(t, "human", jwt.KindHuman)
	require.Equal(t, ActorKind("agent"), ActorKindAgent)
	require.Equal(t, ActorKind("human"), ActorKindHuman)
	require.Equal(t, ActorKind("service"), ActorKindService)
	require.Equal(t, string(ActorKindAgent), jwt.KindAgent)
	require.Equal(t, string(ActorKindHuman), jwt.KindHuman)
}

// ADR-013 §1: nil AllowedTypes means UNRESTRICTED, and only a human may be
// unrestricted. The polarity is the reverse of ADR-009 §3, so it gets a test
// that states the expected answers outright rather than deriving them.
func TestAllowsContentTypePolarity(t *testing.T) {
	human := Subject{UserID: uuid.New()}
	require.True(t, human.AllowsContentType("post"), "a human with no whitelist is unrestricted")
	require.True(t, human.AllowsContentType(""), "a human may use paths that concern no single type")

	unset := Subject{Kind: ActorKindAgent}
	require.False(t, unset.AllowsContentType("post"), "unset is not everything")
	require.False(t, unset.AllowsContentType(""), "unset is not everything, on untyped paths either")

	none := Subject{Kind: ActorKindAgent, AllowedTypes: []string{}}
	require.False(t, none.AllowsContentType("post"), "an empty whitelist permits nothing")

	scoped := Subject{Kind: ActorKindAgent, AllowedTypes: []string{"post", "faq"}}
	require.True(t, scoped.AllowsContentType("post"))
	require.True(t, scoped.AllowsContentType("faq"))
	require.False(t, scoped.AllowsContentType("invoice"), "a type outside the whitelist is refused")
	require.False(t, scoped.AllowsContentType("Post"), "the match is exact, not case-folded")
	require.False(t, scoped.AllowsContentType(""), "media/webhook/usage paths name no type, so an agent is refused them")
}

// The empty content type is the load-bearing case of ADR-013 §4 (and the one
// §A's ruling refused to relax). Named separately so that a change widening it
// fails a test whose NAME says what was given up.
func TestAgentIsRefusedPathsThatNameNoContentType(t *testing.T) {
	agent := Subject{Kind: ActorKindAgent, AllowedTypes: []string{"post"}}
	require.False(t, agent.AllowsContentType(""),
		"media, webhooks, usage and the type list are closed to agents by construction — see ADR-013 §4 and §A before changing this")
}

func TestResponsibleUserIDNamesThePrincipalForAgents(t *testing.T) {
	person := uuid.New()
	bot := uuid.New()

	human := Subject{UserID: person}
	require.Equal(t, person, human.ResponsibleUserID(), "a human answers for their own writes")

	agentID := "content-bot"
	agent := Subject{UserID: bot, Kind: ActorKindAgent, AgentID: &agentID, PrincipalID: &person}
	require.Equal(t, person, agent.ResponsibleUserID(),
		"an agent's writes are answered for by the principal who minted it (ADR-013 §2)")
	require.NotEqual(t, bot, agent.ResponsibleUserID(),
		"recording the credential's own subject would be the unresolvable service-account id actor() has always refused")

	orphan := Subject{UserID: bot, Kind: ActorKindAgent, AgentID: &agentID}
	require.Equal(t, uuid.Nil, orphan.ResponsibleUserID(),
		"an agent with no principal names nobody — it must not fall back to UserID")
}

func TestIsAgentRequiresThePositiveAssertion(t *testing.T) {
	require.False(t, Subject{}.IsAgent(), "the zero value is not an agent")
	require.False(t, Subject{Kind: ActorKindHuman}.IsAgent())
	require.False(t, Subject{Kind: ActorKindService}.IsAgent(), "service is not agent — a different kind, not a synonym")
	require.True(t, Subject{Kind: ActorKindAgent}.IsAgent())
}
