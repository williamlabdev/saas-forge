// Package scheduler executes the publish/unpublish intents filed through the
// content service (ADR-017).
//
// IT IS A CARRIER, NOT A DECIDER. Everything that could be decided was decided
// when a person filed the schedule: whether they were allowed to, which
// operation, at what time, against which version. This worker re-checks exactly
// one of those — the version — and its only other freedom is to DECLINE. It
// holds no credential, has no subject, and calls nothing in the service layer.
//
// THAT IS WHY IT DOES NOT RE-AUTHORIZE. A re-check at execution time would need
// an answer for "the requester's role changed since Friday", and both answers
// are wrong: dropping the release silently strands an editor who is still
// expecting it, and performing it grants a permission the person no longer
// holds. Authorization is a fact about the moment of the decision. What carries
// that fact forward is pinned_version — the release that happens is the one that
// was approved, or none at all.
//
// THE SHAPE IS THE OUTBOX WORKER'S, and the differences are all in one
// direction — this one needs less:
//
//   - No retry. A failed schedule stays failed with its reason recorded,
//     because the failure of a content operation is something an editor must
//     see and re-decide, not something a loop should keep attempting behind
//     their back. ADR-017 records the trigger for revisiting that.
//   - No reclaim job and no `running` state. The claim is a row lock held for
//     the length of the operation, so a worker that dies mid-flight rolls back
//     to pending and the next tick picks the row up. The outbox needs
//     ReclaimStale because its claim is a column write that outlives the
//     process; this one's does not.
package scheduler

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	notifdomain "github.com/williamlabdev/saas-forge/internal/notification/domain"
)

// Notifier tells the requester what became of the schedule they filed. It
// matches SystemNotifier.NotifyUser's own signature exactly — plain strings,
// no pointers — so *SystemNotifier satisfies this with zero adapter code, the
// same convention content/service.Notifier and outbox.Notifier follow (ADR-023).
//
// A single recipient, not a tenant-role fan-out: RequestedBy is the one person
// who filed this schedule and is the one person accountable for re-deciding
// when it fails or goes stale (this worker never re-authorizes — see the
// package doc). Nobody else's business is this worker's to announce.
type Notifier interface {
	NotifyUser(ctx context.Context, userID uuid.UUID, tenantID, kind, title, body, link string) error
}

// DefaultPollInterval is how often a tick looks for due schedules when nothing
// else is configured.
//
// Thirty seconds, and the number is a statement about what this feature
// promises: "published at roughly the minute you asked for", not "at the
// second". A second-granularity poll would multiply the cross-tenant scan by 30
// to buy precision no editorial workflow has ever needed, and the scan is the
// one query here that touches every tenant's rows.
const DefaultPollInterval = 30 * time.Second

// markTimeout bounds the detached failure-marking that must still land while
// the process is shutting down. Same value and same reason as the outbox
// worker's.
const markTimeout = 5 * time.Second

// Tx is the transaction-bound repository this worker needs. It is a NARROWED
// view of repository.ContentRepository, listing the six operations one
// execution performs — which is also the readable statement of how much of the
// content plane an unauthenticated background process can reach.
type Tx interface {
	ClaimDueEntrySchedule(ctx context.Context, tenantID string, id uuid.UUID, now time.Time) (*domain.EntrySchedule, error)
	GetContentTypeByID(ctx context.Context, tenantID string, id uuid.UUID) (*domain.ContentType, error)
	GetEntry(ctx context.Context, tenantID string, contentTypeID, id uuid.UUID) (*domain.Entry, error)
	SetEntryPublishState(ctx context.Context, e *domain.Entry, status string, publishedAt *time.Time, opts ...domain.PublishOrigin) error
	FinishEntrySchedule(ctx context.Context, tenantID string, id uuid.UUID, state string, executedAt *time.Time, errMsg string) error
	RecordActivity(ctx context.Context, a *domain.Activity) error
}

// Store is what the worker needs outside a transaction.
type Store interface {
	// ListDueEntrySchedules is the cross-tenant candidate scan. Its rows are not
	// claims — see the repository implementation.
	ListDueEntrySchedules(ctx context.Context, now time.Time, limit int) ([]*domain.EntrySchedule, error)
	// InTx runs fn in one transaction scoped to tenantID. The whole execution
	// happens inside it, because the claim is a row lock and a lock released
	// early is not a claim.
	InTx(ctx context.Context, tenantID string, fn func(Tx) error) error
	// FinishEntrySchedule is called OUTSIDE the transaction, and only to record
	// a failure — the transaction that failed took the terminal state down with
	// it when it rolled back.
	FinishEntrySchedule(ctx context.Context, tenantID string, id uuid.UUID, state string, executedAt *time.Time, errMsg string) error
}

// NewStore adapts the content repository to Store.
//
// The adapter exists for one signature: ContentRepository.WithTx hands its
// callback a full ContentRepository, and taking that shape here would make the
// worker's dependency the entire content plane — 60-odd methods, of which it
// uses six. The narrowing is what lets the worker's tests state their fake in a
// page rather than in a file.
func NewStore(repo repository.ContentRepository) Store { return repoStore{repo: repo} }

type repoStore struct{ repo repository.ContentRepository }

func (s repoStore) ListDueEntrySchedules(ctx context.Context, now time.Time, limit int) ([]*domain.EntrySchedule, error) {
	return s.repo.ListDueEntrySchedules(ctx, now, limit)
}

func (s repoStore) InTx(ctx context.Context, tenantID string, fn func(Tx) error) error {
	return s.repo.WithTx(ctx, tenantID, func(bound repository.ContentRepository) error { return fn(bound) })
}

func (s repoStore) FinishEntrySchedule(ctx context.Context, tenantID string, id uuid.UUID, state string, executedAt *time.Time, errMsg string) error {
	return s.repo.FinishEntrySchedule(ctx, tenantID, id, state, executedAt, errMsg)
}

// Worker polls for due schedules and executes them.
type Worker struct {
	store Store
	batch int
	// now is the worker's clock, injectable for the same reason the service's
	// is: "did this become due" is a claim about time, and a test that can only
	// make it true by waiting is a test that costs its own wall-clock to run.
	now func() time.Time
	// notify pushes "your schedule succeeded/went stale/failed" to RequestedBy;
	// nil = this deployment has no notification plane wired, and the worker's
	// pre-ADR-023 behavior (activity line only, no push) is unchanged.
	notify Notifier
}

func NewWorker(store Store) *Worker {
	return &Worker{store: store, batch: 0}
}

// WithClock replaces the worker's clock. Tests only.
func (w *Worker) WithClock(now func() time.Time) *Worker {
	w.now = now
	return w
}

// WithNotifier enables the three pushes execute/recordStale/markFailed send.
// Nil is a no-op, not a refusal — see Notifier's doc and content/service's
// WithNotifier for why a missing notifier degrades a convenience rather than
// blocking anything: the schedule still executes, the activity line still
// records it, and the console's own poll of GET /schedules still shows the
// outcome. The push is an added nudge, not the record of truth.
func (w *Worker) WithNotifier(n Notifier) *Worker {
	w.notify = n
	return w
}

// WithBatchSize bounds one tick's work. Zero leaves the repository's default.
func (w *Worker) WithBatchSize(n int) *Worker {
	w.batch = n
	return w
}

func (w *Worker) clock() time.Time {
	if w.now != nil {
		return w.now().UTC()
	}
	return time.Now().UTC()
}

// Run blocks until ctx is cancelled, polling at interval.
//
// A tick's failures are logged and never returned: this loop must outlive any
// single bad schedule, or one entry whose content type was deleted would stop
// every other tenant's releases.
func (w *Worker) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.Tick(ctx); err != nil {
				log.Printf("content scheduler: %v", err)
			}
		}
	}
}

// Tick runs one poll. Exported so tests can drive the worker a step at a time
// rather than racing a ticker.
func (w *Worker) Tick(ctx context.Context) error {
	now := w.clock()
	due, err := w.store.ListDueEntrySchedules(ctx, now, w.batch)
	if err != nil {
		return fmt.Errorf("list due schedules: %w", err)
	}
	for _, s := range due {
		if ctx.Err() != nil {
			// Shutdown. What is left is still due on the next start, and
			// stopping here is what keeps a half-drained batch from being
			// half-marked.
			return nil
		}
		if err := w.execute(ctx, s, now); err != nil {
			w.markFailed(ctx, s, err)
		}
	}
	return nil
}

// execute performs one schedule, entirely inside one transaction.
//
// THE ORDER OF THE TWO WRITES IS LOAD-BEARING. The schedule is moved to `done`
// BEFORE the publish, because SetEntryPublishState supersedes any pending
// schedule on the entry it touches — including, if the order were reversed,
// this one. The terminal state would then depend on which UPDATE ran last, and
// a release performed by the scheduler would be recorded as one performed by
// hand. Marking first makes that unreachable rather than merely unlikely; both
// writes are in the same transaction, so nothing is observable in between.
func (w *Worker) execute(ctx context.Context, s *domain.EntrySchedule, now time.Time) error {
	// Filled in by the closure below, ONLY on a path that returns nil — a
	// notification about a transaction that rolled back would be a lie, the
	// same rule notifyProposalFiled's doc states for the content service. Fired
	// after InTx returns, never from inside it: the closure's return nil is not
	// yet a commit, and this worker has no way to hook "after commit, before the
	// caller sees success" other than waiting for InTx itself to return.
	var pending *scheduleNotice
	err := w.store.InTx(ctx, s.TenantID, func(tx Tx) error {
		claimed, err := tx.ClaimDueEntrySchedule(ctx, s.TenantID, s.ID, now)
		if err != nil {
			return fmt.Errorf("claim schedule %s: %w", s.ID, err)
		}
		if claimed == nil {
			// Another replica holds the lock, or the row stopped being pending
			// between the scan and here — an editor cancelled it, or someone
			// published by hand. All three are correct outcomes and none of them
			// is this worker's business.
			return nil
		}
		ct, err := tx.GetContentTypeByID(ctx, claimed.TenantID, claimed.ContentTypeID)
		if err != nil {
			return fmt.Errorf("load content type %s: %w", claimed.ContentTypeID, err)
		}
		entry, err := tx.GetEntry(ctx, claimed.TenantID, claimed.ContentTypeID, claimed.EntryID)
		if err != nil {
			return fmt.Errorf("load entry %s: %w", claimed.EntryID, err)
		}
		// THE ONE RE-CHECK, AND ONLY FOR publish. UpdateEntry already invalidates
		// a pending publish schedule in the transaction that moves the working
		// copy, so this fires only for the window between that scan and this lock
		// — and for the version bumps no entry write path makes, such as a bulk
		// field rename. Both are real: the first is milliseconds wide and the
		// second has no other guard at all.
		//
		// An unpublish is NOT checked, and PinsVersion is where that ruling
		// lives. Its PinnedVersion is a note about the snapshot it will retract,
		// not a condition — a takedown must still happen even though someone
		// edited the working copy in between, because the working copy is not
		// what it takes down (ADR-017 §2).
		//
		// The mismatch is not an error. Nobody did anything wrong; the content
		// simply moved past the approval, and refusing to publish it is the
		// whole point of the pin (ADR-014 §1). It gets an activity line for the
		// reason the SQL path does: an invalidation nobody is told about is a
		// release that fails in silence.
		if claimed.PinsVersion() && entry.Version != claimed.PinnedVersion {
			if err := tx.FinishEntrySchedule(ctx, claimed.TenantID, claimed.ID, domain.ScheduleStateStale, nil, ""); err != nil {
				return fmt.Errorf("mark schedule %s stale: %w", claimed.ID, err)
			}
			if err := w.recordStale(ctx, tx, claimed, ct, entry, now); err != nil {
				return err
			}
			pending = staleNotice(claimed, entry)
			return nil
		}
		executedAt := now
		if err := tx.FinishEntrySchedule(ctx, claimed.TenantID, claimed.ID, domain.ScheduleStateDone, &executedAt, ""); err != nil {
			return fmt.Errorf("mark schedule %s done: %w", claimed.ID, err)
		}
		status := domain.ScheduleTargetStatus(claimed.Action)
		publishedAt := entry.PublishedAt
		if status == domain.StatusPublished {
			// Keep the first-release timestamp across a re-publish, exactly as
			// SetEntryStatus does. Two spellings of one rule, and they must not
			// drift: published_at means "when this went live", and a scheduled
			// re-release that reset it would make the two paths disagree about
			// what the column means.
			if publishedAt == nil {
				publishedAt = &executedAt
			}
		} else {
			publishedAt = nil
		}
		// PROVENANCE NAMES THE PERSON, NOT THE PROCESS. The requester approved
		// this release; the worker only carried it. Writing a service actor here
		// would make published_by unanswerable for every scheduled release —
		// and published_by is the column ADR-014 §4 exists to keep truthful.
		requester := claimed.RequestedBy
		entry.UpdatedAt = now
		entry.UpdatedBy = &requester
		kind := domain.ActorKindHuman
		entry.UpdatedByKind = &kind
		entry.UpdatedByAgent = nil
		entry.PublishedBy = &requester
		// The publish revision this write records (ADR-018) has to carry the SAME
		// mechanism as the activity line record() writes below, or the two
		// permanent records of one release disagree about whether a person
		// pressed the button. The pair is built once, here, and the biconditional
		// CHECK on both tables refuses either half arriving without the other.
		originScheduleID := claimed.ID
		origin := domain.PublishOrigin{Via: domain.ActivityViaSchedule, ScheduleID: &originScheduleID}
		if err := tx.SetEntryPublishState(ctx, entry, status, publishedAt, origin); err != nil {
			return fmt.Errorf("set publish state for entry %s: %w", claimed.EntryID, err)
		}
		if err := w.record(ctx, tx, claimed, ct, entry, now); err != nil {
			return err
		}
		pending = successNotice(claimed, entry)
		return nil
	})
	if err != nil {
		return err
	}
	if pending != nil {
		w.notifyRequester(ctx, s, pending)
	}
	return nil
}

// scheduleNotice is what execute's closure hands to the notifier call it
// cannot safely make from inside the still-open transaction.
type scheduleNotice struct {
	kind, title, body, link string
}

// successNotice builds the push for a schedule that ran to completion. COUNTS,
// NOT CONTENT for the same reason notifyEntryPendingReview's body is: the
// recipient is the requester of record, but ReadRoles can have changed since
// they filed the schedule (ADR-023), so the type's name and label stay off
// the wire and only the entry id — resolved server-side, behind the same
// guardTypeRead check every other entry read goes through — appears in the
// link.
func successNotice(s *domain.EntrySchedule, e *domain.Entry) *scheduleNotice {
	verb := "published"
	if s.Action == domain.ScheduleActionUnpublish {
		verb = "unpublished"
	}
	return &scheduleNotice{
		kind:  notifdomain.KindScheduleSucceeded,
		title: "Scheduled release succeeded",
		body:  fmt.Sprintf("Your scheduled release ran: the entry was %s as planned.", verb),
		link:  fmt.Sprintf("/content/entries/%s", e.ID),
	}
}

// staleNotice builds the push for a schedule the worker discovered was
// invalidated by a later edit (see execute's PinsVersion comment for why this
// is not an error). The requester is told because ADR-017 records this as the
// trigger for them to re-decide — a stale schedule that notified nobody would
// be a release quietly not happening, the same failure mode ADR-023 exists to
// close for proposals and pending review.
func staleNotice(s *domain.EntrySchedule, e *domain.Entry) *scheduleNotice {
	return &scheduleNotice{
		kind:  notifdomain.KindScheduleStale,
		title: "Scheduled release did not run",
		body:  "Your scheduled release was skipped: the entry changed after you approved it, so the approval no longer matches what would have gone live.",
		link:  fmt.Sprintf("/content/entries/%s", e.ID),
	}
}

// notifyRequester fires the push, best-effort and detached exactly like
// notifyProposalFiled: the schedule already ran (or was marked stale) and
// committed by the time this is called, so a notification outage must not
// retroactively make that untrue, and nothing downstream of this call still
// needs ctx to be live.
func (w *Worker) notifyRequester(ctx context.Context, s *domain.EntrySchedule, n *scheduleNotice) {
	if w.notify == nil {
		return
	}
	if err := w.notify.NotifyUser(context.WithoutCancel(ctx), s.RequestedBy, s.TenantID, n.kind, n.title, n.body, n.link); err != nil {
		log.Printf("content scheduler: notify requester for schedule %s: %v", s.ID, err)
	}
}

// record writes the ordinary publish/unpublish activity line, marked with the
// mechanism that produced it.
//
// It builds the row by hand rather than through the service's activityRec, and
// it has to: that recorder reads the actor off the request subject, and there
// is no request here. What it does NOT do is invent a different vocabulary — a
// scheduled release is an entry.publish like any other, or an activity stream
// that claims to show who published what would be missing every release that
// happened out of hours.
func (w *Worker) record(ctx context.Context, tx Tx, s *domain.EntrySchedule, ct *domain.ContentType, e *domain.Entry, now time.Time) error {
	action := domain.ActivityEntryPublish
	if s.Action == domain.ScheduleActionUnpublish {
		action = domain.ActivityEntryUnpublish
	}
	requester := s.RequestedBy
	scheduleID := s.ID
	entryID := e.ID
	row := &domain.Activity{
		ID:         uuid.New(),
		TenantID:   s.TenantID,
		OccurredAt: now,
		// Human, and the id is the requester's. There is no third actor in this
		// story: a worker is not a party to a release, it is how the release was
		// carried. `via` below is where the machine appears.
		ActorKind:   domain.ActorKindHuman,
		ActorUserID: &requester,
		Action:      action,
		TargetType:  ct.Name,
		// The same fenced title every other activity row carries: TitleFor
		// refuses a read-restricted field, so a scheduled release cannot
		// denormalise into the stream a value a hand-pressed one would not.
		TargetEntryID: &entryID,
		TargetTitle:   domain.TitleFor(ct.Fields, e.Payload),
		Outcome:       domain.ActivityOutcomeSuccess,
		Via:           domain.ActivityViaSchedule,
		ViaScheduleID: &scheduleID,
	}
	if err := tx.RecordActivity(ctx, row); err != nil {
		return fmt.Errorf("record %s activity for entry %s: %w", action, e.ID, err)
	}
	return nil
}

// recordStale writes the entry.schedule.stale line for the invalidation the
// worker discovered itself.
//
// The SQL half of the same rule lives in markSchedulesStale, which fires when an
// editor's save is what overtook the approval. Two spellings, one meaning, and
// they must agree on the detail keys — domain.ScheduleStaleDetails is the Go
// one and the integration test compares them.
//
// THE ACTOR IS THE REQUESTER, not the worker. The thing that happened is "this
// person's approval expired"; naming a service actor would file it under a
// machine nobody can ask about it. Via stays empty because nothing was carried
// out on the requester's authority — the whole point of the line is that
// nothing was.
func (w *Worker) recordStale(ctx context.Context, tx Tx, s *domain.EntrySchedule, ct *domain.ContentType, e *domain.Entry, now time.Time) error {
	requester := s.RequestedBy
	entryID := e.ID
	row := &domain.Activity{
		ID:            uuid.New(),
		TenantID:      s.TenantID,
		OccurredAt:    now,
		ActorKind:     domain.ActorKindHuman,
		ActorUserID:   &requester,
		Action:        domain.ActivityEntryScheduleStale,
		TargetType:    ct.Name,
		TargetEntryID: &entryID,
		TargetTitle:   domain.TitleFor(ct.Fields, e.Payload),
		Outcome:       domain.ActivityOutcomeSuccess,
		Details:       domain.ScheduleStaleDetails(s.ID, s.PinnedVersion, e.Version),
	}
	if err := tx.RecordActivity(ctx, row); err != nil {
		return fmt.Errorf("record stale activity for schedule %s: %w", s.ID, err)
	}
	return nil
}

// markFailed records why an execution did not happen, in a fresh transaction —
// the one that failed rolled its terminal state back along with everything
// else.
//
// NOT ON SHUTDOWN. A cancelled context means the process is stopping, not that
// the schedule is bad; the row is still pending, still due, and the next start
// will find it. Marking it failed here would turn every deploy into a handful
// of releases that silently never happen, which is the failure mode this whole
// feature is supposed to remove.
func (w *Worker) markFailed(ctx context.Context, s *domain.EntrySchedule, cause error) {
	if ctx.Err() != nil {
		log.Printf("content scheduler: schedule %s left pending (shutting down): %v", s.ID, cause)
		return
	}
	msg := cause.Error()
	// The column is TEXT with no cap, but an error chain can carry a whole SQL
	// statement, and this string is shown to an editor. Truncate to something a
	// person reads.
	if len(msg) > 500 {
		msg = msg[:500]
	}
	log.Printf("content scheduler: schedule %s failed: %v", s.ID, cause)
	// Detached from ctx like the outbox worker's mark: a cancellation arriving
	// between the check above and this call must not lose the reason.
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markTimeout)
	defer cancel()
	at := w.clock()
	if err := w.store.FinishEntrySchedule(markCtx, s.TenantID, s.ID, domain.ScheduleStateFailed, &at, msg); err != nil {
		log.Printf("content scheduler: mark schedule %s failed: %v", s.ID, err)
		// The failure was not durably recorded either — pushing a notice for a
		// state the store itself never landed would tell the requester something
		// the schedule's own row does not say. Silence here matches markFailed's
		// existing rule for every other write in this function: this method
		// already returns nothing, so a caller reading FinishEntrySchedule's own
		// error has no path back to try again.
		return
	}
	// msg is the SAME string FinishEntrySchedule just wrote to the schedule's
	// error column — already shown to an editor on the schedule's own record
	// (see the truncation comment above), so repeating it in the push is not a
	// new disclosure.
	w.notifyRequester(ctx, s, &scheduleNotice{
		kind:  notifdomain.KindScheduleFailed,
		title: "Scheduled release failed",
		body:  fmt.Sprintf("Your scheduled release could not run: %s", msg),
		link:  fmt.Sprintf("/content/entries/%s", s.EntryID),
	})
}
