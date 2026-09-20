package domain

import (
	"time"

	"github.com/google/uuid"
)

// Kind* enumerates the recognized notification kinds (2.5a). This list is the
// single Go source of truth and is mirrored by the CHECK constraint migration
// 000049 adds — see that migration's comment for why the constraint lives in
// the database rather than only here.
const (
	KindGeneral            = "general"
	KindSchemaProposal     = "schema_proposal"
	KindEntryPendingReview = "entry_pending_review"
	KindScheduleSucceeded  = "schedule_succeeded"
	KindScheduleFailed     = "schedule_failed"
	KindScheduleStale      = "schedule_stale"
	KindWebhookDead        = "webhook_dead"
	// KindEntryChangesRequested is the eighth value. The table it names lives
	// in internal/cms/content/migrations/000051 (ADR-014 Amendment: review
	// decisions), but the CHECK constraint below widens in this package's own
	// migration 000052, not 000051 — see that migration's comment for why the
	// two had to be split across packages. ADR-023's own "Trigger conditions"
	// section named this exact expansion in advance.
	KindEntryChangesRequested = "entry_changes_requested"
)

// ValidKinds lists every kind the database accepts, in the same order the
// CHECK constraint does. No caller in this codebase validates against it
// today — see migration 000049's comment on why application-layer validation
// was not the chosen enforcement — but it exists so a future caller that DOES
// take kind from outside this package has one list to check instead of a
// second copy of the CHECK's literal set.
var ValidKinds = []string{
	KindGeneral,
	KindSchemaProposal,
	KindEntryPendingReview,
	KindScheduleSucceeded,
	KindScheduleFailed,
	KindScheduleStale,
	KindWebhookDead,
	KindEntryChangesRequested,
}

type Notification struct {
	ID     uuid.UUID
	UserID uuid.UUID
	// TenantID scopes the notice to one tenant. Nil means the notice is not
	// tied to a single tenant — GET /api/v1/notifications reads a nil row
	// regardless of which tenant is currently active (see migration 000049's
	// comment). Every trigger wired in 2.5a sets this; nil is reserved for a
	// future platform-wide notice and for rows the self-notify
	// POST /notifications endpoint writes, which names no tenant.
	TenantID *uuid.UUID
	Title    string
	Body     string
	// Kind classifies the notice for the reader (console icon/routing/
	// grouping) — see the Kind* constants above. It is a hint about content,
	// never a scope the row is checked against.
	Kind string
	// Link is an app-relative path the console can navigate the reader to
	// (e.g. "/schema/proposals/<id>"), or nil when the notice has nowhere more
	// specific to send them than the notification list itself.
	Link      *string
	ReadAt    *time.Time
	CreatedAt time.Time
}
