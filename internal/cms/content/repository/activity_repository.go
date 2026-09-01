package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// --- activity record (ADR-014 §3) ---------------------------------------------

// ActivityFilter narrows one tenant's stream. Every field but TenantID is
// optional; the zero filter is "this tenant's most recent activity".
type ActivityFilter struct {
	TenantID string
	// EntryID narrows to one entry — the question §1's per-field author
	// attribution asks on the release screen (step 4), which is why the partial
	// index in migration 000032 exists.
	EntryID *uuid.UUID
	// Since bounds the window from below, exclusive. §1's attribution uses it to
	// mean "since this entry was last published", the boundary that keeps the
	// previous release's changes from being attributed into this one's diff.
	Since *time.Time
	// ViewerRole and ViewerUserID carry ADR-009's data layer into the query, as
	// PendingReviewFilter's do. NOT optional, and an empty role deliberately
	// matches no read_roles list: a caller the service failed to describe sees
	// less, never more.
	ViewerRole   string
	ViewerUserID uuid.UUID
	Limit        int
}

// activityLimitDefault / activityLimitMax bound a stream read. The console
// paints a page; nothing needs the whole history in one response, and this
// table is the one that grows without an editorial act to slow it down.
const (
	activityLimitDefault = 50
	activityLimitMax     = 200
)

const activitySelectColumns = `id, tenant_id, occurred_at,
	actor_kind, actor_user_id, actor_agent_id,
	action, target_type, target_entry_id, target_title,
	outcome, error_code, changed_keys`

// RecordActivity appends one row.
//
// It joins the caller's transaction when there is one, through withTenant like
// every other operation here. That is not how the entry write paths use it,
// and the distinction is worth stating because the comment above emitEntryEvent
// makes the opposite promise about events:
//
//   - The service records AFTER the operation returns, outside its transaction.
//     Ordering, not atomicity, is what buys the property that matters: a crash
//     between the two loses a row, and no ordering produces the dangerous
//     failure, a line claiming a write that was rolled back. An event has to be
//     transactional because a consumer acts on it; nobody acts on a record.
//   - ApplySchema is the exception and gets atomicity for free: the verbs it
//     drives run against a transaction-bound repository, so the rows they
//     record roll back with the apply that failed. That is right — a schema
//     change that did not land should not be reported as one that did.
func (r *PostgresContentRepository) RecordActivity(ctx context.Context, a *domain.Activity) error {
	return r.withTenant(ctx, a.TenantID, func(q querier) error {
		_, err := q.Exec(ctx, `
			INSERT INTO content_activity
				(id, tenant_id, occurred_at, actor_kind, actor_user_id, actor_agent_id,
				 action, target_type, target_entry_id, target_title,
				 outcome, error_code, changed_keys)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			a.ID, a.TenantID, a.OccurredAt, a.ActorKind, a.ActorUserID, a.ActorAgentID,
			a.Action, a.TargetType, a.TargetEntryID, a.TargetTitle,
			a.Outcome, a.ErrorCode, orEmptyKeys(a.ChangedKeys),
		)
		if err != nil {
			return fmt.Errorf("insert activity: %w", err)
		}
		return nil
	})
}

// ListActivity returns the stream newest first.
//
// Ordering breaks ties on id, not on occurred_at alone: several rows of one
// request can share a timestamp to the microsecond, and an unstable order there
// would make the same page render differently on a refresh.
func (r *PostgresContentRepository) ListActivity(ctx context.Context, f ActivityFilter) ([]*domain.Activity, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = activityLimitDefault
	}
	if limit > activityLimitMax {
		limit = activityLimitMax
	}
	// ADR-009's data layer, and it has to be spelled differently here than in the
	// queue's pendingReviewVisibleExpr because this table holds no
	// content_type_id — target_type is the type's NAME, denormalised at write
	// time, and it is '' for the actions that concern no single type (schema
	// verbs, collection reads). So the join is by name, and the two halves are:
	//
	//   * READ LIST — hide a row whose type the caller's role may not read. The
	//     leak this closes is concrete: target_title is a real content value,
	//     fenced only at the FIELD level (domain.TitleFor refuses a
	//     read-restricted field), so before this an editor off a type's
	//     read_roles still read that type's titles out of the stream.
	//
	//   * CONFINEMENT — where the caller's role is own_only on the type, show
	//     only rows the caller is the actor of. This is a JUDGEMENT and not a
	//     transcription of guardOwned: the row records an ACTION, and it cannot
	//     be resolved against the entry's created_by the way the queue's is,
	//     because the entry may be deleted — this table denormalises the title
	//     precisely so the record outlives the row. "You see only your own
	//     entries of this type, so you see only your own actions on it" is the
	//     reading that survives deletion and needs no join to entries.
	//
	// BOTH HALVES FAIL OPEN ON A NAME THAT NO LONGER RESOLVES, and that is a
	// deliberate trade rather than an oversight. A type can be renamed; these
	// rows keep the old name forever, since the table is append-only. Hiding
	// unresolvable names would be the fail-closed reflex, and it would silently
	// delete history from the audit record for EVERY reader, owners included,
	// the first time anyone renames a type — destroying the thing the table
	// exists to be. Showing them leaks, at most, the old name and an
	// already-field-fenced title of a type that has since been renamed. The
	// cheaper loss is the leak. Listed in ADR-014's 未解項.
	visible := `(
		NOT EXISTS (
			SELECT 1 FROM content_types ct
			WHERE ct.tenant_id = content_activity.tenant_id
			  AND ct.name = content_activity.target_type
			  AND cardinality(ct.read_roles) > 0
			  AND NOT ($2 = ANY(ct.read_roles))
		)
		AND (
			NOT EXISTS (
				SELECT 1 FROM content_types ct
				WHERE ct.tenant_id = content_activity.tenant_id
				  AND ct.name = content_activity.target_type
				  AND $2 = ANY(ct.own_only_roles)
			)
			OR content_activity.actor_user_id = $3
		)
	)`
	sql := `SELECT ` + activitySelectColumns + ` FROM content_activity WHERE tenant_id = $1 AND ` + visible
	args := []any{f.TenantID, f.ViewerRole, f.ViewerUserID}
	if f.EntryID != nil {
		args = append(args, *f.EntryID)
		sql += fmt.Sprintf(" AND target_entry_id = $%d", len(args))
	}
	if f.Since != nil {
		args = append(args, *f.Since)
		sql += fmt.Sprintf(" AND occurred_at > $%d", len(args))
	}
	args = append(args, limit)
	sql += fmt.Sprintf(" ORDER BY occurred_at DESC, id DESC LIMIT $%d", len(args))

	var out []*domain.Activity
	err := r.withTenant(ctx, f.TenantID, func(q querier) error {
		rows, err := q.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a domain.Activity
			if err := rows.Scan(
				&a.ID, &a.TenantID, &a.OccurredAt,
				&a.ActorKind, &a.ActorUserID, &a.ActorAgentID,
				&a.Action, &a.TargetType, &a.TargetEntryID, &a.TargetTitle,
				&a.Outcome, &a.ErrorCode, &a.ChangedKeys,
			); err != nil {
				return err
			}
			out = append(out, &a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list activity: %w", err)
	}
	return out, nil
}

// EntryFieldAuthors answers "who last changed each field of this entry"
// (ADR-014 §6, the release screen's per-field attribution).
//
// One row per KEY, not per activity line: the question is per field, and
// answering it by handing the caller a page of activity and letting it fold
// would put the newest-wins rule in the caller — where a page boundary can hide
// the answer. DISTINCT ON does the fold in the database, so the result is
// bounded by the number of keys the entry has rather than by how many times it
// was edited. An entry patched three hundred times since its last release still
// attributes every field.
//
// `since` is EXCLUSIVE and means "since the snapshot being compared against was
// taken". Without it, a write that was already published would be offered as the
// author of a change made after it by something that records no keys — a schema
// apply rewriting payloads is the reachable case. The caller supplies it; see
// the service for why the bound it can supply is a lower one.
//
// NO `outcome = 'success'` PREDICATE, and that is a decision rather than an
// omission: a denied row cannot carry keys at all
// (content_activity_denied_changes_nothing_check, migration 000032), so unnest
// yields nothing for one and the filter could never fire. Adding it would be a
// second layer with no reachable case to test it — see the codebase rule that a
// redundant guard needs a white-box test or it is only decoration.
func (r *PostgresContentRepository) EntryFieldAuthors(ctx context.Context, tenantID string, entryID uuid.UUID, since *time.Time) ([]*domain.FieldAuthor, error) {
	sql := `
		SELECT DISTINCT ON (k.key)
			k.key, a.actor_kind, a.actor_user_id, a.actor_agent_id, a.occurred_at
		FROM content_activity a, unnest(a.changed_keys) AS k(key)
		WHERE a.tenant_id = $1 AND a.target_entry_id = $2`
	args := []any{tenantID, entryID}
	if since != nil {
		args = append(args, *since)
		sql += fmt.Sprintf(" AND a.occurred_at > $%d", len(args))
	}
	// Ties break on id for the same reason ListActivity's do: several rows of one
	// request share a timestamp to the microsecond, and an unstable choice
	// between them would make the same field attribute to a different actor on a
	// refresh.
	sql += ` ORDER BY k.key, a.occurred_at DESC, a.id DESC`

	var out []*domain.FieldAuthor
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		rows, err := q.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var fa domain.FieldAuthor
			if err := rows.Scan(&fa.Key, &fa.ActorKind, &fa.ActorUserID, &fa.ActorAgentID, &fa.OccurredAt); err != nil {
				return err
			}
			out = append(out, &fa)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("entry field authors: %w", err)
	}
	return out, nil
}

// orEmptyKeys sends [] rather than NULL for an absent key list, matching the
// column's NOT NULL DEFAULT '{}'. Same reason orEmptyRoles exists: a nil slice
// reaching a NOT NULL array column is a constraint violation at write time,
// which is a worse way to learn about it than here.
func orEmptyKeys(k []string) []string {
	if k == nil {
		return []string{}
	}
	return k
}
