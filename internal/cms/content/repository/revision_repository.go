package repository

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// --- entry revisions (ADR-014 §5) ---------------------------------------------

// recordEntryRevision appends the row's CURRENT state as a revision.
//
// It takes no payload and no actor. Both are read back out of `entries` inside
// the caller's transaction, and that is the point rather than a shortcut: the
// revision is then a copy of what the row actually holds, not a second
// rendering of what the caller believed it was writing. The two cannot drift,
// and the columns 000031 leaves nullable arrive nullable instead of being
// defaulted into a specific false answer on the way past.
//
// IT MUST RUN IN THE WRITE'S OWN TRANSACTION — it is called with the caller's
// querier, never with the pool. This is the opposite choice from RecordActivity
// (see its doc comment, which argues ordering is enough because nobody acts on
// a record) and the difference is what each row claims. An activity line that
// survives a rolled-back write is a line about an attempt. A REVISION that
// survives one is a version of the content that never existed, offered to
// whoever later reads the history as the thing to put back.
func recordEntryRevision(ctx context.Context, q querier, tenantID string, entryID uuid.UUID) error {
	// NO `ON CONFLICT DO NOTHING`, deliberately. It looks like cheap insurance
	// and it is not earned: withTenant has no retry loop, and version only ever
	// advances, so the same (entry_id, version) cannot present itself twice
	// unless something that is supposed to be impossible has happened — two
	// applied writes claiming one version, which is the optimistic lock failing.
	// Swallowing that would hide it behind a revision that silently is not the
	// content of the write that just ran. Refusing loudly is the same call
	// guardWritableKeys makes for the same reason.
	if _, err := q.Exec(ctx, `
		INSERT INTO entry_revisions
			(entry_id, version, tenant_id, payload, author_kind, author_user_id, author_agent_id, created_at)
		SELECT id, version, tenant_id, payload, updated_by_kind, updated_by, updated_by_agent, updated_at
		FROM entries
		WHERE tenant_id = $1 AND id = $2`,
		tenantID, entryID,
	); err != nil {
		return fmt.Errorf("insert entry revision: %w", err)
	}
	return nil
}

const revisionSelectColumns = `entry_id, version, tenant_id, payload,
	author_kind, author_user_id, author_agent_id, created_at`

// entryRevisionListLimit is william's 0806 retention ruling — "每 entry 保留最新
// 50 筆" — landed on the READ side rather than as a purge.
//
// WHY THE READ SIDE, when the ruling was phrased as retention. A purge would
// have to delete rows, and "this table has no purge job and no delete path at
// all" is not a description in 000034 — it is a PREMISE of the ON DELETE
// CASCADE argument, which reasons that keeping revisions past an entry delete
// would strand tenant content no product path can remove. Introducing a delete
// path re-opens that argument, and it is entangled with an unruled question
// (whether content:delete opens to agents). Capping the read delivers the half
// of the ruling that is about what anyone can SEE, at the only place rows leave
// this repository, without touching the half that is about what is stored.
//
// WHAT IS THEREFORE NOT DELIVERED, stated plainly so nobody reads the ADR's
// "50" and this table's "unbounded growth" as one of them being stale: the rows
// past 50 are still written, still stored, and still count against disk. The
// storage half of the ruling is deferred, not done, and it is on ADR-014's
// 未解項 as such.
//
// RAISING THIS CONSTANT IS A RULING, NOT A TUNING. A caller that needs to walk
// deeper history is asking for exactly what the cap refused; today no caller
// exists, and the first one arrives together with §6's masking anyway.
const entryRevisionListLimit = 50

// ListEntryRevisions returns one entry's newest stored versions, newest first,
// at most entryRevisionListLimit of them.
//
// DELIBERATELY NOT ON THE ContentRepository INTERFACE. §5 is "store"; there is
// no service or handler caller, and this exists so the tests that prove the
// rows are written can read them. Promoting it is not a one-line change: these
// rows hold every restricted field's value in full, so §6's field-level masking
// (驗證計畫第 10 條) has to apply before any of this reaches a response.
func (r *PostgresContentRepository) ListEntryRevisions(ctx context.Context, tenantID string, entryID uuid.UUID) ([]domain.EntryRevision, error) {
	var out []domain.EntryRevision
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		// LIMIT is a bind parameter rather than a literal so the constant above
		// is the single place the number lives — a second spelling inside the
		// SQL string is the one a later edit updates only half of.
		rows, err := q.Query(ctx, `
			SELECT `+revisionSelectColumns+`
			FROM entry_revisions
			WHERE tenant_id = $1 AND entry_id = $2
			ORDER BY version DESC
			LIMIT $3`,
			tenantID, entryID, entryRevisionListLimit,
		)
		if err != nil {
			return fmt.Errorf("list entry revisions: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var rev domain.EntryRevision
			var payload []byte
			if err := rows.Scan(
				&rev.EntryID, &rev.Version, &rev.TenantID, &payload,
				&rev.AuthorKind, &rev.AuthorUserID, &rev.AuthorAgentID, &rev.CreatedAt,
			); err != nil {
				return fmt.Errorf("scan entry revision: %w", err)
			}
			rev.Payload = payload
			out = append(out, rev)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
