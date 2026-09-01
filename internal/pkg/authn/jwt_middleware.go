package authn

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/auth/jwt"
)

// AgentCredentialChecker answers, for one request, whether an agent credential
// is still allowed to work — the revocation half of ADR-013's credential
// lifecycle (ruled 2026-08-06). Missing, revoked and expired are all false, and
// so is "the database could not say".
//
// It is an interface here rather than the repository type because this package
// must not depend on auth/repository: authn is imported by every plane,
// including the ones that have no database at all.
type AgentCredentialChecker interface {
	IsActive(ctx context.Context, id uuid.UUID) (bool, error)
}

// JWTMiddleware validates Bearer tokens and injects Subject; falls back to dev headers when allowed.
//
// A NIL revocation CHECKER REFUSES EVERY AGENT CREDENTIAL, and that is the
// whole design of the parameter rather than an edge case. An app that did not
// wire a checker cannot ask whether a credential was revoked, and the only two
// available readings of that are "honour it anyway" and "do not honour it" —
// the first turns every deployment that forgot one line into the deployment
// where revocation silently does nothing, which is indistinguishable from a
// working one until the day it matters. Human and delivery credentials are
// untouched: they carry no revocation record because they are short-lived, so
// an app with no agent surface passes nil and behaves exactly as before.
func JWTMiddleware(signer *jwt.Signer, allowDevHeaders bool, agentCredentials AgentCredentialChecker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if sub, ok := subjectFromBearer(r, signer); ok {
				if sub.IsAgent() && !agentCredentialUsable(r.Context(), agentCredentials, sub) {
					// Not "unauthenticated with a reason": the token is simply
					// not honoured, and the request continues as anonymous so
					// the ordinary 401/403 path answers it. A revoked agent
					// learns that it is refused, not why.
					next.ServeHTTP(w, r)
					return
				}
				next.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), sub)))
				return
			}
			if allowDevHeaders {
				Middleware(next).ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func subjectFromBearer(r *http.Request, signer *jwt.Signer) (Subject, bool) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return Subject{}, false
	}
	raw := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	claims, err := signer.ParseAccessToken(raw)
	if err != nil {
		return Subject{}, false
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return Subject{}, false
	}
	// A malformed preview claim invalidates the WHOLE token rather than being
	// dropped. Dropping it is an escalation, not a tidy-up: the claim's only
	// effect is to narrow a delivery credential to one entry, so ignoring an
	// unparseable one hands the bearer the unrestricted delivery credential
	// underneath — every published entry in the tenant, list endpoints included.
	// The safe reading of "I cannot tell which entry this is scoped to" is no.
	var previewEntryID *uuid.UUID
	if claims.PreviewEntry != "" {
		id, perr := uuid.Parse(claims.PreviewEntry)
		if perr != nil {
			return Subject{}, false
		}
		previewEntryID = &id
	}
	// The agent claims are validated as a SET by ParseAccessToken, which
	// refuses the whole token when they are incomplete — so reaching here with
	// kind=agent means agent id, principal and whitelist are all present and
	// well formed. Parsing the principal again rather than trusting that: the
	// cost is one uuid.Parse, and the failure it guards is a Subject whose
	// PrincipalID is nil, which every write path downstream reads as "nobody
	// nameable" and silently stops attributing.
	var principalID *uuid.UUID
	var agentID *string
	var credentialID *uuid.UUID
	if claims.Kind == jwt.KindAgent {
		id, perr := uuid.Parse(claims.Principal)
		if perr != nil {
			return Subject{}, false
		}
		principalID = &id
		a := claims.AgentID
		agentID = &a
		// Same treatment and the same reason as the principal above:
		// ParseAccessToken already refuses an agent token whose jti will not
		// parse, and re-parsing costs one uuid.Parse. The failure it guards is
		// worse than a missing principal — a Subject with no CredentialID is one
		// the revocation check cannot look up, and a check that cannot look its
		// subject up has to refuse, so a silent nil here would take the agent
		// off the air rather than let it through. Cheaper to refuse the token.
		cid, cerr := uuid.Parse(claims.ID)
		if cerr != nil {
			return Subject{}, false
		}
		credentialID = &cid
	}
	return Subject{
		UserID:      userID,
		Roles:       claims.Roles,
		TenantID:    claims.TenantID,
		TenantRole:  claims.TenantRole,
		Region:      claims.Region,
		MFAVerified: claims.MFA,
		// Only ever from a signed claim — never from a dev header (see
		// Middleware), because it gates a public-facing surface.
		PublicDelivery: claims.Delivery,
		PreviewEntryID: previewEntryID,
		// Same rule for the actor kind, for the same reason plus one: a dev
		// header that could say "I am an agent" would let a developer's browser
		// mint the credential the whole of ADR-013 §1 exists to bound.
		Kind:         ActorKind(claims.Kind),
		AgentID:      agentID,
		PrincipalID:  principalID,
		AllowedTypes: claims.AllowedTypes,
		CredentialID: credentialID,
	}, true
}

// agentCredentialUsable is the revocation check, kept in one function so that
// every way of answering "no" reads the same at the call site.
func agentCredentialUsable(ctx context.Context, checker AgentCredentialChecker, sub Subject) bool {
	if checker == nil {
		return false
	}
	if sub.CredentialID == nil {
		// Unreachable through ParseAccessToken, which refuses an agent token
		// with no jti — but this function's answer must not depend on that
		// being true one refactor from now, and the safe answer to "which
		// credential is this?" being unanswerable is no.
		return false
	}
	active, err := checker.IsActive(ctx, *sub.CredentialID)
	if err != nil {
		// A database that cannot answer is not a pass. This does mean an
		// outage stops agents while people keep working — which is the right
		// way round: people are present to notice, and an unattended writer
		// running against a system whose revocation list is unreadable is the
		// case revocation exists for.
		return false
	}
	return active
}
