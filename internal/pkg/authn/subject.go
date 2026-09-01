package authn

import (
	"context"
	"slices"

	"github.com/google/uuid"
)

type contextKey struct{}

// ActorKind names what KIND of actor a credential speaks for (ADR-013 §1).
//
// The zero value is "" — not "human" — and every agent narrowing triggers on
// ActorKindAgent only. That is the same polarity as PublicDelivery: an actor
// kind that constrains must be POSITIVELY asserted, so a subject built by any
// path that has never heard of agents (dev headers, tests, service startup)
// keeps behaving exactly as it did. The fail-closed direction is elsewhere: a
// token whose agent claims do not parse yields no Subject at all rather than a
// Subject with Kind dropped — see jwt.ParseAccessToken.
type ActorKind string

const (
	ActorKindHuman   ActorKind = "human"
	ActorKindAgent   ActorKind = "agent"
	ActorKindService ActorKind = "service"
)

// Subject carries verified identity claims propagated from gateway/BFF.
//
// Roles is the platform plane (user_roles); TenantRole is the membership role
// in the active tenant. Keep them separate — HasRole must never see tenant
// roles, or a tenant "admin" would satisfy platform is_admin checks (D6/F1).
type Subject struct {
	UserID      uuid.UUID
	Roles       []string
	TenantID    string
	TenantRole  string
	Region      string
	MFAVerified bool
	// PublicDelivery marks a credential minted for the public content delivery
	// edge (ADR-004). It only ever NARROWS what the bearer may do: reads are
	// forced to published content and writes are refused, regardless of what the
	// caller asks for or what role the token carries. Never set from a dev
	// header — a public-facing constraint must come from a signed claim.
	PublicDelivery bool
	// PreviewEntryID, when non-nil, marks a preview credential: the bearer sees
	// the WORKING COPY of exactly this one entry, rendered in the delivery shape.
	//
	// It is a narrowing ON TOP of PublicDelivery, never an alternative to it — a
	// preview credential sets both. That is what keeps ADR-006's "the delivery
	// credential is held only by the platform's own edge" premise intact while
	// preview links go to outside reviewers: a preview token inherits every
	// delivery restriction (writes refused, filter/sort refused, field permission
	// narrowed) and differs in one thing only, which copy the projector reads.
	//
	// The failure direction is deliberate. Any read path that forgets preview
	// exists still sees PublicDelivery and serves the published snapshot — the
	// safe answer. Forgetting cannot leak a draft; it can only fail to show one.
	//
	// Scope is a single entry because a preview token leaves the platform's
	// control. Anything broader would be a delivery credential in the hands of a
	// reviewer, which is the premise ADR-006 says must not change.
	PreviewEntryID *uuid.UUID
	// Kind, AgentID, PrincipalID and AllowedTypes describe an agent credential
	// (ADR-013 §1). They are only ever set from a signed claim minted by
	// jwt.IssueAgentToken, which can only DOWNGRADE an existing tenant
	// credential — an agent reaches nothing its minter could not reach.
	Kind ActorKind
	// AgentID names WHICH agent, for provenance and for the admin UI. Required
	// when Kind is agent. It is a string, not a uuid: it identifies a piece of
	// software, and nothing renders it as a person.
	AgentID *string
	// PrincipalID is the person this credential speaks for — the minter.
	//
	// It is what keeps own_only working (§2). An agent's writes record it as
	// created_by, so agent-written rows have an author, do not accumulate as an
	// unownable pool, and cannot ratchet a type out of ever confining a role.
	// Accountability and attribution are separated but both are kept: the row
	// says Alice is answerable, created_by_agent says the keystrokes were a
	// bot's.
	PrincipalID *uuid.UUID
	// AllowedTypes whitelists the content types this credential may touch.
	//
	// nil = unrestricted, and ONLY a human may be unrestricted; an empty slice
	// means nothing is permitted. This polarity is the opposite of ADR-009 §3's
	// "empty list = unrestricted", deliberately: ADR-009 chose its polarity so
	// EXISTING rows would not break at migration, and AllowedTypes has no
	// existing data — it exists only on newly minted credentials, so it can
	// afford the polarity that fails closed at minting time (§1, CTR F10).
	AllowedTypes []string
	// CredentialID is the agent_credentials row that governs this credential —
	// the token's `jti`. Set whenever Kind is agent, and never otherwise: a
	// human access token has no revocation record, because a human credential
	// dies of its own 15-minute TTL long before anyone could revoke it.
	//
	// It is the ONE agent field that is not a narrowing. The rest bound what the
	// bearer may reach; this one is how the tenant reaches back and stops the
	// bearer, which is what a long-lived credential has to have to be issuable
	// at all (ruled 2026-08-06).
	CredentialID *uuid.UUID
}

// IsAgent reports whether this credential was minted for an agent.
func (s Subject) IsAgent() bool { return s.Kind == ActorKindAgent }

// AllowsContentType answers, for one request, whether this credential may
// touch the named type. It is the PREDICATE for ADR-013 §4; the refusal itself
// belongs at the service's authorize() chokepoint, not here.
//
// The empty type name means "this request concerns no single content type" —
// media, webhooks, usage, whole-schema artifacts, the type list itself. Those
// paths cannot name a type because they have none, so an agent is refused them
// BY CONSTRUCTION rather than by a blacklist somebody has to remember to
// extend. Widening this one case is how that guarantee would be lost.
func (s Subject) AllowsContentType(contentType string) bool {
	if !s.IsAgent() {
		return true
	}
	if s.AllowedTypes == nil || contentType == "" {
		return false
	}
	return slices.Contains(s.AllowedTypes, contentType)
}

// ResponsibleUserID returns the user id that ANSWERS FOR this subject's
// writes: the principal for an agent credential, the acting user otherwise.
//
// uuid.Nil means "nobody nameable", which every caller already treats as the
// fail-closed answer — actor() stores SQL NULL for it and confinedAuthor()
// confines to an author no row can match. An agent credential missing its
// principal lands there rather than falling back to UserID, which for a minted
// agent token is the minter's id arriving through an unvalidated path.
func (s Subject) ResponsibleUserID() uuid.UUID {
	if s.IsAgent() {
		if s.PrincipalID == nil {
			return uuid.Nil
		}
		return *s.PrincipalID
	}
	return s.UserID
}

// WithSubject attaches a verified subject to the context.
func WithSubject(ctx context.Context, s Subject) context.Context {
	return context.WithValue(ctx, contextKey{}, s)
}

// SubjectFromContext returns the subject when present.
func SubjectFromContext(ctx context.Context) (Subject, bool) {
	s, ok := ctx.Value(contextKey{}).(Subject)
	return s, ok
}

// HasRole reports whether the subject has the given role.
func (s Subject) HasRole(role string) bool {
	for _, r := range s.Roles {
		if r == role {
			return true
		}
	}
	return false
}
