package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// --- publish revisions (ADR-018) ----------------------------------------------
//
// The sibling file revision_repository.go stores the WORKING COPY at every
// version. This one stores the LIVE SNAPSHOT at every release. Both are called
// "revisions" by their tables and by the ADRs, and the two are not
// interchangeable in either direction — see domain.EntryPublishRevision.

const publishRevisionColumns = `id, tenant_id, entry_id, revision_no, version,
	published_at, published_by, via, via_schedule_id, created_at`

// recordPublishRevision appends the snapshot the row is CARRYING RIGHT NOW as a
// release, then trims the entry back to its retention cap.
//
// Like recordEntryRevision it takes no payload and no actor: every value is read
// back out of `entries` inside the caller's transaction, so the row is a copy of
// what the database actually holds rather than a second rendering of what the
// caller meant to write. That matters more here than there, because two of the
// values are produced by CASE expressions the caller does not control —
// published_by survives a retract rather than taking the parameter, and
// published_version is version + 1 — so reconstructing them in Go would be
// reconstructing the very logic the statement exists to own.
//
// IT MUST RUN IN THE PUBLISH'S OWN TRANSACTION, for recordEntryRevision's reason
// sharpened by one degree. A stray activity line describes an attempt; a stray
// revision here is a release that never happened, offered by name to whoever
// later says "put revision 7 back" — and unlike the working-copy table, this one
// has a restore path that will do it.
//
// WHAT IT IS NOT CALLED FOR: an unpublish. A retract moves no snapshot (ADR-014
// §5.1 keeps published_payload), so there is no new release to record, and a row
// saying otherwise would put a duplicate of the previous release into the list
// with a later timestamp. The caller decides; this function does not inspect
// status, because a helper that silently no-ops for some callers is a helper
// whose caller stops knowing what it does.
func recordPublishRevision(ctx context.Context, q querier, tenantID string, entryID uuid.UUID, origin domain.PublishOrigin) error {
	if !origin.Valid() {
		// Unreachable from either caller — the service passes the zero value and
		// the worker passes its own schedule's id — so this is a guard against a
		// future third caller, refusing before the CHECK does. The constraint
		// would catch it anyway; failing here names the parameter instead of the
		// column.
		return fmt.Errorf("record publish revision: invalid origin via=%q", origin.Via)
	}

	// The ordinal is allocated as MAX+1 in the same statement that inserts it,
	// which is safe for the reason the optimistic lock is safe rather than by
	// luck: the caller has already UPDATEd this entry row in this transaction, so
	// it holds that row's lock, and a second publish of the same entry cannot be
	// between its own MAX and its own INSERT at the same time. The UNIQUE
	// constraint is the backstop.
	//
	// NO `ON CONFLICT`, on recordEntryRevision's reasoning: a duplicate
	// (entry_id, revision_no) means two committed publishes believed they were
	// the same release, and swallowing that would leave the list quietly missing
	// one. Refusing loudly is the same call guardWritableKeys makes.
	if _, err := q.Exec(ctx, `
		INSERT INTO entry_publish_revisions
			(id, tenant_id, entry_id, revision_no, payload, version, published_at, published_by, via, via_schedule_id)
		SELECT $3, e.tenant_id, e.id,
		       COALESCE((
		           SELECT MAX(r.revision_no) FROM entry_publish_revisions r
		           WHERE r.tenant_id = e.tenant_id AND r.entry_id = e.id
		       ), 0) + 1,
		       e.published_payload, e.published_version, e.updated_at, e.published_by, $4, $5
		FROM entries e
		WHERE e.tenant_id = $1 AND e.id = $2 AND e.published_payload IS NOT NULL`,
		tenantID, entryID, uuid.New(), origin.Via, origin.ScheduleID,
	); err != nil {
		return fmt.Errorf("insert entry publish revision: %w", err)
	}

	// Retention, in the same transaction, as a real DELETE rather than
	// entryRevisionListLimit's read cap — the difference is argued on
	// domain.MaxPublishRevisionsPerEntry.
	//
	// Expressed against MAX rather than as "delete everything below the Nth
	// newest" because the surviving ordinals are contiguous: the purge only ever
	// removes from the bottom, so `revision_no <= max - N` is exactly the excess,
	// with no ORDER BY / OFFSET and no second pass over the rows that are staying.
	// It also self-heals — if a row ever went missing, the next publish trims to
	// the same window instead of drifting one short forever.
	if _, err := q.Exec(ctx, `
		DELETE FROM entry_publish_revisions
		WHERE tenant_id = $1 AND entry_id = $2
		  AND revision_no <= (
		      SELECT MAX(revision_no) FROM entry_publish_revisions
		      WHERE tenant_id = $1 AND entry_id = $2
		  ) - $3`,
		tenantID, entryID, domain.MaxPublishRevisionsPerEntry,
	); err != nil {
		return fmt.Errorf("trim entry publish revisions: %w", err)
	}
	return nil
}

// ListEntryPublishRevisions returns one entry's releases, newest first, WITHOUT
// their payloads.
//
// The omission is the point rather than an optimisation. A list of twenty full
// snapshots is twenty times an entry on the wire for a panel that renders dates
// and names, and — the reason that actually decides it — every payload here
// holds restricted fields in full, so a list endpoint returning them would have
// to mask twenty payloads to render none of them. The payload is fetched one at
// a time by GetEntryPublishRevision, where there is exactly one thing to mask.
//
// No LIMIT parameter: retention already bounds this at
// domain.MaxPublishRevisionsPerEntry rows per entry, so pagination would be
// machinery for a page that cannot exist.
func (r *PostgresContentRepository) ListEntryPublishRevisions(ctx context.Context, tenantID string, entryID uuid.UUID) ([]domain.EntryPublishRevision, error) {
	var out []domain.EntryPublishRevision
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT `+publishRevisionColumns+`
			FROM entry_publish_revisions
			WHERE tenant_id = $1 AND entry_id = $2
			ORDER BY revision_no DESC`,
			tenantID, entryID,
		)
		if err != nil {
			return fmt.Errorf("list entry publish revisions: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			rev, err := scanPublishRevision(rows)
			if err != nil {
				return err
			}
			out = append(out, rev)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetEntryPublishRevision returns one release WITH its payload, or
// apperrors.ErrNotFound.
//
// Not-found here covers three different situations on purpose — no such entry,
// no such ordinal, and an ordinal that retention has already discarded — and the
// caller cannot tell them apart. That is deliberate: distinguishing "never
// existed" from "expired" would need a tombstone per purged row, and the answer
// to all three is the same thing the console has to say, which is that this is
// not something you can restore.
func (r *PostgresContentRepository) GetEntryPublishRevision(ctx context.Context, tenantID string, entryID uuid.UUID, revisionNo int) (*domain.EntryPublishRevision, error) {
	var rev domain.EntryPublishRevision
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		row := q.QueryRow(ctx, `
			SELECT `+publishRevisionColumns+`, payload
			FROM entry_publish_revisions
			WHERE tenant_id = $1 AND entry_id = $2 AND revision_no = $3`,
			tenantID, entryID, revisionNo,
		)
		var payload []byte
		var err error
		rev, err = scanPublishRevisionRow(row, &payload)
		if err != nil {
			return err
		}
		rev.Payload = payload
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperrors.ErrNotFound
		}
		return nil, err
	}
	return &rev, nil
}

// scanner is what pgx.Row and pgx.Rows have in common, so one scan body serves
// the list and the single read. Same shape as scanEntry's.
type publishRevisionScanner interface {
	Scan(dest ...any) error
}

func scanPublishRevision(s publishRevisionScanner) (domain.EntryPublishRevision, error) {
	return scanPublishRevisionRow(s, nil)
}

// scanPublishRevisionRow reads the metadata columns, and the payload too when
// payload is non-nil — the extra column has to be appended in the same order the
// SELECT lists it, which is why the two callers cannot simply share a column
// constant and diverge on the tail.
func scanPublishRevisionRow(s publishRevisionScanner, payload *[]byte) (domain.EntryPublishRevision, error) {
	var rev domain.EntryPublishRevision
	dest := []any{
		&rev.ID, &rev.TenantID, &rev.EntryID, &rev.RevisionNo, &rev.Version,
		&rev.PublishedAt, &rev.PublishedBy, &rev.Via, &rev.ViaScheduleID, &rev.CreatedAt,
	}
	if payload != nil {
		dest = append(dest, payload)
	}
	if err := s.Scan(dest...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return rev, err
		}
		return rev, fmt.Errorf("scan entry publish revision: %w", err)
	}
	return rev, nil
}
