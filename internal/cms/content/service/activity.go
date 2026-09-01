package service

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// The activity record's write side (ADR-014 §3).
//
// SHAPE: one recorder per service method, opened on the first line and closed
// by a deferred finish(). That is deliberate and it is the whole reason this
// file is small. §3's hardest requirement is that REFUSALS are recorded, and a
// refusal can come from any of half a dozen guards — the authorize chokepoint,
// the type-level and field-level permission gates, own_only confinement, the
// version check, payload validation. Recording at each guard would be a list to
// remember at every future guard, and the one that gets forgotten is invisible
// by construction. A deferred close catches whatever the method returns,
// including refusals added after this was written.
//
// WHAT IS RECORDED, and why it is not simply "everything":
//
//	                      | success        | refused
//	agent credential      | yes            | yes
//	human (admin console) | writes only    | yes
//	delivery / preview    | no             | writes only
//
//   - Agent success on READS too, because the dominating rule is about what an
//     AGENT can do: "anything an agent can do, the console must be able to say
//     afterwards what it did". cms_list_entries is something it can do.
//   - Human successful reads are not recorded. The rule above does not reach
//     them, and a row per console page view would bury the handful of agent
//     lines this table exists to surface.
//   - The delivery audience is public web traffic, accounted by ADR-004's read
//     counter, not editorial action; a row per page view is not a record, it is
//     an outage. Its refused WRITES are recorded and stay recorded: that
//     credential is refused every write at the chokepoint precisely because the
//     published-only property must not rest on one layer (ADR-004), and an
//     attempt to write with one is worth a line whoever caused it.
//
// WHAT IS NOT RECORDED AT ALL: 5xx. §3 asks for two outcomes, succeeded and was
// refused, and an outage is neither — nothing was decided and nothing was done.
// Filing it under "denied" would put an infrastructure failure in the column a
// reader consults to find out who refused them.
//
// ORDERING: the row is written AFTER the operation returns, so the only failure
// mode is a LOST record (crash between the write and the record), never a false
// one. The reverse order would let a rolled-back write leave a line saying it
// happened, and nothing downstream could tell that line from a true one.

// activityRec accumulates one service call's row. A nil *activityRec is a
// working no-op, so a call site never needs to check.
type activityRec struct {
	svc    *contentService
	sub    authn.Subject
	action string
	// write distinguishes the verbs that change something from the ones that
	// only look, which is the axis the table above turns on.
	write      bool
	targetType string
	entryID    *uuid.UUID
	title      string
	changed    []string
	at         time.Time
	cancelled  bool
}

// activityWrite opens a recorder for a verb that changes something.
func (s *contentService) activityWrite(ctx context.Context, action, typeName string) *activityRec {
	return s.beginActivity(ctx, action, true, typeName)
}

// activityRead opens a recorder for a verb that only looks.
func (s *contentService) activityRead(ctx context.Context, action, typeName string) *activityRec {
	return s.beginActivity(ctx, action, false, typeName)
}

// beginActivity captures what is known before the guards run: who is asking and
// what they are asking for.
//
// It reads the subject straight from the context rather than waiting for
// authorize() to return one, because the refusals worth recording are precisely
// the ones where authorize() returns no subject at all.
//
// No subject, or a subject with no tenant, records nothing: there is no tenant
// to file the row under, and RLS would refuse it anyway. Both cases are
// answered before any content state is touched, so nothing is lost but a line
// nobody could attribute.
func (s *contentService) beginActivity(ctx context.Context, action string, write bool, typeName string) *activityRec {
	sub, ok := authn.SubjectFromContext(ctx)
	if !ok || sub.TenantID == "" {
		return nil
	}
	return &activityRec{
		svc:        s,
		sub:        sub,
		action:     action,
		write:      write,
		targetType: typeName,
		at:         time.Now().UTC(),
	}
}

// forEntry names the entry this action concerned.
func (a *activityRec) forEntry(id uuid.UUID) *activityRec {
	if a == nil {
		return nil
	}
	a.entryID = &id
	return a
}

// describe attaches the human-readable label, derived under domain.TitleFor's
// two fences — notably that a read-restricted field may not supply one.
func (a *activityRec) describe(fields []domain.Field, payload json.RawMessage) *activityRec {
	if a == nil {
		return nil
	}
	a.title = domain.TitleFor(fields, payload)
	return a
}

// changedKeys names the payload keys this action altered. Keys only; the
// domain type's comment says why values may not follow them here.
func (a *activityRec) changedKeys(keys []string) *activityRec {
	if a == nil {
		return nil
	}
	a.changed = keys
	return a
}

// action names the verb, for the one call site whose verb is not known until
// after its argument is validated (publish versus unpublish).
func (a *activityRec) setAction(action string) *activityRec {
	if a == nil {
		return nil
	}
	a.action = action
	return a
}

// cancel drops the row. It exists for ONE shape: a method that succeeded
// without doing anything, where recording it would claim an effect that did not
// happen. Pressing publish on content that is already live and unchanged is the
// case — SetEntryStatus treats it as a no-op on purpose ("republishing
// identical content is not a new release"), and a stream that shows a release
// line per button press would disagree with the code about what a release is.
//
// It is NOT for suppressing outcomes somebody would rather not see. A refusal
// cannot be cancelled here; the recorder decides that, not the call site.
func (a *activityRec) cancel() {
	if a != nil {
		a.cancelled = true
	}
}

// finish writes the row, or decides there is none to write. It is the deferred
// half of every recorder and it never returns an error: the caller's own result
// is already decided, and failing a request because its audit line could not be
// stored would turn a bookkeeping fault into an outage.
//
// It does NOT swallow the failure silently either — a lost refusal is exactly
// the blindness §3 exists to end, so it goes to the process log where the
// operator looking for the missing line will find out why.
func (a *activityRec) finish(ctx context.Context, err error) {
	if a == nil {
		return
	}
	// Cancellation applies to a success only. A refusal that a call site tried
	// to drop is the one thing §3 will not let it drop.
	if a.cancelled && err == nil {
		return
	}
	outcome, code := domain.ActivityOutcomeSuccess, ""
	if err != nil {
		var ae *apperrors.AppError
		if !errors.As(err, &ae) || ae.HTTPStatus < 400 || ae.HTTPStatus >= 500 {
			return
		}
		outcome, code = domain.ActivityOutcomeDenied, ae.Code
		// A refusal changed nothing. Migration 000032 refuses the contradiction
		// too; clearing it here is what keeps a half-completed method — one that
		// recorded its keys and then hit a guard — from producing it.
		a.changed = nil
	}
	if !a.shouldRecord(outcome) {
		return
	}
	kind, userID, agentID := activityActorOf(a.sub)
	row := &domain.Activity{
		ID:            uuid.New(),
		TenantID:      a.sub.TenantID,
		OccurredAt:    a.at,
		ActorKind:     kind,
		ActorUserID:   userID,
		ActorAgentID:  agentID,
		Action:        a.action,
		TargetType:    a.targetType,
		TargetEntryID: a.entryID,
		TargetTitle:   a.title,
		Outcome:       outcome,
		ErrorCode:     code,
		ChangedKeys:   a.changed,
	}
	// A title with no entry to hang it on is a label for nothing, and the
	// migration refuses the pair. Reachable: a create that was refused before
	// the row got an id, having already been handed a payload to label.
	if row.TargetEntryID == nil {
		row.TargetTitle = ""
	}
	// context.WithoutCancel: the row must land even when the caller's context is
	// already done. A refused request whose client hung up is still a refused
	// request, and it is the one an operator most wants to see.
	if err := a.svc.repo.RecordActivity(context.WithoutCancel(ctx), row); err != nil {
		log.Printf("content activity: record %s/%s for tenant %s: %v", a.action, outcome, a.sub.TenantID, err)
	}
}

// shouldRecord is the table in this file's header, as code.
func (a *activityRec) shouldRecord(outcome string) bool {
	refused := outcome == domain.ActivityOutcomeDenied
	switch {
	case a.sub.PublicDelivery:
		return a.write && refused
	case a.sub.IsAgent():
		return true
	default:
		return a.write || refused
	}
}

// activityActorOf maps a credential onto §3's three actor kinds.
//
// It is NOT provenanceOf: that one answers "who authored this row" for the
// entry columns and has only two answers, because a delivery credential is
// refused every write and so never authors anything. This one describes an
// ATTEMPT, which a delivery credential can make, so the third kind the schema
// has always allowed finally has something that emits it.
//
// An agent whose principal is missing produces kind=agent with a nil user id,
// which migration 000032's ratchet refuses — deliberately, and for the same
// reason 000030's does on entries: a bot acting with nobody accountable is the
// shape ADR-013 §2 keeps out, and the constraint is the last word on it. The
// case is unreachable through the middleware, which refuses an agent token
// whose principal does not parse (jwt_middleware.go:65-73); a fourth behaviour
// invented here would be a second answer to a question that already has one.
func activityActorOf(sub authn.Subject) (kind string, userID *uuid.UUID, agentID *string) {
	switch {
	case sub.PublicDelivery:
		return domain.ActorKindService, nil, nil
	case sub.IsAgent():
		return domain.ActorKindAgent, actor(sub), sub.AgentID
	default:
		return domain.ActorKindHuman, actor(sub), nil
	}
}

// --- read side ----------------------------------------------------------------

// ActivityDTO is one line of the stream as the console sees it.
//
// No `omitempty` on the actor trio or the outcome pair, and that is a decision
// rather than an oversight: those fields are what a reader must render, and a
// key that vanishes when it holds the zero value is a key an API-shape test can
// silently stop covering — the exact way TestDelivery_EntryDTOCarriesNoAuthorship
// stopped covering the provenance columns it names (ADR-014 §4 落地回饋).
type ActivityDTO struct {
	ID         uuid.UUID `json:"id"`
	OccurredAt time.Time `json:"occurred_at"`

	// ActorKind is human | agent | service. ActorUserID is who ANSWERS — the
	// principal, for an agent — and null means there is no person to name, which
	// a reader renders as unknown rather than inventing one (§4's third state).
	ActorKind    string     `json:"actor_kind"`
	ActorUserID  *uuid.UUID `json:"actor_user_id"`
	ActorAgentID *string    `json:"actor_agent_id"`

	Action string `json:"action"`

	TargetType    string     `json:"target_type"`
	TargetEntryID *uuid.UUID `json:"target_entry_id"`
	TargetTitle   string     `json:"target_title"`

	Outcome   string `json:"outcome"`
	ErrorCode string `json:"error_code"`

	// ChangedKeys names keys and never values (§3). It is `[]` rather than null
	// when empty so a client can iterate it without a nil check.
	ChangedKeys []string `json:"changed_keys"`
}

// ListActivityInput narrows the stream.
type ListActivityInput struct {
	EntryID *uuid.UUID
	Limit   int
}

// ListActivity returns the tenant's activity stream, newest first.
//
// It authorizes on content:activity:read, a verb of its own (ruled 2026-08-06).
// It shipped on content:list, which handed the stream to every role that can
// list content — VIEWER INCLUDED — and that consequence was ruled unacceptable.
// owner/admin/editor keep it; viewer is the only role that loses anything.
//
// AN AGENT IS REFUSED IT, by two layers in this order: ADR-013 §4's untyped
// rule fires FIRST, at authorizeAgentScope, because the stream spans every type
// and so names none — that is unchanged by the verb split and is still what
// produces the error. The agent gate's refusal of content:activity:read is the
// second layer, and it is the one that survives if §4's rule is ever relaxed
// (§A records that as a standing temptation). Before the split there was no
// second layer at all. Why an agent must not read the stream is unchanged: the
// record of what an agent did is for the person answerable for it, and an agent
// that could read it could see which of its refusals were noticed.
//
// The delivery and preview audiences are refused twice over — isReadAction does
// not list this verb, so the chokepoint refuses them before the authorizer, and
// the explicit check below is the second layer. See there for why it stays.
//
// WHAT THE VERB DOES NOT DO is narrow WHICH rows come back; that is the data
// layer, applied in SQL below.
func (s *contentService) ListActivity(ctx context.Context, in ListActivityInput) ([]ActivityDTO, error) {
	sub, err := s.authorize(ctx, ActionContentActivityRead, "activity", "")
	if err != nil {
		return nil, err
	}
	// SECOND LAYER, and reachable by one plausible edit rather than none. The
	// chokepoint already refuses these audiences because isReadAction fails
	// closed on an unlisted verb — but this verb is SPELLED "read", and adding it
	// to isReadAction is exactly the tidy-up a future reader will make. On that
	// day this line is the only thing between a public delivery credential and
	// the tenant's audit log. ADR-004's rule that the published-only property
	// must not rest on a single layer is the same rule.
	if sub.PublicDelivery {
		return nil, apperrors.ErrForbidden
	}
	// The DATA layer travels into the query for the reason it does on the queue:
	// a stream that spans every type gives the service no type to guard against.
	rows, err := s.repo.ListActivity(ctx, repository.ActivityFilter{
		TenantID:     sub.TenantID,
		EntryID:      in.EntryID,
		ViewerRole:   sub.TenantRole,
		ViewerUserID: sub.ResponsibleUserID(),
		Limit:        in.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]ActivityDTO, 0, len(rows))
	for _, r := range rows {
		keys := r.ChangedKeys
		if keys == nil {
			keys = []string{}
		}
		out = append(out, ActivityDTO{
			ID:            r.ID,
			OccurredAt:    r.OccurredAt,
			ActorKind:     r.ActorKind,
			ActorUserID:   r.ActorUserID,
			ActorAgentID:  r.ActorAgentID,
			Action:        r.Action,
			TargetType:    r.TargetType,
			TargetEntryID: r.TargetEntryID,
			TargetTitle:   r.TargetTitle,
			Outcome:       r.Outcome,
			ErrorCode:     r.ErrorCode,
			ChangedKeys:   keys,
		})
	}
	return out, nil
}

// --- per-field attribution (ADR-014 §6, step 4) --------------------------------

// FieldAuthorDTO is one field's last recorded writer.
//
// No `omitempty` on the trio, for the reason ActivityDTO states: these are the
// fields a reader must render, and a key that vanishes when it holds the zero
// value is a key an API-shape test can silently stop covering.
type FieldAuthorDTO struct {
	// ActorKind is human | agent | service, and ActorUserID is who ANSWERS —
	// the principal, for an agent. The console renders the trio through the same
	// three-state rule as §4's "last edited by"; see describeWriter on the web
	// side, which is the single implementation of it.
	ActorKind    string     `json:"actor_kind"`
	ActorUserID  *uuid.UUID `json:"actor_user_id"`
	ActorAgentID *string    `json:"actor_agent_id"`
	OccurredAt   time.Time  `json:"occurred_at"`
}

// EntryAttributionDTO is the diff's per-field authorship.
//
// A key with no recorded author is ABSENT from the map. That is the whole
// contract and the reason this is a map rather than a list parallel to the
// diff's fields: the console must render an unattributed field as "unknown",
// and the ADR forbids the one fallback that would otherwise be tempting —
// showing the entry's updated_by, which turns "we don't know who changed this
// field" into a named person's byline for work that may not be theirs.
type EntryAttributionDTO struct {
	Fields map[string]FieldAuthorDTO `json:"fields"`
}

// EntryFieldAttribution answers "who last changed each field of this entry".
//
// AUTHORIZATION is the entry's own read rule, plus one refusal: the delivery and
// preview audiences are turned away outright. This is admin-only material for
// the same reason published_data and the provenance columns are (see
// ProjectEntry's admin-only block) — internal user ids and a tenant's private
// automation topology — and a preview link goes to people outside the platform.
// Refused BEFORE the entry is looked up, so the answer cannot distinguish an id
// that exists from one that does not.
//
// An agent IS allowed, unlike the tenant-wide stream, and the difference is not
// an inconsistency: ListActivity refuses agents because it spans everything and
// would show an agent which of its own refusals were noticed. This answers one
// question about one entry the agent may already read, and its updated_by_agent
// is on that entry's DTO today. It is recorded under its own verb, so the
// console can say afterwards that the agent asked.
//
// THE WINDOW is "since the live snapshot was taken", and PublishedAt is the
// closest bound the schema can supply — not the exact one. It does not move on
// re-publish (see domain.Entry), so it can only be EARLIER than the snapshot,
// never later. That direction is the safe one: an over-wide window can only
// offer authors for keys that are not in the diff, and the diff is what decides
// which keys get rendered. A too-narrow window would do the damage — it would
// report "unknown" for a field whose author is recorded.
//
// nil (a draft, and after §5 lands, a retracted entry) means no bound at all,
// which is the same safe direction: the whole of the entry's recorded history,
// folded newest-first.
func (s *contentService) EntryFieldAttribution(ctx context.Context, typeName string, id uuid.UUID) (_ EntryAttributionDTO, err error) {
	act := s.activityRead(ctx, domain.ActivityEntryAttribution, typeName).forEntry(id)
	defer func() { act.finish(ctx, err) }()
	sub, err := s.authorize(ctx, ActionContentRead, id.String(), typeName)
	if err != nil {
		return EntryAttributionDTO{}, err
	}
	if sub.PublicDelivery {
		return EntryAttributionDTO{}, apperrors.ErrForbidden
	}
	ct, err := s.repo.GetContentTypeByName(ctx, sub.TenantID, typeName)
	if err != nil {
		return EntryAttributionDTO{}, err
	}
	if err := guardTypeRead(ct, sub); err != nil {
		return EntryAttributionDTO{}, err
	}
	e, err := s.repo.GetEntry(ctx, sub.TenantID, ct.ID, id)
	if err != nil {
		return EntryAttributionDTO{}, err
	}
	// Confinement, exactly as GetEntry applies it: a row this caller does not own
	// answers 404 rather than confirming it exists.
	if err := guardOwned(ct, sub, e); err != nil {
		return EntryAttributionDTO{}, err
	}
	authors, err := s.repo.EntryFieldAuthors(ctx, sub.TenantID, id, e.PublishedAt)
	if err != nil {
		return EntryAttributionDTO{}, err
	}
	// The SAME mask the diff itself goes through (MarshalJSON strips these from
	// both payloads). Without it this endpoint would answer questions about
	// fields the caller cannot see — a narrower leak than the values, but the
	// same door, and §6's rule is that the two sides of a diff are narrowed
	// identically. A masked key is simply absent, which is what an unattributed
	// key looks like too; the caller has no field to hang it on either way.
	hidden := unreadableKeys(ct.Fields, sub)
	out := EntryAttributionDTO{Fields: map[string]FieldAuthorDTO{}}
	for _, a := range authors {
		if slices.Contains(hidden, a.Key) {
			continue
		}
		out.Fields[a.Key] = FieldAuthorDTO{
			ActorKind:    a.ActorKind,
			ActorUserID:  a.ActorUserID,
			ActorAgentID: a.ActorAgentID,
			OccurredAt:   a.OccurredAt,
		}
	}
	return out, nil
}
