package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/outbox"
)

// PostgresContentRepository persists the runtime-dynamic content model. One
// repository serves every content type: schema lives in content_types /
// content_type_fields and documents live in entries.payload (JSONB).
type PostgresContentRepository struct {
	pool *pgxpool.Pool
	// tx and txTenant are set only on the repository handed to a WithTx
	// callback. When tx is non-nil every operation joins that transaction
	// instead of opening its own, which is what lets several schema verbs
	// commit or roll back as one unit.
	tx       querier
	txTenant string
	// outbox is retained so the repository can adopt the platform's
	// transactional-outbox pattern later; the PoC's core signals do not need
	// integration events, so none are emitted yet.
	outbox outbox.Repository
}

func NewPostgresContentRepository(pool *pgxpool.Pool, ob outbox.Repository) *PostgresContentRepository {
	return &PostgresContentRepository{pool: pool, outbox: ob}
}

const uniqueViolation = "23505"

// querier is the subset of pgx used by the tenant-scoped content queries;
// pgx.Tx satisfies it.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// withTenant runs fn inside a transaction that has the app.tenant_id GUC set,
// so Postgres RLS (TKT-R6b) scopes every statement to this tenant as a second
// isolation layer behind the explicit WHERE clauses. All content_types /
// entries access goes through here; forgetting it would make RLS return zero
// rows (fail-closed) under a production non-superuser role.
// WithTx runs fn against a repository bound to ONE transaction, so a caller
// that must apply several schema verbs together gets all of them or none.
//
// ADR-008's apply is the reason this exists: a half-applied schema is precisely
// the state ADR-007 refuses to produce — a type that gained a required field
// while its entries were not migrated has no PATCHable entry left, and the error
// names a field the caller never touched. Compensating afterwards is not an
// option either, because the reverse of a schema change is another data
// migration, which is the thing ADR-007 rules out one case at a time.
//
// The tenant GUC is set ONCE, when the transaction opens. Every operation
// inside therefore runs under that tenant's RLS scope regardless of what it
// passes, so withTenant refuses a mismatch rather than running under the wrong
// one — the same cross-tenant failure class ADR-007's cascade join addresses,
// arriving through a different door.
func (r *PostgresContentRepository) WithTx(ctx context.Context, tenantID string, fn func(ContentRepository) error) error {
	// Already inside one. Beginning a nested transaction here would announce an
	// atomicity boundary that pgx does not give us without savepoints, and the
	// caller already has the guarantee it asked for.
	if r.tx != nil {
		if tenantID != r.txTenant {
			return fmt.Errorf("content: transaction is bound to tenant %q, cannot nest for tenant %q", r.txTenant, tenantID)
		}
		return fn(r)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	bound := &PostgresContentRepository{pool: r.pool, tx: tx, txTenant: tenantID, outbox: r.outbox}
	if err := fn(bound); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresContentRepository) withTenant(ctx context.Context, tenantID string, fn func(querier) error) error {
	// Bound to a caller-owned transaction: join it. Committing here would end
	// the caller's unit of work halfway through it.
	if r.tx != nil {
		if tenantID != r.txTenant {
			return fmt.Errorf("content: transaction is bound to tenant %q, refusing an operation for tenant %q — app.tenant_id was set once and this would run under the wrong RLS scope", r.txTenant, tenantID)
		}
		return fn(r.tx)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// set_config(_, _, true) = transaction-local; cleared at commit/rollback so
	// the pooled connection carries no tenant into the next request.
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- schema: content types + fields ----------------------------------------

// contentTypeColumns is shared by GetContentTypeByName and ListContentTypes for
// the reason entrySelectColumns is shared by the two entry reads, and with a
// sharper failure mode. A permission column written but not read back comes back
// EMPTY, empty means unrestricted, and the collection is served to everyone it
// was just closed to — while the single-type path, which did read it, refuses.
// One list, edited once.
const contentTypeColumns = `id, tenant_id, name, label,
	read_roles, write_roles, own_only_roles, created_at, updated_at`

// CreateContentType inserts the type and all its fields in one transaction.
func (r *PostgresContentRepository) CreateContentType(ctx context.Context, ct *domain.ContentType) error {
	return r.withTenant(ctx, ct.TenantID, func(q querier) error {
		if _, err := q.Exec(ctx, `
			INSERT INTO content_types
				(id, tenant_id, name, label, read_roles, write_roles, own_only_roles, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			ct.ID, ct.TenantID, ct.Name, ct.Label,
			orEmptyRoles(ct.ReadRoles), orEmptyRoles(ct.WriteRoles), orEmptyRoles(ct.OwnOnlyRoles),
			ct.CreatedAt, ct.UpdatedAt,
		); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
				return apperrors.New("CONTENT_TYPE_EXISTS", "content type already exists", 409).
					WithDetails(map[string]any{"name": ct.Name})
			}
			return fmt.Errorf("insert content_type: %w", err)
		}
		// Fields live on content_type_fields (not RLS'd) but share this tx so
		// the type + its fields commit atomically.
		for i := range ct.Fields {
			if err := insertField(ctx, q, &ct.Fields[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

// AddField appends one field to an existing type and moves the parent type's
// updated_at with it. The runtime-dynamic signal: no deploy, no codegen — a row
// insert grows the schema.
//
// It runs inside withTenant, which it did not before: it was the only content
// write reaching the pool directly, which worked solely because
// content_type_fields carries no tenant_id and therefore no RLS policy. Touching
// content_types needs the two statements to be atomic anyway.
//
// The tenant is a parameter for a sharper reason: content_types has FORCE row
// level security, so an UPDATE without app.tenant_id set matches zero rows and
// reports success. Silent, not an error.
func (r *PostgresContentRepository) AddField(ctx context.Context, tenantID string, f *domain.Field) error {
	return r.withTenant(ctx, tenantID, func(q querier) error {
		if err := insertField(ctx, q, f); err != nil {
			return err
		}
		return touchContentType(ctx, q, f.ContentTypeID, f.CreatedAt)
	})
}

// touchContentType keeps content_types.updated_at honest. Before schema mutation
// existed the column was written once at insert and never again, while the DTO
// exposed it — so a type reported a timestamp that stayed at created_at even as
// fields were added. The timestamp comes from the service, matching how
// entries.updated_at is handled; a DB trigger would not work here because the
// mutations that matter most happen on a DIFFERENT table.
func touchContentType(ctx context.Context, q execer, id uuid.UUID, now time.Time) error {
	if _, err := q.Exec(ctx, `UPDATE content_types SET updated_at = $2 WHERE id = $1`, id, now); err != nil {
		return fmt.Errorf("touch content_type: %w", err)
	}
	return nil
}

// execer is the subset of pgx shared by *pgxpool.Pool and pgx.Tx.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// orEmptyRoles converts a nil permission list into an empty one for the wire.
//
// pgx encodes a nil slice as SQL NULL, and read_roles/write_roles are NOT NULL —
// the same trap enum_values documents at buildField. It is fixed HERE rather
// than by making the domain carry []string{}: nil is the canonical empty in this
// model (it is what makes the artifact omit the key and round-trip
// byte-identically), and NULL-versus-empty-array is a pgx encoding detail that
// belongs at the boundary that has it. Domain objects built in tests, or by any
// future path that does not go through buildField, are covered by putting it
// here and would not be by putting it there.
func orEmptyRoles(roles []string) []string {
	if roles == nil {
		return []string{}
	}
	return roles
}

func insertField(ctx context.Context, q execer, f *domain.Field) error {
	if _, err := q.Exec(ctx, `
		INSERT INTO content_type_fields
			(id, content_type_id, key, field_type, label, required, multiple, enum_values, read_roles, write_roles, relation_entity, ordinal, created_at,
			 is_unique, format, pattern, min_value, max_value, ref_component_id, zone_components, description)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
			-- Append: one past the highest position this type currently holds.
			-- Computed in the INSERT rather than by a prior SELECT so it is
			-- decided inside the same statement, under the same transaction, as
			-- the row it numbers.
			COALESCE((SELECT MAX(ordinal) FROM content_type_fields WHERE content_type_id = $2), 0) + 1,
			$12, $13, $14, $15, $16, $17, $18, $19, $20)`,
		f.ID, f.ContentTypeID, f.Key, f.Type, f.Label, f.Required, f.Multiple, f.EnumValues,
		orEmptyRoles(f.ReadRoles), orEmptyRoles(f.WriteRoles), f.RelationEntity, f.CreatedAt,
		f.Unique, f.Format, f.Pattern, f.Min, f.Max, f.ComponentID, f.ZoneComponents, f.Description,
	); err != nil {
		return fieldInsertError(err, f.Key)
	}
	return nil
}

// insertComponentField is insertField for a component's OWN sub-field
// (ADR-020): content_type_id NULL, component_id set, ordinal numbered within
// the component. A sub-field never references a component (no nesting), so
// ref_component_id stays NULL and the CHECK in 000044 agrees.
func insertComponentField(ctx context.Context, q execer, componentID uuid.UUID, f *domain.Field) error {
	if _, err := q.Exec(ctx, `
		INSERT INTO content_type_fields
			(id, component_id, key, field_type, label, required, multiple, enum_values, read_roles, write_roles, relation_entity, ordinal, created_at,
			 is_unique, format, pattern, min_value, max_value, description)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
			COALESCE((SELECT MAX(ordinal) FROM content_type_fields WHERE component_id = $2), 0) + 1,
			$12, $13, $14, $15, $16, $17, $18)`,
		f.ID, componentID, f.Key, f.Type, f.Label, f.Required, f.Multiple, f.EnumValues,
		orEmptyRoles(f.ReadRoles), orEmptyRoles(f.WriteRoles), f.RelationEntity, f.CreatedAt,
		f.Unique, f.Format, f.Pattern, f.Min, f.Max, f.Description,
	); err != nil {
		return fieldInsertError(err, f.Key)
	}
	return nil
}

func fieldInsertError(err error, key string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return apperrors.New("CONTENT_FIELD_EXISTS", "field already defined", 409).
			WithDetails(map[string]any{"field": key})
	}
	return fmt.Errorf("insert content_type_field: %w", err)
}

func (r *PostgresContentRepository) GetContentTypeByName(ctx context.Context, tenantID, name string) (*domain.ContentType, error) {
	var ct domain.ContentType
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		return q.QueryRow(ctx, `
			SELECT `+contentTypeColumns+`
			FROM content_types
			WHERE tenant_id = $1 AND name = $2`, tenantID, name,
		).Scan(&ct.ID, &ct.TenantID, &ct.Name, &ct.Label,
			&ct.ReadRoles, &ct.WriteRoles, &ct.OwnOnlyRoles, &ct.CreatedAt, &ct.UpdatedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperrors.ErrNotFound
		}
		return nil, fmt.Errorf("content_type scan: %w", err)
	}
	fields, err := r.loadFields(ctx, ct.ID)
	if err != nil {
		return nil, err
	}
	ct.Fields = fields
	return &ct, nil
}

// GetContentTypeByID is GetContentTypeByName keyed by id, and exists for the
// one caller that starts from an id: the scheduler worker, which holds a
// schedule row and no request context.
//
// The name-keyed read stays the primary one — every request-driven path names
// its type in the URL — so this is not a replacement. It is here rather than in
// the scheduler package because a type is loaded with its fields, and loadFields
// is repository-internal.
func (r *PostgresContentRepository) GetContentTypeByID(ctx context.Context, tenantID string, id uuid.UUID) (*domain.ContentType, error) {
	var ct domain.ContentType
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		return q.QueryRow(ctx, `
			SELECT `+contentTypeColumns+`
			FROM content_types
			WHERE tenant_id = $1 AND id = $2`, tenantID, id,
		).Scan(&ct.ID, &ct.TenantID, &ct.Name, &ct.Label,
			&ct.ReadRoles, &ct.WriteRoles, &ct.OwnOnlyRoles, &ct.CreatedAt, &ct.UpdatedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperrors.ErrNotFound
		}
		return nil, fmt.Errorf("content_type scan: %w", err)
	}
	fields, err := r.loadFields(ctx, ct.ID)
	if err != nil {
		return nil, err
	}
	ct.Fields = fields
	return &ct, nil
}

func (r *PostgresContentRepository) ListContentTypes(ctx context.Context, tenantID string) ([]*domain.ContentType, error) {
	var out []*domain.ContentType
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT `+contentTypeColumns+`
			FROM content_types
			WHERE tenant_id = $1
			ORDER BY created_at DESC`, tenantID)
		if err != nil {
			return fmt.Errorf("content_types list: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var ct domain.ContentType
			if err := rows.Scan(&ct.ID, &ct.TenantID, &ct.Name, &ct.Label,
				&ct.ReadRoles, &ct.WriteRoles, &ct.OwnOnlyRoles, &ct.CreatedAt, &ct.UpdatedAt); err != nil {
				return fmt.Errorf("content_types scan: %w", err)
			}
			out = append(out, &ct)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(out))
	for i, ct := range out {
		ids[i] = ct.ID
	}
	byType, err := r.loadFieldsByType(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, ct := range out {
		ct.Fields = byType[ct.ID]
	}
	return out, nil
}

func (r *PostgresContentRepository) CountContentTypes(ctx context.Context, tenantID string) (int, error) {
	var n int
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		return q.QueryRow(ctx, `SELECT COUNT(*) FROM content_types WHERE tenant_id = $1`, tenantID).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("content_types count: %w", err)
	}
	return n, nil
}

func (r *PostgresContentRepository) CountEntriesForTenant(ctx context.Context, tenantID string) (int, error) {
	var n int
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		return q.QueryRow(ctx, `SELECT COUNT(*) FROM entries WHERE tenant_id = $1`, tenantID).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("entries count: %w", err)
	}
	return n, nil
}

// loadFields returns one type's fields. It is loadFieldsByType narrowed to a
// single id on purpose: the column list, the scan and the ordering below are
// the kind of thing that has to exist exactly once, and a second copy written
// for the batch path is precisely the drift the comment there warns about.
func (r *PostgresContentRepository) loadFields(ctx context.Context, contentTypeID uuid.UUID) ([]domain.Field, error) {
	byType, err := r.loadFieldsByType(ctx, []uuid.UUID{contentTypeID})
	if err != nil {
		return nil, err
	}
	return byType[contentTypeID], nil
}

// loadFieldsByType loads the fields of every given type in ONE query and groups
// them in Go. ListContentTypes used to call loadFields once per type, so the
// admin console's type list grew linearly with the tenant's content model
// (~145µs per type, 8ms at 50 types — docs/readpath-optimisation-slices.md §1.3).
//
// The ORDER BY is no longer a total order over the whole result once several
// types are in play, and it does not need to be: what has to be deterministic
// is the order WITHIN each type, and restricting a sort by (ordinal, created_at,
// key) to one content_type_id leaves exactly that order intact.
func (r *PostgresContentRepository) loadFieldsByType(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]domain.Field, error) {
	return loadFieldsByTypeWith(ctx, r.pool, ids)
}

// loadFieldsByTypeWith is loadFieldsByType on a caller-chosen connection, so a
// schema verb can read the fields as they stand INSIDE its own transaction.
func loadFieldsByTypeWith(ctx context.Context, q querier, ids []uuid.UUID) (map[uuid.UUID][]domain.Field, error) {
	out := make(map[uuid.UUID][]domain.Field, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	// Keep this column list in lockstep with insertField's. A column written but
	// not read back is the silent half of the pair: the write succeeds, the flag
	// comes back false, and every multi-valued field quietly behaves as a scalar
	// on the very next request. For the permission columns that half is not
	// merely wrong, it is open: an unread read_roles comes back empty, empty
	// means unrestricted, and the field is served to everyone it was just closed
	// to.
	// ORDER BY ordinal, then created_at, then key. The first is the definition
	// order (migration 000025); the second catches any row that predates a path
	// learning to set it (they sit at the DEFAULT of 0); the third is what makes
	// the result a TOTAL order rather than one more tie resolved by the query
	// plan — which is the bug 000025 exists to close, and leaving a second copy
	// of it here would defeat the point.
	rows, err := q.Query(ctx, `
		SELECT f.id, f.content_type_id, f.key, f.field_type, f.label, f.required, f.multiple, f.enum_values,
		       f.read_roles, f.write_roles, f.relation_entity, f.ordinal, f.created_at,
		       f.is_unique, f.format, f.pattern, f.min_value, f.max_value, f.description,
		       f.ref_component_id, COALESCE(c.name, ''), f.zone_components
		FROM content_type_fields f
		LEFT JOIN content_components c ON c.id = f.ref_component_id
		WHERE f.content_type_id = ANY($1::uuid[])
		ORDER BY f.ordinal ASC, f.created_at ASC, f.key ASC`, ids)
	if err != nil {
		return nil, fmt.Errorf("content_type_fields list: %w", err)
	}
	defer rows.Close()

	var refs []uuid.UUID
	for rows.Next() {
		f, err := scanField(rows)
		if err != nil {
			return nil, err
		}
		if f.ComponentID != nil {
			refs = append(refs, *f.ComponentID)
		}
		out[f.ContentTypeID] = append(out[f.ContentTypeID], f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Attach the referenced components' sub-fields (ADR-020) so a loaded
	// ContentType is complete on its own: validation, projection and the
	// artifact all read Field.ComponentFields and never go back to the
	// database for it. One query for every component the batch references.
	if len(refs) > 0 {
		subs, err := loadComponentFieldsWith(ctx, q, refs)
		if err != nil {
			return nil, err
		}
		for id, fields := range out {
			for i := range fields {
				if fields[i].ComponentID != nil {
					fields[i].ComponentFields = subs[*fields[i].ComponentID]
				}
			}
			out[id] = fields
		}
	}
	if err := attachZoneFields(ctx, q, ids, out); err != nil {
		return nil, err
	}
	return out, nil
}

// attachZoneFields fills Field.ZoneFields for every dynamic zone in the batch,
// so a loaded ContentType is complete on its own — the same promise the
// ComponentFields attach above makes, and for the same reason: validation,
// media collection and the artifact must never go back to the database.
//
// A zone stores NAMES, so the lookup goes through content_components by name
// rather than by id. The tenant is derived from the types being loaded rather
// than passed in, because loadFieldsByType runs on the pool with no
// app.tenant_id set: taking it from the rows keeps the two-layer story intact
// (the app scopes explicitly, RLS is the second net) instead of relying on a
// setting that is not there.
func attachZoneFields(ctx context.Context, q querier, typeIDs []uuid.UUID, out map[uuid.UUID][]domain.Field) error {
	var names []string
	seen := map[string]struct{}{}
	for _, fields := range out {
		for _, f := range fields {
			if f.Type != domain.FieldTypeDynamicZone {
				continue
			}
			for _, n := range f.ZoneComponents {
				if _, dup := seen[n]; !dup {
					seen[n] = struct{}{}
					names = append(names, n)
				}
			}
		}
	}
	if len(names) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, `
		SELECT c.id, c.name
		FROM content_components c
		WHERE c.name = ANY($2::text[])
		  AND c.tenant_id IN (SELECT tenant_id FROM content_types WHERE id = ANY($1::uuid[]))`,
		typeIDs, names)
	if err != nil {
		return fmt.Errorf("zone components list: %w", err)
	}
	byName := map[string]uuid.UUID{}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return fmt.Errorf("zone components scan: %w", err)
		}
		byName[name] = id
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	subs, err := loadComponentFieldsWith(ctx, q, ids)
	if err != nil {
		return err
	}
	for id, fields := range out {
		for i := range fields {
			f := &fields[i]
			if f.Type != domain.FieldTypeDynamicZone {
				continue
			}
			f.ZoneFields = make(map[string][]domain.Field, len(f.ZoneComponents))
			for _, n := range f.ZoneComponents {
				// A name with no component is left OUT rather than mapped to an
				// empty list. Both fail closed at validation, but absence is the
				// honest report: the allowed-list has no FK, so this is the one
				// place a dangling name could show up, and a silent empty
				// sub-field set would read as "a component with no fields".
				if cid, ok := byName[n]; ok {
					f.ZoneFields[n] = subs[cid]
				}
			}
		}
		out[id] = fields
	}
	return nil
}

// fieldScanner is the subset of pgx.Rows scanField needs.
type fieldScanner interface {
	Scan(dest ...any) error
}

// scanField reads one content_type_fields row in the column order
// loadFieldsByType and loadComponentFields SELECT. content_type_id is
// nullable since 000044 (sub-field rows leave it NULL), so it lands in a
// NullUUID and the zero value stands in for "none".
func scanField(row fieldScanner) (domain.Field, error) {
	var f domain.Field
	var ctID uuid.NullUUID
	if err := row.Scan(&f.ID, &ctID, &f.Key, &f.Type, &f.Label, &f.Required, &f.Multiple, &f.EnumValues,
		&f.ReadRoles, &f.WriteRoles, &f.RelationEntity, &f.Ordinal, &f.CreatedAt,
		&f.Unique, &f.Format, &f.Pattern, &f.Min, &f.Max, &f.Description,
		&f.ComponentID, &f.ComponentName, &f.ZoneComponents); err != nil {
		return domain.Field{}, fmt.Errorf("content_type_fields scan: %w", err)
	}
	if ctID.Valid {
		f.ContentTypeID = ctID.UUID
	}
	return f, nil
}

// --- schema mutation --------------------------------------------------------

// bothCopies renders a predicate against payload and published_payload, OR'd.
// Every guard has to ask about both: a value edited out of the working copy but
// still live in the snapshot is still a value the schema must accommodate, or
// the published document stops validating against its own type.
//
// The snapshot half is qualified by status, and that qualifier is what keeps
// this helper meaning the same thing before and after ADR-014 §5.1. While a
// retract nulled the snapshot, "has a snapshot" and "is live" were the same
// condition and the qualifier was redundant — jsonb_exists(NULL, …) is NULL, so
// an unpublished row could never match this half anyway. §5.1 splits those two
// conditions apart, and without the qualifier the guards would silently widen
// to cover RETRACTED snapshots: a schema change could then be refused by
// content that is not published and has not been for months.
//
// Refusing it would also contradict §5.1 itself, which says a retained snapshot
// that no longer validates is a problem "at restore time" — pruneUndefined and
// validateAndNormalize already sit on that path. Blocking the mutation would
// move that cost to a person who cannot see why.
func bothCopies(predicate string) string {
	return "((" + strings.ReplaceAll(predicate, "{c}", "payload") + ") OR (status = '" +
		domain.StatusPublished + "' AND (" +
		strings.ReplaceAll(predicate, "{c}", "published_payload") + ")))"
}

func (r *PostgresContentRepository) countEntries(ctx context.Context, tenantID string, contentTypeID uuid.UUID, pred string, args ...any) (int, error) {
	var n int
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		all := append([]any{tenantID, contentTypeID}, args...)
		return q.QueryRow(ctx,
			`SELECT COUNT(*) FROM entries WHERE tenant_id = $1 AND content_type_id = $2 AND `+pred, all...,
		).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("count entries: %w", err)
	}
	return n, nil
}

func (r *PostgresContentRepository) CountEntriesForType(ctx context.Context, tenantID string, contentTypeID uuid.UUID) (int, error) {
	return r.countEntries(ctx, tenantID, contentTypeID, "TRUE")
}

func (r *PostgresContentRepository) CountEntriesWithField(ctx context.Context, tenantID string, contentTypeID uuid.UUID, key string) (int, error) {
	return r.countEntries(ctx, tenantID, contentTypeID, bothCopies("jsonb_exists({c}, $3)"), key)
}

func (r *PostgresContentRepository) CountEntriesMissingField(ctx context.Context, tenantID string, contentTypeID uuid.UUID, key string) (int, error) {
	// "Missing" must mean what validatePayload means: absent OR explicitly null.
	// A bare jsonb_exists would call `{"tags": null}` present, disagree with the
	// validator, and let `required` be tightened over rows that then fail on
	// their next write. Only the working copy is consulted — tightening required
	// is about what an editor must now supply.
	return r.countEntries(ctx, tenantID, contentTypeID,
		"NOT (jsonb_exists(payload, $3) AND jsonb_typeof(payload -> $3) <> 'null')", key)
}

func (r *PostgresContentRepository) CountEntriesWithValuesOutside(ctx context.Context, tenantID string, contentTypeID uuid.UUID, f domain.Field, allowed []string) (int, error) {
	pred := `(jsonb_exists({c}, $3) AND jsonb_typeof({c} -> $3) NOT IN ('null') AND NOT ({c} ->> $3 = ANY($4::text[])))`
	if f.Multiple {
		// Element-wise. jsonb_array_elements_text over a non-array would error,
		// so the shape is guarded first — a field that only just became multiple
		// could still hold scalars written under the old definition.
		pred = `(jsonb_typeof({c} -> $3) = 'array' AND EXISTS (
			SELECT 1 FROM jsonb_array_elements_text({c} -> $3) AS v
			WHERE NOT (v = ANY($4::text[]))))`
	}
	return r.countEntries(ctx, tenantID, contentTypeID, bothCopies(pred), f.Key, allowed)
}

func (r *PostgresContentRepository) ListRelationReferrers(ctx context.Context, tenantID, typeName string) ([]RelationRef, error) {
	var out []RelationRef
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		// Component sub-fields (ADR-020) can be relations too; they refer
		// through the owning component, which is what the second half of the
		// UNION reports. Both halves join their owner for the tenant scope —
		// content_type_fields has none of its own (000014).
		rows, err := q.Query(ctx, `
			SELECT ct.name, '' AS component, f.key
			FROM content_type_fields f
			JOIN content_types ct ON ct.id = f.content_type_id
			WHERE ct.tenant_id = $1 AND f.field_type = $2 AND f.relation_entity = $3
			UNION ALL
			SELECT '' AS name, c.name AS component, f.key
			FROM content_type_fields f
			JOIN content_components c ON c.id = f.component_id
			WHERE c.tenant_id = $1 AND f.field_type = $2 AND f.relation_entity = $3
			ORDER BY 1, 2, 3`,
			tenantID, domain.FieldTypeRelation, typeName)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ref RelationRef
			if err := rows.Scan(&ref.TypeName, &ref.Component, &ref.FieldKey); err != nil {
				return err
			}
			out = append(out, ref)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list relation referrers: %w", err)
	}
	return out, nil
}

func (r *PostgresContentRepository) UpdateFieldDefinition(ctx context.Context, tenantID string, ct *domain.ContentType, f domain.Field, now time.Time) error {
	return r.withTenant(ctx, tenantID, func(q querier) error {
		// The stored unique flag is read under a row lock so that "was it unique
		// before this write" is decided in the same transaction that changes it.
		// Two concurrent PATCHes flipping the flag would otherwise both see
		// false and both rebuild — harmless — or one see the other's half-done
		// rebuild and skip its own, which is not.
		var wasUnique bool
		err := q.QueryRow(ctx, `
			SELECT is_unique FROM content_type_fields
			WHERE content_type_id = $1 AND key = $2 FOR UPDATE`, ct.ID, f.Key).Scan(&wasUnique)
		if errors.Is(err, pgx.ErrNoRows) {
			return apperrors.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("update field: %w", err)
		}
		if _, err := q.Exec(ctx, `
			UPDATE content_type_fields
			SET label = $3, required = $4, enum_values = $5, read_roles = $6, write_roles = $7,
			    is_unique = $8, format = $9, pattern = $10, min_value = $11, max_value = $12,
			    zone_components = $13, description = $14
			WHERE content_type_id = $1 AND key = $2`,
			ct.ID, f.Key, f.Label, f.Required, f.EnumValues,
			orEmptyRoles(f.ReadRoles), orEmptyRoles(f.WriteRoles),
			f.Unique, f.Format, f.Pattern, f.Min, f.Max, f.ZoneComponents, f.Description); err != nil {
			return fmt.Errorf("update field: %w", err)
		}
		switch {
		case f.Unique && !wasUnique:
			// Switching uniqueness ON reserves every value the type already
			// holds, and the primary key is the guard: a duplicate the service's
			// pre-check did not see (a write that landed between its count and
			// this lock) fails HERE, not on some editor's next save.
			if err := rebuildFieldUniqueValues(ctx, q, tenantID, f); err != nil {
				return err
			}
		case !f.Unique && wasUnique:
			if _, err := q.Exec(ctx, `
				DELETE FROM entry_unique_values WHERE tenant_id = $1 AND field_id = $2`,
				tenantID, f.ID); err != nil {
				return fmt.Errorf("release unique values: %w", err)
			}
		}
		return touchContentType(ctx, q, ct.ID, now)
	})
}

// reservationsSQL renders the set of (tenant_id, field_id, entry_id, locale,
// value) rows that entry_unique_values must hold for the entries and fields
// `scope` selects (a WHERE fragment over aliases e = entries and f =
// content_type_fields). It is the ONE definition of "which values an entry
// reserves": the working copy's value and, while the entry is published, the
// snapshot's; never the empty string, never null. Every writer below derives
// the ledger from it, so the ledger cannot mean two things.
//
// `->>` renders the value as text — the same rendering the eq filter compares
// with — so a number written as 10 and one written as 10.0 collide exactly
// when jsonb says they are the same number.
func reservationsSQL(scope string) string {
	return `
		SELECT e.tenant_id, f.id AS field_id, e.id AS entry_id, e.locale, v.value
		FROM entries e
		JOIN content_type_fields f ON f.content_type_id = e.content_type_id
		CROSS JOIN LATERAL (
			SELECT DISTINCT x.value FROM (VALUES
				(e.payload ->> f.key),
				(CASE WHEN e.status = '` + domain.StatusPublished + `' THEN e.published_payload ->> f.key END)
			) AS x(value)
			WHERE x.value IS NOT NULL AND x.value <> ''
		) v
		WHERE ` + scope
}

// refreshEntryUniqueValues rewrites one entry's rows in entry_unique_values
// from the entry row as it stands in q's transaction. Called after EVERY write
// that can change what the entry holds — create, update, publish, unpublish —
// and nowhere else, so the ledger is derived state with one derivation.
//
// The pre-check exists for the error message, not for correctness: it names the
// field, the value and the entry that already holds it, which a primary-key
// violation cannot. Correctness is the INSERT's — a value that two transactions
// race for is reserved by exactly one of them, and the other gets the same code
// without the details.
func refreshEntryUniqueValues(ctx context.Context, q querier, tenantID string, entryID uuid.UUID) error {
	scope := `e.tenant_id = $1 AND e.id = $2 AND f.is_unique`
	var key, locale, value string
	var holder uuid.UUID
	err := q.QueryRow(ctx, `
		WITH r AS (`+reservationsSQL(scope)+`)
		SELECT f.key, r.locale, r.value, u.entry_id
		FROM r
		JOIN content_type_fields f ON f.id = r.field_id
		JOIN entry_unique_values u
		  ON u.field_id = r.field_id AND u.locale = r.locale AND u.value = r.value AND u.entry_id <> r.entry_id
		LIMIT 1`, tenantID, entryID).Scan(&key, &locale, &value, &holder)
	switch {
	case err == nil:
		return errValueTaken(key, locale, value, &holder)
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("unique pre-check: %w", err)
	}
	if _, err := q.Exec(ctx, `
		DELETE FROM entry_unique_values WHERE tenant_id = $1 AND entry_id = $2`, tenantID, entryID); err != nil {
		return fmt.Errorf("release unique values: %w", err)
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO entry_unique_values (tenant_id, field_id, entry_id, locale, value)
		`+reservationsSQL(scope), tenantID, entryID); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return errValueTaken("", "", "", nil)
		}
		return fmt.Errorf("reserve unique values: %w", err)
	}
	return nil
}

// rebuildFieldUniqueValues reserves every value of one field across the whole
// type — the moment `unique` is switched on.
func rebuildFieldUniqueValues(ctx context.Context, q querier, tenantID string, f domain.Field) error {
	if _, err := q.Exec(ctx, `
		DELETE FROM entry_unique_values WHERE tenant_id = $1 AND field_id = $2`, tenantID, f.ID); err != nil {
		return fmt.Errorf("release unique values: %w", err)
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO entry_unique_values (tenant_id, field_id, entry_id, locale, value)
		`+reservationsSQL(`e.tenant_id = $1 AND f.id = $2`), tenantID, f.ID); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return apperrors.New("CONTENT_FIELD_UNIQUE_DUPLICATES", "entries already share a value in this field", 409).
				WithDetails(map[string]any{"field": f.Key})
		}
		return fmt.Errorf("reserve unique values: %w", err)
	}
	return nil
}

// errValueTaken is the 409 an entry write gets when a unique field's value is
// already held by another entry. The details are omitted when the collision was
// found by the primary key rather than the pre-check (see
// refreshEntryUniqueValues) — the code is the contract, the details are help.
func errValueTaken(key, locale, value string, holder *uuid.UUID) error {
	e := apperrors.New("CONTENT_FIELD_VALUE_TAKEN", "value is already used by another entry", 409)
	if key == "" {
		return e
	}
	return e.WithDetails(map[string]any{"field": key, "locale": locale, "value": value, "entry": holder.String()})
}

func (r *PostgresContentRepository) ListDuplicateFieldValues(ctx context.Context, tenantID string, contentTypeID uuid.UUID, f domain.Field) ([]DuplicateValue, error) {
	var out []DuplicateValue
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		rows, err := q.Query(ctx, `
			WITH r AS (`+reservationsSQL(`e.tenant_id = $1 AND e.content_type_id = $2 AND f.id = $3`)+`)
			SELECT locale, value, COUNT(*) FROM r
			GROUP BY locale, value HAVING COUNT(*) > 1
			ORDER BY locale, value LIMIT 50`, tenantID, contentTypeID, f.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d DuplicateValue
			if err := rows.Scan(&d.Locale, &d.Value, &d.Entries); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list duplicate values: %w", err)
	}
	return out, nil
}

func (r *PostgresContentRepository) ScanFieldValues(ctx context.Context, tenantID string, contentTypeID uuid.UUID, key string, fn func(entryID uuid.UUID, working, live any) error) error {
	return r.withTenant(ctx, tenantID, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT id, payload -> $3,
			       CASE WHEN status = $4 THEN published_payload -> $3 END
			FROM entries
			WHERE tenant_id = $1 AND content_type_id = $2
			ORDER BY created_at, id`, tenantID, contentTypeID, key, domain.StatusPublished)
		if err != nil {
			return fmt.Errorf("scan field values: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			var w, l json.RawMessage
			if err := rows.Scan(&id, &w, &l); err != nil {
				return fmt.Errorf("scan field values: %w", err)
			}
			wv, err := decodeScalar(w)
			if err != nil {
				return err
			}
			lv, err := decodeScalar(l)
			if err != nil {
				return err
			}
			if err := fn(id, wv, lv); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}

// decodeScalar turns a jsonb column (nil for SQL NULL) into the Go value
// validatePayload would see for it: string, float64, bool, []any, map — or nil
// for absent and for JSON null, which the validator treats alike.
func decodeScalar(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("decode field value: %w", err)
	}
	return v, nil
}

// DeleteField removes the definition and the stored data in one transaction.
//
// Stripping rather than leaving orphan keys is not the aggressive option, it is
// the only correct one: validatePayload rejects undefined keys across the whole
// document, so an orphan makes every affected entry un-PATCHable — and it stays
// in published_payload, so the deleted field keeps being served.
//
// Yes, this changes what delivery serves without a publish. That is the point of
// the operation, not a leak: publish-gating exists to keep an editor's
// in-progress content private, not to keep a deleted field alive. What ADR-004
// and ADR-006 require is that it be DELIBERATE, which the service's force flag
// supplies at the right layer.
func (r *PostgresContentRepository) DeleteField(ctx context.Context, tenantID string, ct *domain.ContentType, f domain.Field, actor domain.WriteActor, now time.Time) error {
	return r.withTenant(ctx, tenantID, func(q querier) error {
		// The version bumps are guarded per copy. published_version is what a
		// delivery consumer watches for change (ADR-006 Am.1a); moving the
		// snapshot while it stands still is exactly the lie that amendment
		// closed. Rows that never held the key keep their versions, so an admin
		// with an open editor is not handed a spurious conflict for nothing.
		//
		// THE `working` CTE IS THE COLLISION GUARD, not an optimisation. The
		// statement touches every row holding the key in EITHER copy, but only
		// the ones holding it in the WORKING copy get a version bump — and
		// entry_revisions is keyed (entry_id, version). Writing a revision for
		// the other rows would re-insert a version that already has one, and
		// recordEntryRevision's reasoning applies here too: there is no ON
		// CONFLICT DO NOTHING to swallow it, so it would be a hard failure of a
		// bulk schema change. Restricting the insert to `working` is also the
		// semantically right answer rather than a way to dodge the error — a row
		// whose working copy did not change has no new version of its content to
		// record.
		//
		// The three provenance columns are guarded by the same condition, for
		// the same reason stated as a fact about content rather than about keys:
		// updated_by means "who last wrote the working copy", so stamping it on
		// a row whose working copy was untouched would be a false answer about a
		// write that did not happen. All CTEs read one snapshot, so `working`
		// sees the pre-update rows even though it is evaluated alongside the
		// UPDATE.
		if _, err := q.Exec(ctx, `
			WITH working AS (
				SELECT id FROM entries
				WHERE tenant_id = $1 AND content_type_id = $2 AND jsonb_exists(payload, $3)
			),
			stripped AS (
				UPDATE entries
				SET payload = payload - $3,
				    published_payload = published_payload - $3,
				    -- The stripped key's own text (if it held any) must not go on
				    -- matching a search query forever, so search_text is refreshed
				    -- here too, via the SQL approximation (migration 000047) -- the same
				    -- one the initial backfill uses, since this bulk statement has no
				    -- per-row Go loop to call domain.ExtractSearchText from. It is
				    -- CONDITIONED THE SAME WAY payload is: unlike RenameField (a key
				    -- rename touches no value) this operation removes a value, so
				    -- recomputing it is not optional the way it was optional there.
				    search_text = CASE WHEN jsonb_exists(payload, $3)
				        THEN content_entry_search_text_approx(payload - $3) ELSE search_text END,
				    published_search_text = CASE WHEN jsonb_exists(published_payload, $3)
				        THEN content_entry_search_text_approx(published_payload - $3) ELSE published_search_text END,
				    version = CASE WHEN jsonb_exists(payload, $3) THEN version + 1 ELSE version END,
				    published_version = CASE WHEN jsonb_exists(published_payload, $3) THEN published_version + 1 ELSE published_version END,
				    updated_at = $4,
				    updated_by = CASE WHEN jsonb_exists(payload, $3) THEN $5::uuid ELSE updated_by END,
				    updated_by_kind = CASE WHEN jsonb_exists(payload, $3) THEN $6::text ELSE updated_by_kind END,
				    updated_by_agent = CASE WHEN jsonb_exists(payload, $3) THEN $7::text ELSE updated_by_agent END
				WHERE tenant_id = $1 AND content_type_id = $2
				  AND `+bothCopies("jsonb_exists({c}, $3)")+`
				RETURNING id, version, tenant_id, payload,
				          updated_by_kind, updated_by, updated_by_agent, updated_at
			)
			INSERT INTO entry_revisions
				(entry_id, version, tenant_id, payload, author_kind, author_user_id, author_agent_id, created_at)
			SELECT s.id, s.version, s.tenant_id, s.payload,
			       s.updated_by_kind, s.updated_by, s.updated_by_agent, s.updated_at
			FROM stripped s JOIN working w ON w.id = s.id`,
			tenantID, ct.ID, f.Key, now, actor.UserID, actor.Kind, actor.AgentID,
		); err != nil {
			return fmt.Errorf("strip field from entries: %w", err)
		}
		if linksMedia(f) {
			if err := relinkEntryMedia(ctx, q, tenantID, ct.ID, withoutKey(ct.Fields, f.Key)); err != nil {
				return err
			}
		}
		if linksRelations(f) {
			if err := relinkEntryRelations(ctx, q, tenantID, ct.ID, withoutKey(ct.Fields, f.Key)); err != nil {
				return err
			}
		}
		tag, err := q.Exec(ctx,
			`DELETE FROM content_type_fields WHERE content_type_id = $1 AND key = $2`, ct.ID, f.Key)
		if err != nil {
			return fmt.Errorf("delete field: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return apperrors.ErrNotFound
		}
		return touchContentType(ctx, q, ct.ID, now)
	})
}

// linksMedia reports whether values of f are linked through entry_media by
// the write path (mediaRefsIn): file ids, richtext image blocks, and a
// component whose sub-fields do either.
func linksMedia(f domain.Field) bool {
	switch f.Type {
	case domain.FieldTypeFile, domain.FieldTypeRichText:
		return true
	case domain.FieldTypeComponent:
		for _, sf := range f.ComponentFields {
			if linksMedia(sf) {
				return true
			}
		}
	case domain.FieldTypeDynamicZone:
		// ANY allowed component with a media-bearing sub-field makes the zone
		// media-bearing. Under-reporting here is the dangerous direction: the
		// relink pass would be skipped, links to a deleted field's assets would
		// survive, and AssetIsPublished would keep the signed-URL gate open on
		// bytes nothing references (ADR-005).
		for _, subs := range f.ZoneFields {
			for _, sf := range subs {
				if linksMedia(sf) {
					return true
				}
			}
		}
	}
	return false
}

// withoutKey is fields minus the one named — the survivor set a delete leaves.
func withoutKey(fields []domain.Field, key string) []domain.Field {
	out := make([]domain.Field, 0, len(fields))
	for _, f := range fields {
		if f.Key != key {
			out = append(out, f)
		}
	}
	return out
}

// mediaPaths is the jsonpath of every place a media asset id can sit in a
// payload shaped by fields — the SAME places mediaRefsIn walks when an entry
// is written, or the rebuild below would prune links the write path made.
// Component values are reached with [*], which in lax mode wraps a single
// object as well as iterating a list, so one path serves both cardinalities
// (ADR-020: the sub-fields are one level down and never deeper).
func mediaPaths(fields []domain.Field) []string {
	out := []string{}
	for _, f := range fields {
		switch f.Type {
		case domain.FieldTypeFile:
			out = append(out, fmt.Sprintf(`$."%s"`, f.Key))
		case domain.FieldTypeRichText:
			out = append(out, fmt.Sprintf(`$."%s"[*] ? (@."type" == "%s")."media_id"`, f.Key, domain.RichTextBlockImage))
		case domain.FieldTypeComponent:
			for _, p := range mediaPaths(f.ComponentFields) {
				out = append(out, fmt.Sprintf(`$."%s"[*]%s`, f.Key, strings.TrimPrefix(p, "$")))
			}
		case domain.FieldTypeDynamicZone:
			// One path per (allowed component × media place inside it), each
			// filtered to the items that ARE that component. The filter is what
			// keeps two components with the same sub-field key apart: without
			// it, deleting `hero.image` would leave `cta.image`'s links looking
			// unreferenced and the relink would delete them.
			//
			// Sorted by component name so the path list is deterministic —
			// ZoneFields is a map, and an unordered $3::text[] would make the
			// same schema produce a different statement on every call, which is
			// the kind of thing that turns a reproducible test into a flaky one.
			for _, name := range sortedKeys(f.ZoneFields) {
				for _, p := range mediaPaths(f.ZoneFields[name]) {
					out = append(out, fmt.Sprintf(`$."%s"[*] ? (@."%s" == "%s")%s`,
						f.Key, domain.ZoneDiscriminator, name, strings.TrimPrefix(p, "$")))
				}
			}
		}
	}
	return out
}

// sortedKeys returns a map's keys in ascending order, so anything built out of
// ZoneFields comes out the same way twice.
func sortedKeys(m map[string][]domain.Field) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// relinkEntryMedia rebuilds both link tables from the media places that
// SURVIVE, rather than trying to name the asset that just left. entry_media is
// only ever rewritten by an entry write, so without this a deleted file field
// leaves its links behind, AssetIsPublished keeps answering true, and the
// signed-URL gate stays open on bytes nothing references any more (ADR-005's
// invariant). survivors is the type's field list AFTER the delete, with
// component sub-fields inlined; the top-level verb and the component
// sub-field verb both come through here so there is exactly one notion of
// "where an asset id lives".
func relinkEntryMedia(ctx context.Context, q execer, tenantID string, typeID uuid.UUID, survivors []domain.Field) error {
	paths := mediaPaths(survivors)
	for _, spec := range []struct{ table, column string }{
		{"entry_media", "payload"},
		{"entry_media_published", "published_payload"},
	} {
		// NOT EXISTS, never NOT IN: published_payload is NULL for a draft, and
		// `x NOT IN (…NULL…)` evaluates to NULL, which would silently delete
		// nothing at all. jsonb_path_query runs silent so a value of the wrong
		// shape yields nothing instead of failing the whole delete.
		if _, err := q.Exec(ctx, fmt.Sprintf(`
			DELETE FROM %s em
			USING entries e
			WHERE em.entry_id = e.id
			  AND em.tenant_id = $1
			  AND e.content_type_id = $2
			  AND NOT EXISTS (
			        SELECT 1
			        FROM unnest($3::text[]) AS p,
			             jsonb_path_query(e.%s, p::jsonpath, '{}'::jsonb, true) AS v
			        WHERE v #>> '{}' = em.asset_id::text)`, spec.table, spec.column),
			tenantID, typeID, paths,
		); err != nil {
			return fmt.Errorf("relink %s: %w", spec.table, err)
		}
	}
	return nil
}

// linksRelations reports whether f is itself a relation field, or (through a
// component or dynamic zone) contains one — linksMedia's twin for
// entry_relations.
func linksRelations(f domain.Field) bool {
	switch f.Type {
	case domain.FieldTypeRelation:
		return true
	case domain.FieldTypeComponent:
		for _, sf := range f.ComponentFields {
			if linksRelations(sf) {
				return true
			}
		}
	case domain.FieldTypeDynamicZone:
		for _, subs := range f.ZoneFields {
			for _, sf := range subs {
				if linksRelations(sf) {
					return true
				}
			}
		}
	}
	return false
}

// relationFieldPaths lists the field_path values a survivor field list can
// still hold relation values at, dot-joined the same way prefixed() names
// them at request time ("key" top-level, "parent.key" one level into a
// component or zone). Unlike mediaPaths, this need not reconstruct a
// jsonpath: entry_relations rows already carry field_path (000050's PK
// departure from entry_media, see that migration's header), so a field
// delete only has to know WHICH field_paths still exist, not re-derive the
// values stored at each one — the surviving VALUES are untouched by deleting
// a different field.
func relationFieldPaths(fields []domain.Field) []string {
	// Initialized, not nil: pgx binds a nil []string as SQL NULL rather than
	// an empty array, and `field_path = ANY(NULL)` is NULL — which would make
	// relinkEntryRelations's NOT(...) delete NOTHING when the deleted field
	// was the type's only relation field, the exact opposite of its job.
	// mediaPaths guards the identical footgun the same way.
	out := []string{}
	for _, f := range fields {
		switch f.Type {
		case domain.FieldTypeRelation:
			out = append(out, f.Key)
		case domain.FieldTypeComponent:
			for _, k := range relationFieldPaths(f.ComponentFields) {
				out = append(out, f.Key+"."+k)
			}
		case domain.FieldTypeDynamicZone:
			for _, name := range sortedKeys(f.ZoneFields) {
				for _, k := range relationFieldPaths(f.ZoneFields[name]) {
					out = append(out, f.Key+"."+k)
				}
			}
		}
	}
	return out
}

// relinkEntryRelations drops entry_relations / entry_relations_published rows
// whose field_path no longer exists in survivors — the field delete's
// cleanup, mirroring relinkEntryMedia's purpose without needing its
// jsonpath-vs-stored-value comparison (see relationFieldPaths). survivors is
// the type's field list AFTER the delete, exactly as relinkEntryMedia takes
// it, so deleting a component or zone field drops every one of its sub-field
// paths in the same pass as the top-level one.
func relinkEntryRelations(ctx context.Context, q execer, tenantID string, typeID uuid.UUID, survivors []domain.Field) error {
	keep := relationFieldPaths(survivors)
	for _, table := range []string{"entry_relations", "entry_relations_published"} {
		if _, err := q.Exec(ctx, fmt.Sprintf(`
			DELETE FROM %s er
			USING entries e
			WHERE er.entry_id = e.id
			  AND er.tenant_id = $1
			  AND e.content_type_id = $2
			  AND NOT (er.field_path = ANY($3::text[]))`, table),
			tenantID, typeID, keep,
		); err != nil {
			return fmt.Errorf("relink %s: %w", table, err)
		}
	}
	return nil
}

func (r *PostgresContentRepository) RenameField(ctx context.Context, tenantID string, ct *domain.ContentType, oldKey, newKey string, actor domain.WriteActor, now time.Time) error {
	return r.withTenant(ctx, tenantID, func(q querier) error {
		// The CASE guards are load-bearing, not defensive style:
		// jsonb_build_object($new, payload -> $old) on a row that lacks the key
		// yields {"new": null} — a silent document change on entries the rename
		// should not have touched at all.
		//
		// No search_text / published_search_text touch: this moves a JSON key,
		// never a value, so the plain text ExtractSearchText would find is
		// unchanged.
		//
		// The ::text on $4 is equally load-bearing. jsonb_build_object is
		// VARIADIC "any", so it gives the planner nothing to infer $4 from, and
		// $4 appears nowhere else in the statement — Postgres rejects the whole
		// PARSE with 42P18 "could not determine data type of parameter $4" before
		// a single row is considered. $3 needs no cast because jsonb_exists(jsonb,
		// text) pins it, which is why the otherwise-identical DeleteField statement
		// does not have this problem.
		if _, err := q.Exec(ctx, `
			WITH working AS (
				SELECT id FROM entries
				WHERE tenant_id = $1 AND content_type_id = $2 AND jsonb_exists(payload, $3)
			),
			renamed AS (
				UPDATE entries
				SET payload = CASE WHEN jsonb_exists(payload, $3)
				                   THEN (payload - $3) || jsonb_build_object($4::text, payload -> $3)
				                   ELSE payload END,
				    published_payload = CASE WHEN jsonb_exists(published_payload, $3)
				                   THEN (published_payload - $3) || jsonb_build_object($4::text, published_payload -> $3)
				                   ELSE published_payload END,
				    version = CASE WHEN jsonb_exists(payload, $3) THEN version + 1 ELSE version END,
				    published_version = CASE WHEN jsonb_exists(published_payload, $3) THEN published_version + 1 ELSE published_version END,
				    updated_at = $5,
				    updated_by = CASE WHEN jsonb_exists(payload, $3) THEN $6::uuid ELSE updated_by END,
				    updated_by_kind = CASE WHEN jsonb_exists(payload, $3) THEN $7::text ELSE updated_by_kind END,
				    updated_by_agent = CASE WHEN jsonb_exists(payload, $3) THEN $8::text ELSE updated_by_agent END
				WHERE tenant_id = $1 AND content_type_id = $2
				  AND `+bothCopies("jsonb_exists({c}, $3)")+`
				RETURNING id, version, tenant_id, payload,
				          updated_by_kind, updated_by, updated_by_agent, updated_at
			)
			INSERT INTO entry_revisions
				(entry_id, version, tenant_id, payload, author_kind, author_user_id, author_agent_id, created_at)
			SELECT rn.id, rn.version, rn.tenant_id, rn.payload,
			       rn.updated_by_kind, rn.updated_by, rn.updated_by_agent, rn.updated_at
			FROM renamed rn JOIN working w ON w.id = rn.id`,
			tenantID, ct.ID, oldKey, newKey, now, actor.UserID, actor.Kind, actor.AgentID,
		); err != nil {
			return fmt.Errorf("rename field in entries: %w", err)
		}
		tag, err := q.Exec(ctx,
			`UPDATE content_type_fields SET key = $3 WHERE content_type_id = $1 AND key = $2`,
			ct.ID, oldKey, newKey)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
				return apperrors.New("CONTENT_FIELD_EXISTS", "field already defined", 409).
					WithDetails(map[string]any{"field": newKey})
			}
			return fmt.Errorf("rename field: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return apperrors.ErrNotFound
		}
		return touchContentType(ctx, q, ct.ID, now)
	})
}

func (r *PostgresContentRepository) UpdateContentTypeDefinition(ctx context.Context, tenantID string, ct *domain.ContentType, now time.Time) error {
	return r.withTenant(ctx, tenantID, func(q querier) error {
		tag, err := q.Exec(ctx, `
			UPDATE content_types
			SET label = $3, read_roles = $4, write_roles = $5, own_only_roles = $6, updated_at = $7
			WHERE tenant_id = $1 AND id = $2`,
			tenantID, ct.ID, ct.Label,
			orEmptyRoles(ct.ReadRoles), orEmptyRoles(ct.WriteRoles), orEmptyRoles(ct.OwnOnlyRoles), now)
		if err != nil {
			return fmt.Errorf("update content type definition: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return apperrors.ErrNotFound
		}
		return nil
	})
}

// CountEntriesWithoutAuthor counts rows the confinement predicate can never
// match. It asks about created_by alone — not about the published snapshot the
// other guards check both copies of — because authorship is a property of the
// ROW, and there is only ever one of it.
func (r *PostgresContentRepository) CountEntriesWithoutAuthor(ctx context.Context, tenantID string, contentTypeID uuid.UUID) (int, error) {
	var n int
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		return q.QueryRow(ctx, `
			SELECT COUNT(*) FROM entries
			WHERE tenant_id = $1 AND content_type_id = $2 AND created_by IS NULL`,
			tenantID, contentTypeID).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("entries without author count: %w", err)
	}
	return n, nil
}

func (r *PostgresContentRepository) RenameContentType(ctx context.Context, tenantID string, id uuid.UUID, oldName, newName string, now time.Time) error {
	return r.withTenant(ctx, tenantID, func(q querier) error {
		tag, err := q.Exec(ctx,
			`UPDATE content_types SET name = $3, updated_at = $4 WHERE tenant_id = $1 AND id = $2`,
			tenantID, id, newName, now)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
				return apperrors.New("CONTENT_TYPE_EXISTS", "content type already exists", 409).
					WithDetails(map[string]any{"name": newName})
			}
			return fmt.Errorf("rename content type: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return apperrors.ErrNotFound
		}
		// relation_entity stores the type NAME, resolved per write, so referrers
		// break the moment the name moves.
		//
		// The join through content_types is MANDATORY, not stylistic.
		// content_type_fields has no tenant_id and no RLS policy — deliberately,
		// and documented in migration 000014 — so the obvious-looking
		// `UPDATE content_type_fields SET relation_entity = $new WHERE relation_entity = $old`
		// would rewrite every OTHER tenant's fields that happen to use the same
		// type name. That is cross-tenant data corruption arriving through the
		// exact gap 000014 chose to accept. The join also brings RLS to bear on
		// the read side for free.
		if _, err := q.Exec(ctx, `
			UPDATE content_type_fields f
			SET relation_entity = $3
			FROM content_types ct
			WHERE f.content_type_id = ct.id
			  AND ct.tenant_id = $1
			  AND f.field_type = $4
			  AND f.relation_entity = $2`,
			tenantID, oldName, newName, domain.FieldTypeRelation,
		); err != nil {
			return fmt.Errorf("cascade relation_entity: %w", err)
		}
		// Component sub-fields (ADR-020) may be relations to this type as
		// well; they are scoped through their owning component, same rule.
		if _, err := q.Exec(ctx, `
			UPDATE content_type_fields f
			SET relation_entity = $3
			FROM content_components c
			WHERE f.component_id = c.id
			  AND c.tenant_id = $1
			  AND f.field_type = $4
			  AND f.relation_entity = $2`,
			tenantID, oldName, newName, domain.FieldTypeRelation,
		); err != nil {
			return fmt.Errorf("cascade component relation_entity: %w", err)
		}
		// The referring types' DTOs changed too, so their timestamps move with
		// them. Same tenant join, same reason.
		if _, err := q.Exec(ctx, `
			UPDATE content_types ct SET updated_at = $3
			WHERE ct.tenant_id = $1
			  AND EXISTS (SELECT 1 FROM content_type_fields f
			              WHERE f.content_type_id = ct.id AND f.relation_entity = $2)`,
			tenantID, newName, now,
		); err != nil {
			return fmt.Errorf("touch referring types: %w", err)
		}
		return nil
	})
}

func (r *PostgresContentRepository) DeleteContentType(ctx context.Context, tenantID string, id uuid.UUID) error {
	return r.withTenant(ctx, tenantID, func(q querier) error {
		// The service refuses this when entries or referrers exist; the WHERE
		// clause here is the tenant scope, and RLS is the layer below it. The
		// cascade on content_type_fields and entries does the rest.
		tag, err := q.Exec(ctx, `DELETE FROM content_types WHERE tenant_id = $1 AND id = $2`, tenantID, id)
		if err != nil {
			return fmt.Errorf("delete content type: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return apperrors.ErrNotFound
		}
		return nil
	})
}

// --- entries ----------------------------------------------------------------

// unpublishedChangesExpr computes domain.Entry.HasUnpublishedChanges in SQL.
// Every path that returns an existing Entry — GetEntry, ListEntries, and both
// UPDATEs via RETURNING — selects it, so the field is never stale. CreateEntry
// is the one exception and does not need it: a new entry is always a draft
// (service/content_service.go), for which the expression is false anyway.
//
// Comparing the two payloads is the whole test. `version` is deliberately NOT
// consulted: it bumps on EVERY write, including one that stores the value the
// row already had, which is what reported phantom "unpublished changes"
// (ADR-006).
//
// An earlier draft led with `version <> published_version` as a cheap filter,
// relying on "every statement that writes payload also bumps version". That
// invariant is real today but nothing enforces it, and its failure direction is
// the unsafe one: a payload written without a version bump would report NO
// pending changes, and SetEntryStatus would then swallow the publish. Measured
// on 100 rows of course-body-sized payloads, the filter saved ~4ms per page
// (8.9ms → 4.8ms) — not worth an unguarded invariant on an admin list page.
//
// `IS DISTINCT FROM` on jsonb is a SEMANTIC comparison, so the same content
// written two ways is not a difference. That is not merely a nicety: values
// read back out of a jsonb column are canonical in key order and whitespace,
// but NOT in numeric form — '{"n":1.0}' and '{"n":1}' are jsonb-equal while
// their text differs. A Go-side byte comparison would call that an edit; the
// database does not. (It would also mean shipping both payloads to Go on every
// row just to compare them.)
var unpublishedChangesExpr = fmt.Sprintf(
	`(status = '%s' AND payload IS DISTINCT FROM published_payload)`,
	domain.StatusPublished,
)

// pendingReviewExpr selects what the release queue asks for (ADR-014 §2): the
// entries whose current working copy is not what the public can see.
//
// TWO HALVES, and §2's own table names both — `Live · edited` AND draft.
//
// §2 says the criterion is reusable because unpublishedChangesExpr "is already
// an independent SQL predicate, so the new query need not rewrite what counts as
// an unpublished change". That is true of the FIRST half only. The predicate
// opens with `status = 'published'`, so by itself it answers false for every
// draft — including the drafts §2 puts in the queue. Reused alone it builds a
// queue missing the half that has never been live, and verification clause 9
// does not catch that: a fixture whose "has changes" row is a live edited entry
// passes with the draft half entirely absent. So the difference test is reused
// rather than restated, and the not-live half is added beside it.
//
// `status <> published` rather than `status = draft`: the question is "what is
// not live", and spelling it as the absence of live keeps a status added later
// from silently falling out of the queue. Today the two are the same set —
// domain.ValidStatuses() has exactly draft and published.
//
// A RETRACTED entry is a draft holding a snapshot (ADR-014 §5.1 keeps
// published_payload through an unpublish), so it lands in the queue: it is not
// live, and whether it goes back up is a decision waiting for someone. Note this
// is the one row where §2's table and clause 9 can be read against each other —
// a retracted entry nobody has edited has NO working-copy difference, yet §2
// calls it a draft. The table decides here, because "no difference" is only
// well-defined for the live half.
var pendingReviewExpr = fmt.Sprintf(
	`(status <> '%s' OR %s)`,
	domain.StatusPublished, unpublishedChangesExpr,
)

// reviewDecisionNotSentBackExpr excludes an entry whose LATEST review decision
// was made against the version the entry is STILL at — "sent back and not yet
// re-edited" (ADR-014 Amendment: review decisions). It is ANDed alongside
// pendingReviewExpr in ListPendingReview rather than folded into it, because
// pendingReviewExpr's own criterion — does the working copy differ from what
// is live — is unchanged by this feature: a sent-back entry still, correctly,
// has unpublished changes. It is simply not the reviewer's turn to look at it
// again until the author acts, which is exactly the fact this clause removes
// it from the queue for.
//
// The entry returns to the queue automatically the moment it is next saved:
// UpdateEntry bumps entries.version, so the comparison below stops matching
// with no code path that has to notice "this was sent back" and clear
// anything.
//
// COALESCE(..., -1) makes "no decision at all" compare unequal to any real
// version (entries.version starts at 1), the same "absence cannot match" shape
// unpublishedChangesExpr already gives a NULL published_payload. The subquery
// is correlated by entries.id, spelled out rather than left unqualified,
// because entry_review_decisions has its own `id` column and an unqualified
// reference inside the subquery would resolve to THAT one, not entries.id —
// idx_entry_review_decisions_entry_recent (migration 000051) is what keeps
// this one indexed lookup per scanned row rather than a join that fans out.
var reviewDecisionNotSentBackExpr = `entries.version <> COALESCE((
	SELECT erd.entry_version
	FROM entry_review_decisions erd
	WHERE erd.entry_id = entries.id
	ORDER BY erd.decided_at DESC, erd.id DESC
	LIMIT 1
), -1)`

// pendingReviewVisibleExpr is ADR-009's DATA layer applied to the queue: the
// type-level read list, and own_only confinement, as a WHERE clause.
//
// IT EXISTS BECAUSE A CROSS-TYPE QUERY HAS NO PLACE ELSE TO PUT THEM. Every
// other entry read names ONE type, so the service can load that type and call
// guardTypeRead / guardOwned before it asks the database anything. The queue and
// the activity stream name none — which is exactly why they are their own
// queries — so a guard in the service has no type to guard against and the
// checks were simply absent: until this landed, an editor whose role is not on a
// type's read_roles still saw that type's rows here, name and title included,
// and a confined editor saw colleagues' drafts. The verb narrowing that shipped
// alongside does not touch this; it decides WHO may call the queue, not WHICH
// rows the queue may show them.
//
// $4 is the caller's tenant role and $5 the user who answers for them (the
// principal, for an agent credential — though no agent reaches this path).
//
// THE TWO LISTS HAVE OPPOSITE POLARITY and the SQL has to say so separately:
// read_roles names who is ALLOWED, so an EMPTY list allows everyone; own_only
// names who is CONFINED, so an empty list confines nobody. Matching them by
// shape rather than by meaning reads the second one backwards — domain's
// ConfinesToOwn carries the same warning for the same reason.
//
// `created_by = $5` and NOT `IS NOT DISTINCT FROM`: a row with no recorded
// author matches nobody, the fail-closed direction, and the same spelling
// buildWhere uses for confinement three hundred lines down. Rows predating
// 000021 therefore drop out of a confined caller's queue rather than appearing
// in everyone's.
//
// dataVisibleExpr is the same clause, parameterized by whichever placeholders
// a caller's OWN positional argument list assigns to the role and the user
// id — $4/$5 are literal parameter numbers, not a position-agnostic pattern,
// so a second query with a different argument layout (SearchEntries, ADR-021
// §3) cannot reuse the fixed string and calls this instead with its own
// placeholders. pendingReviewVisibleExpr is this function pinned to $4/$5,
// kept as a named var rather than inlined at its one call site because
// ListPendingReview's own comment refers to it by name.
func dataVisibleExpr(roleParam, userIDParam string) string {
	return fmt.Sprintf(`EXISTS (
		SELECT 1 FROM content_types ct
		WHERE ct.id = entries.content_type_id
		  AND ct.tenant_id = entries.tenant_id
		  AND (cardinality(ct.read_roles) = 0 OR %s = ANY(ct.read_roles))
		  AND (NOT (%s = ANY(ct.own_only_roles)) OR entries.created_by = %s)
	)`, roleParam, roleParam, userIDParam)
}

var pendingReviewVisibleExpr = dataVisibleExpr("$4", "$5")

// entrySelectColumns is shared by GetEntry and ListEntries so a column added to
// one cannot be forgotten in the other. They read the same row into the same
// struct, and when they drifted the symptom was silent: the single-entry path
// returned a field the list path left zeroed, which looks like data rather than
// a bug. scanEntry is the matching half — the two must be edited together.
var entrySelectColumns = `id, tenant_id, content_type_id, payload, version, status,
	published_payload, published_version, published_at, locale, translation_group_id,
	created_by, updated_by, published_by, created_by_kind, created_by_agent,
	updated_by_kind, updated_by_agent, created_at, updated_at, ` + unpublishedChangesExpr

// entryScanner is satisfied by both pgx.Row and pgx.Rows.
type entryScanner interface{ Scan(dest ...any) error }

// scanEntry reads one row selected with entrySelectColumns. The nullable columns
// land in locals first because the domain type holds them unwrapped.
func scanEntry(s entryScanner) (*domain.Entry, error) {
	var e domain.Entry
	var payload, publishedPayload []byte
	var publishedVersion *int
	if err := s.Scan(
		&e.ID, &e.TenantID, &e.ContentTypeID, &payload, &e.Version, &e.Status,
		&publishedPayload, &publishedVersion, &e.PublishedAt, &e.Locale, &e.TranslationGroupID,
		&e.CreatedBy, &e.UpdatedBy, &e.PublishedBy, &e.CreatedByKind, &e.CreatedByAgent,
		&e.UpdatedByKind, &e.UpdatedByAgent,
		&e.CreatedAt, &e.UpdatedAt, &e.HasUnpublishedChanges,
	); err != nil {
		return nil, err
	}
	e.Payload = json.RawMessage(payload)
	if publishedPayload != nil {
		e.PublishedPayload = json.RawMessage(publishedPayload)
	}
	if publishedVersion != nil {
		e.PublishedVersion = *publishedVersion
	}
	return &e, nil
}

func (r *PostgresContentRepository) CreateEntry(ctx context.Context, e *domain.Entry) error {
	if e.Version == 0 {
		e.Version = 1
	}
	// pgx encodes json.RawMessage as jsonb (see internal/pkg/outbox); passing a
	// plain []byte would be sent as bytea and fail the jsonb column.
	if e.Status == "" {
		e.Status = domain.StatusDraft
	}
	return r.withTenant(ctx, e.TenantID, func(q querier) error {
		// created_by and updated_by both get the creator: a new entry's last
		// editor IS its author, mirroring created_at/updated_at both getting now.
		//
		// The created_by provenance pair is written here and never updated
		// afterwards: it describes how the row CAME INTO BEING, so a later edit by
		// someone else does not restate it, exactly as created_by does not move.
		// The updated_by pair opposite it starts as a copy and is overwritten by
		// every subsequent write, exactly as updated_by is.
		kind := e.CreatedByKind
		if kind == "" {
			// A caller that says nothing gets the column default's meaning
			// rather than a constraint violation. Not a fabricated fact: only a
			// path that never heard of ADR-013 leaves it empty, and no such path
			// can produce an agent write — an agent Subject reaches this struct
			// only through provenanceOf, which always fills the pair in.
			kind = domain.ActorKindHuman
		}
		// Put the normalised values back on the struct the caller still holds: the
		// service projects its DTO from this same entry after the call returns, and
		// leaving them as the caller passed them would have the response report a
		// provenance the table does not hold (an empty kind, and no updated_by pair
		// at all — which a reader is required to render as unknown, for a row whose
		// author we know perfectly well).
		e.CreatedByKind = kind
		e.UpdatedByKind, e.UpdatedByAgent = &kind, e.CreatedByAgent
		// search_text is Go-computed by the caller (domain.ExtractSearchText,
		// ADR-021) and simply bound here — no schema access at this layer.
		// published_search_text is not: a new entry is always created as a
		// draft (Status defaults to domain.StatusDraft above), so it has no
		// published snapshot and the column keeps its '' default until a
		// publish copies search_text into it (SetEntryPublishState).
		if _, err := q.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, search_text, version, status, published_at, locale, translation_group_id, created_by, updated_by, created_by_kind, created_by_agent, updated_by_kind, updated_by_agent, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11, $12, $13, $12, $13, $14, $15)`,
			e.ID, e.TenantID, e.ContentTypeID, e.Payload, e.SearchText, e.Version, e.Status, e.PublishedAt, e.Locale, e.TranslationGroupID, e.CreatedBy, kind, e.CreatedByAgent, e.CreatedAt, e.UpdatedAt,
		); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
				// (tenant, translation_group_id, locale) is taken: this group
				// already has a row in that language.
				return ErrTranslationExists
			}
			return fmt.Errorf("insert entry: %w", err)
		}
		if err := refreshEntryUniqueValues(ctx, q, e.TenantID, e.ID); err != nil {
			return err
		}
		// Version 1 is an ordinary revision, not a special case: it overwrote
		// nothing, and a history whose first row is missing cannot answer "what
		// did this entry start as" — the question a reader asks precisely when
		// everything after it looks wrong.
		if err := recordEntryRevision(ctx, q, e.TenantID, e.ID); err != nil {
			return err
		}
		return r.emitEntryEvent(ctx, q, outbox.EventContentEntryCreated,
			e.TenantID, e.ContentTypeID, e.ID, e.Locale,
			fmt.Sprintf("content:%s:created", e.ID))
	})
}

func (r *PostgresContentRepository) GetEntry(ctx context.Context, tenantID string, contentTypeID, id uuid.UUID) (*domain.Entry, error) {
	var e *domain.Entry
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		row := q.QueryRow(ctx, `
			SELECT `+entrySelectColumns+`
			FROM entries
			WHERE tenant_id = $1 AND content_type_id = $2 AND id = $3`,
			tenantID, contentTypeID, id,
		)
		var scanErr error
		e, scanErr = scanEntry(row)
		return scanErr
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperrors.ErrNotFound
		}
		return nil, fmt.Errorf("entry scan: %w", err)
	}
	return e, nil
}

// GetEntriesByIDs is the batched read behind relation populate — see the
// interface for the contract. Everything interesting about it is the two things
// it does NOT do.
//
// It does not bind content_type_id, so one statement serves every populated
// field of a page no matter how many types they point at. Tenancy is still
// bound twice: `app.tenant_id` for RLS via withTenant, and `tenant_id = $1` in
// the WHERE, exactly as every other read here spells it.
//
// It does not loop. `id = ANY($2)` takes the whole deduplicated id set as one
// array parameter, which is the difference between one round trip per PAGE and
// one per relation VALUE. pgx encodes []uuid.UUID as a uuid[], so the array
// never becomes an IN-list built by string concatenation.
//
// matchPublished requires BOTH `status = published` and a non-NULL snapshot.
// The status alone is not enough — a draft has no snapshot to serve — and the
// snapshot alone is not enough either, because an unpublish RETAINS
// published_payload (migration 000033): matching on it would keep serving
// content someone deliberately retracted. This is the same pair the delivery
// list enforces through Status + MatchPublished, spelled here as a predicate
// rather than a flag because this query has no caller-supplied clauses to bind.
//
// Without it, every state of the tenant's own rows comes back — the admin
// expansion (ADR-006 Amendment 7), where the working copy is the answer and a
// never-published target is an ordinary draft. Nothing else changes: the tenant
// predicate and withTenant's RLS are outside the conditional, so the flag can
// only ever widen the STATE filter, never the tenancy one.
func (r *PostgresContentRepository) GetEntriesByIDs(ctx context.Context, tenantID string, ids []uuid.UUID, matchPublished bool) ([]*domain.Entry, error) {
	// No ids is not a degenerate query to send — it is a page with nothing to
	// populate, which is the common case for an entry whose relation fields are
	// all empty.
	if len(ids) == 0 {
		return nil, nil
	}
	where := "tenant_id = $1 AND id = ANY($2)"
	args := []any{tenantID, ids}
	if matchPublished {
		where += " AND status = $3 AND published_payload IS NOT NULL"
		args = append(args, domain.StatusPublished)
	}
	var items []*domain.Entry
	if err := r.withTenant(ctx, tenantID, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT `+entrySelectColumns+` FROM entries WHERE `+where, args...)
		if err != nil {
			return fmt.Errorf("entries by ids: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanEntry(rows)
			if err != nil {
				return fmt.Errorf("entries by ids scan: %w", err)
			}
			items = append(items, e)
		}
		return rows.Err()
	}); err != nil {
		return nil, err
	}
	return items, nil
}

// SetEntryPublishState flips editorial state AND moves the published snapshot in
// one statement. It is separate from UpdateEntry because the two writes mean
// opposite things: UpdateEntry saves a draft and must leave the live snapshot
// alone; this promotes the draft to live (or retracts it).
//
// published_version is written as version + 1 — the value the row will hold
// after this same UPDATE bumps it, so it records the version that was actually
// snapshotted. The pre-increment value would name a version that is not the one
// on the shelf. Nothing reads it to decide "has unpublished changes" (see
// unpublishedChangesExpr); it is a record of what went live, not a criterion.
//
// published_at is only set on a transition INTO published; a re-publish keeps
// the original. Note the timestamp means "first released SINCE the last
// unpublish" — SetEntryStatus clears it on retract, so a retract/re-release
// cycle restarts it.
func (r *PostgresContentRepository) SetEntryPublishState(ctx context.Context, e *domain.Entry, status string, publishedAt *time.Time, opts ...domain.PublishOrigin) error {
	// VARIADIC, and that is a compatibility decision rather than a taste one.
	// This method has three production callers and roughly twenty integration
	// tests; only two of the callers know how the publish was triggered. A fourth
	// required parameter would make every one of those tests state a mechanism it
	// has no opinion about, and the reviewer would then have to check twenty
	// mechanical edits for the one that got it wrong. The zero value — a direct,
	// hand-pressed publish — is both the default and the right answer for a
	// caller that has not thought about it.
	//
	// More than one is a programming error, not a merge: two callers disagreeing
	// about why a publish happened is not something to average.
	var origin domain.PublishOrigin
	switch len(opts) {
	case 0:
	case 1:
		origin = opts[0]
	default:
		return fmt.Errorf("set entry publish state: %d publish origins given, want at most 1", len(opts))
	}
	return r.withTenant(ctx, e.TenantID, func(q querier) error {
		// published_by rides the same CASE as the snapshot columns because it
		// describes the snapshot — and since ADR-014 §5.1 the snapshot SURVIVES a
		// retract, so all three keep their existing values instead of being
		// nulled. Clearing only published_by would leave the three describing
		// different copies, which is what ADR-006's standing rule forbids; the
		// full argument is in 000033's header.
		//
		// The CASE — not the caller — is still the enforcement point, exactly as
		// it is for published_payload, so the service may pass the actor either
		// way. That is why published_by is READ BACK below: the caller sets it
		// unconditionally, and on a retract the value that survives is the one
		// already in the row. Without the read-back the returned entry would name
		// whoever took the entry DOWN as its publisher.
		//
		// published_search_text rides the SAME CASE as published_payload, one row
		// down, needing no new bound parameter: by the time a publish reaches
		// here, search_text already holds the precise value the last
		// CreateEntry/UpdateEntry/RestoreEntryRevision computed via
		// domain.ExtractSearchText, so copying the column onto itself is exactly
		// as correct as copying payload onto published_payload one line up
		// (ADR-021 §2).
		//
		// updated_by is a SEPARATE parameter on purpose. Binding it to the same
		// value as published_by would null it on retract, and an unpublish very
		// much has an editor.
		//
		// Its kind/agent pair moves with it, in the same statement, for the reason
		// the domain comment gives: publish and unpublish ARE writes, so leaving
		// the pair behind would leave the row saying a bot made the last change
		// when a person just pressed publish over it, or the reverse.
		err := q.QueryRow(ctx, `
			UPDATE entries
			SET status = $4,
			    published_at = $5,
			    updated_at = $6,
			    updated_by = $9,
			    updated_by_kind = $11,
			    updated_by_agent = $12,
			    published_payload = CASE WHEN $4 = $8 THEN payload ELSE published_payload END,
			    published_search_text = CASE WHEN $4 = $8 THEN search_text ELSE published_search_text END,
			    published_version = CASE WHEN $4 = $8 THEN version + 1 ELSE published_version END,
			    published_by = CASE WHEN $4 = $8 THEN $10::uuid ELSE published_by END,
			    version = version + 1
			WHERE tenant_id = $1 AND content_type_id = $2 AND id = $3 AND version = $7
			RETURNING version, published_version, published_by, `+unpublishedChangesExpr,
			e.TenantID, e.ContentTypeID, e.ID, status, publishedAt, e.UpdatedAt, e.Version, domain.StatusPublished, e.UpdatedBy, e.PublishedBy,
			e.UpdatedByKind, e.UpdatedByAgent,
		).Scan(&e.Version, &publishedVersionScan{e}, &e.PublishedBy, &e.HasUnpublishedChanges)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("set entry publish state: %w", err)
			}
			var exists bool
			if exErr := q.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM entries WHERE tenant_id = $1 AND content_type_id = $2 AND id = $3)`,
				e.TenantID, e.ContentTypeID, e.ID,
			).Scan(&exists); exErr != nil {
				return fmt.Errorf("set entry publish state conflict check: %w", exErr)
			}
			if exists {
				return ErrVersionConflict
			}
			return apperrors.ErrNotFound
		}
		// A publish reserves the snapshot's values and an unpublish releases
		// them; the working copy's reservations are unchanged either way. A
		// publish cannot collide — the working copy already held the value —
		// but the refresh is unconditional so that "when does the ledger move"
		// has one answer: whenever the row does.
		if err := refreshEntryUniqueValues(ctx, q, e.TenantID, e.ID); err != nil {
			return err
		}
		// Someone did by hand what a schedule was waiting to do. The schedule
		// yields, in this transaction, so the two records cannot disagree about
		// what put the entry into its current state (ADR-017).
		//
		// The scheduler worker does not reach this: it moves its own row out of
		// 'pending' before calling here, so there is nothing left for this to
		// supersede. See markSchedulesSuperseded.
		if err := markSchedulesSuperseded(ctx, q, e.TenantID, e.ID); err != nil {
			return err
		}
		e.Status = status
		e.PublishedAt = publishedAt
		// Mirror the CASE above. A retract no longer touches the snapshot, so the
		// in-memory copy must not be touched either — it already holds what the
		// row holds, and nulling it here would make the returned entry disagree
		// with the database it was just written to. published_by needs no mirror
		// at all: the RETURNING clause above scanned the surviving value back.
		if status == domain.StatusPublished {
			e.PublishedPayload = e.Payload
		}
		// The published asset references follow the snapshot, not the working copy
		// — same transaction, or a crash between the two would leave delivery
		// serving a snapshot whose images are already revoked.
		//
		// "Follow the snapshot" is why a retract leaves this table ALONE since
		// ADR-014 §5.1. The rewrite below is a publish-only operation: the
		// unconditional DELETE it used to lead with was correct only while a
		// retract also destroyed the snapshot. Now the snapshot survives, and
		// clearing its references would leave it naming assets that nothing
		// protects from a revoke — 000020's original failure mode, reached from
		// the other side. Retaining them widens no access: AssetIsPublished joins
		// entries and requires status = 'published'.
		if status == domain.StatusPublished {
			if _, err := q.Exec(ctx,
				`DELETE FROM entry_media_published WHERE tenant_id = $1 AND entry_id = $2`, e.TenantID, e.ID,
			); err != nil {
				return fmt.Errorf("clear entry_media_published: %w", err)
			}
			if _, err := q.Exec(ctx, `
				INSERT INTO entry_media_published (entry_id, asset_id, tenant_id)
				SELECT entry_id, asset_id, tenant_id FROM entry_media
				WHERE tenant_id = $1 AND entry_id = $2
				ON CONFLICT DO NOTHING`, e.TenantID, e.ID,
			); err != nil {
				return fmt.Errorf("snapshot entry_media_published: %w", err)
			}
			// entry_relations_published follows the snapshot for exactly the
			// same reason entry_media_published does — see the paragraph
			// above and 000050's header. Copied wholesale from entry_relations
			// rather than re-derived from published_payload: the working
			// copy's links are already the validated, current truth (this
			// transaction's ReplaceEntryRelations call ran moments earlier at
			// the same write point ReplaceEntryMedia did), and published_payload
			// IS payload as of the CASE above, so the two sources agree.
			if _, err := q.Exec(ctx,
				`DELETE FROM entry_relations_published WHERE tenant_id = $1 AND entry_id = $2`, e.TenantID, e.ID,
			); err != nil {
				return fmt.Errorf("clear entry_relations_published: %w", err)
			}
			if _, err := q.Exec(ctx, `
				INSERT INTO entry_relations_published (entry_id, field_path, target_entry_id, tenant_id)
				SELECT entry_id, field_path, target_entry_id, tenant_id FROM entry_relations
				WHERE tenant_id = $1 AND entry_id = $2
				ON CONFLICT DO NOTHING`, e.TenantID, e.ID,
			); err != nil {
				return fmt.Errorf("snapshot entry_relations_published: %w", err)
			}
		}
		// NO entry_revisions ROW IS RECORDED HERE, and that omission is still
		// deliberate (ADR-014 §5). This statement bumps `version` without
		// touching `payload`, so the row would be a byte-identical copy of the
		// previous working-copy revision. The cost is that version numbers in
		// entry_revisions are sparse, which is why the read rule is "newest
		// revision with version <= N" rather than "the row with version = N";
		// migration 000034's header carries it.
		//
		// A entry_publish_revisions ROW IS RECORDED, on a publish only (ADR-018).
		// It is the other table and the other question: not "what did the working
		// copy look like at version N", which the paragraph above answers by
		// resolution, but "which of those drafts was the one on the shelf, when,
		// and on whose authority" — which nothing above can answer, because the
		// facts are produced by this statement's own CASE expressions.
		//
		// THIS IS THE CHOKE POINT, and that is why the write lives at the
		// repository rather than in the service beside the activity line. A
		// manual publish arrives through SetEntryStatus; a scheduled one arrives
		// from the scheduler worker, which builds its own transaction and calls
		// this method directly without passing through the service at all. A
		// revision recorded in the service would therefore exist for
		// hand-pressed releases and silently not for scheduled ones — the exact
		// asymmetry the `via` column exists to make visible.
		//
		// An unpublish records nothing: it moves no snapshot (§5.1 keeps
		// published_payload), so there is no new release, and a row would put a
		// duplicate of the previous one into the list under a later date.
		if status == domain.StatusPublished {
			if err := recordPublishRevision(ctx, q, e.TenantID, e.ID, origin); err != nil {
				return err
			}
		}
		event := outbox.EventContentEntryUnpublished
		if status == domain.StatusPublished {
			event = outbox.EventContentEntryPublished
		}
		return r.emitEntryEvent(ctx, q, event,
			e.TenantID, e.ContentTypeID, e.ID, e.Locale,
			fmt.Sprintf("content:%s:%s:%d", e.ID, status, e.Version))
	})
}

// publishedVersionScan lets one RETURNING clause write a nullable column into a
// non-pointer domain field: NULL (unpublished) lands as 0.
type publishedVersionScan struct{ e *domain.Entry }

func (s *publishedVersionScan) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		s.e.PublishedVersion = 0
	case int32:
		s.e.PublishedVersion = int(v)
	case int64:
		s.e.PublishedVersion = int(v)
	case int:
		s.e.PublishedVersion = v
	default:
		return fmt.Errorf("published_version: unexpected type %T", src)
	}
	return nil
}

func (r *PostgresContentRepository) UpdateEntry(ctx context.Context, e *domain.Entry) error {
	// Optimistic lock: the write only applies if the stored version still equals
	// the version the caller read (e.Version). This closes the read-modify-write
	// window so a concurrent writer can't be silently clobbered (last-write-wins).
	return r.withTenant(ctx, e.TenantID, func(q querier) error {
		// Editorial state is deliberately NOT in this statement: saving a draft
		// must not be able to change what is published, even by accident. The
		// only path that moves status or the snapshot is SetEntryPublishState.
		// search_text rides beside payload here for the same reason it rides
		// beside it in CreateEntry: the caller (UpdateEntry / RestoreEntryRevision
		// in the service) has already recomputed it via domain.ExtractSearchText
		// against the same normalised payload this statement writes, and this
		// layer has no schema to compute it from itself. published_search_text is
		// untouched — editing a draft must not move what is published, exactly as
		// the comment above already says for status and the snapshot.
		err := q.QueryRow(ctx, `
			UPDATE entries
			SET payload = $4, search_text = $10, updated_at = $5, updated_by = $7,
			    updated_by_kind = $8, updated_by_agent = $9, version = version + 1
			WHERE tenant_id = $1 AND content_type_id = $2 AND id = $3 AND version = $6
			RETURNING version, `+unpublishedChangesExpr,
			e.TenantID, e.ContentTypeID, e.ID, e.Payload, e.UpdatedAt, e.Version, e.UpdatedBy,
			e.UpdatedByKind, e.UpdatedByAgent, e.SearchText,
		).Scan(&e.Version, &e.HasUnpublishedChanges)
		if err == nil {
			if err := refreshEntryUniqueValues(ctx, q, e.TenantID, e.ID); err != nil {
				return err
			}
			// Only on the applied path. The two failures below — gone, or the
			// version moved under us — wrote nothing to `entries`, and a revision
			// for either would be a version of the content that never existed
			// (ADR-014 §5).
			if err := recordEntryRevision(ctx, q, e.TenantID, e.ID); err != nil {
				return err
			}
			// The working copy moved, so any schedule pinned to the version it
			// moved from is now aimed at content nobody approved (ADR-017). Same
			// transaction as the write for the reason on markSchedulesStale, and
			// on the applied path only for recordEntryRevision's: a write that did
			// not land invalidates nothing.
			if err := markSchedulesStale(ctx, q, e); err != nil {
				return err
			}
			// The post-bump version keys the event, so every applied write is one
			// distinct event and a replayed transaction dedupes on conflict.
			return r.emitEntryEvent(ctx, q, outbox.EventContentEntryUpdated,
				e.TenantID, e.ContentTypeID, e.ID, e.Locale,
				fmt.Sprintf("content:%s:updated:%d", e.ID, e.Version))
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("update entry: %w", err)
		}
		// No row matched id+version: either the entry is gone (404) or its
		// version advanced under us (409 conflict). Disambiguate on the same tx.
		var exists bool
		if exErr := q.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM entries WHERE tenant_id = $1 AND content_type_id = $2 AND id = $3)`,
			e.TenantID, e.ContentTypeID, e.ID,
		).Scan(&exists); exErr != nil {
			return fmt.Errorf("update entry conflict check: %w", exErr)
		}
		if exists {
			return ErrVersionConflict
		}
		return apperrors.ErrNotFound
	})
}

func (r *PostgresContentRepository) DeleteEntry(ctx context.Context, tenantID string, contentTypeID, id uuid.UUID) error {
	var affected int64
	if err := r.withTenant(ctx, tenantID, func(q querier) error {
		tag, err := q.Exec(ctx, `
			DELETE FROM entries
			WHERE tenant_id = $1 AND content_type_id = $2 AND id = $3`,
			tenantID, contentTypeID, id,
		)
		if err != nil {
			return fmt.Errorf("delete entry: %w", err)
		}
		affected = tag.RowsAffected()
		if affected == 0 {
			// The 404 is decided outside; a miss must not emit.
			return nil
		}
		// Locale is deliberately absent: the row is gone, and the type lookup
		// inside emitEntryEvent still resolves because deleting a TYPE refuses
		// while entries remain — this row's deletion is what makes that possible.
		return r.emitEntryEvent(ctx, q, outbox.EventContentEntryDeleted,
			tenantID, contentTypeID, id, "",
			fmt.Sprintf("content:%s:deleted", id))
	}); err != nil {
		return err
	}
	if affected == 0 {
		return apperrors.ErrNotFound
	}
	return nil
}

func (r *PostgresContentRepository) EntryExists(ctx context.Context, tenantID string, contentTypeID, id uuid.UUID) (bool, error) {
	var exists bool
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		return q.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM entries
				WHERE tenant_id = $1 AND content_type_id = $2 AND id = $3
			)`, tenantID, contentTypeID, id,
		).Scan(&exists)
	})
	if err != nil {
		return false, fmt.Errorf("entry exists: %w", err)
	}
	return exists, nil
}

func (r *PostgresContentRepository) ListEntries(ctx context.Context, f ListEntriesFilter) ([]*domain.Entry, int, error) {
	where, args, err := buildWhere(f)
	if err != nil {
		return nil, 0, err
	}

	limit := f.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	// Reuse the WHERE args, then append ORDER BY key (if any), LIMIT, OFFSET.
	selArgs := append([]any{}, args...)
	bind := func(v any) string { selArgs = append(selArgs, v); return fmt.Sprintf("$%d", len(selArgs)) }

	// Every ordering ends in `id DESC`. created_at is not unique (a seeding run
	// writes many rows in one microsecond) and neither is a payload sort key, so
	// without the tiebreaker the order within a tie is whatever the plan happens
	// to produce — which makes ANY pagination, offset or keyset, able to skip or
	// repeat rows across page boundaries.
	orderExpr := "created_at DESC, id DESC"
	// sortExpr is the JSONB extraction the ORDER BY and the keyset predicate
	// BOTH render from. One string, built once, so the window and the order
	// cannot drift apart — a keyset that compares a different expression from
	// the one it orders by skips rows silently, and only under a tie.
	sortExpr := ""
	if f.Sort != nil {
		// The same copy the WHERE read: ordering the snapshot's rows by the
		// working copy's values would leak the edit order back out.
		col, err := predicateColumn(f)
		if err != nil {
			return nil, 0, err
		}
		kp := bind(f.Sort.Field.Key)
		sortExpr = orderedExpr(col, kp, f.Sort.Field.Type)
		dir := "ASC"
		if f.Sort.Desc {
			dir = "DESC"
		}
		if f.CursorPaged {
			// NULLS LAST in BOTH directions, spelled as a leading boolean rather
			// than the NULLS LAST clause because the keyset predicate below has
			// to reproduce it as a comparison, and `(expr IS NULL)` is a term it
			// can compare against. Postgres's own default is nulls FIRST under
			// DESC and LAST under ASC, so flipping the direction would otherwise
			// move the missing-value rows from one end of the collection to the
			// other — a caller reversing the sort would find its first page full
			// of entries that never set the field.
			//
			// created_at is NOT in this ordering: id alone breaks every tie, and
			// a third column the cursor does not carry would be a term the
			// window cannot reproduce.
			orderExpr = fmt.Sprintf("(%s IS NULL), %s %s, id %s", sortExpr, sortExpr, dir, dir)
		} else {
			orderExpr = fmt.Sprintf("%s %s, created_at DESC, id DESC", sortExpr, dir)
		}
	}

	// Keyset pagination: rows STRICTLY past the cursor in exactly the order
	// above.
	// Deliberately a separate string: `where` (and its `args`) still feed the
	// COUNT below, and the cursor placeholders are indexed against selArgs.
	selWhere := where
	if f.CursorPaged && f.After != nil {
		ip := bind(f.After.ID)
		switch {
		case f.Sort == nil:
			// The row-value comparison is what makes this a single
			// index-friendly predicate rather than the (created_at < x OR
			// (created_at = x AND id < y)) spelling.
			cp := bind(f.After.CreatedAt)
			selWhere += fmt.Sprintf(" AND (created_at, id) < (%s, %s)", cp, ip)
		case f.After.SortValue == nil:
			// The cursor is already inside the trailing null block, so every row
			// still ahead of it is a null row with a smaller id. No row-value
			// spelling here: `(NULL, id) < (NULL, $n)` is NULL, not true, and a
			// window that evaluates to NULL returns nothing at all — the page
			// loop would stop early and call it the end of the collection.
			selWhere += fmt.Sprintf(" AND %s IS NULL AND id %s %s", sortExpr, cmpFor(f.Sort.Desc), ip)
		default:
			// Ahead of the cursor within the non-null block, OR anywhere in the
			// null block — which sorts entirely after it. Not a row-value
			// comparison: `expr` is nullable, and the whole point of the leading
			// `(expr IS NULL)` in the ORDER BY is that nulls are ordered by a
			// term a row-value tuple has no way to name.
			vp := bind(*f.After.SortValue)
			v := orderedValue(vp, f.Sort.Field.Type)
			cmp := cmpFor(f.Sort.Desc)
			selWhere += fmt.Sprintf(
				" AND ((%[1]s IS NOT NULL AND (%[1]s %[2]s %[3]s OR (%[1]s = %[3]s AND id %[2]s %[4]s))) OR %[1]s IS NULL)",
				sortExpr, cmp, v, ip)
		}
	}

	lp := bind(limit)
	sql := "SELECT " + entrySelectColumns + " " +
		"FROM entries WHERE " + selWhere + " ORDER BY " + orderExpr + " LIMIT " + lp
	if !f.CursorPaged {
		sql += " OFFSET " + bind(offset)
	}

	var (
		items []*domain.Entry
		total int
	)
	// Count + page in one tenant-scoped tx so RLS applies to both and they see
	// a consistent snapshot.
	if err := r.withTenant(ctx, f.TenantID, func(q querier) error {
		// Keyset pagination does not report a total: COUNT(*) over the whole
		// match set is the cost this mode exists to avoid, and the caller is
		// handed has_more/next_cursor instead. total stays 0 — callers in cursor
		// mode must not read it.
		if !f.CursorPaged {
			if err := q.QueryRow(ctx, "SELECT COUNT(*) FROM entries WHERE "+where, args...).Scan(&total); err != nil {
				return fmt.Errorf("entries count: %w", err)
			}
		}
		rows, err := q.Query(ctx, sql, selArgs...)
		if err != nil {
			return fmt.Errorf("entries list: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanEntry(rows)
			if err != nil {
				return fmt.Errorf("entries scan: %w", err)
			}
			items = append(items, e)
		}
		return rows.Err()
	}); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// ListPendingReview returns the tenant's release queue (ADR-014 §2), newest
// first, with the entries an agent touched last ahead of the rest.
//
// It spans EVERY content type, which is the whole reason it exists as its own
// query: ListEntries binds content_type_id in buildWhere's opening clause, so
// the queue could otherwise only be assembled by asking once per type and
// merging in Go — N+1 requests whose combined page has no stable order.
//
// It selects entrySelectColumns and scans with scanEntry, so a column added to
// the entry row reaches this path for free; the queue is a different WHERE over
// the same row, not a different row.
//
// AGENT-FIRST ordering is §2's, and the reason is that the queue answers "what
// needs me" — work a person did themselves needs them least.
//
// `IS NOT DISTINCT FROM` rather than `=`, and this is not stylistic.
// updated_by_kind is NULLABLE — entries written before the provenance columns
// landed have no recorded writer — and `NULL = 'agent'` is NULL, not false. A
// NULL sort key under DESC sorts NULLS FIRST in Postgres, so the plain equality
// spelling puts every entry with an UNKNOWN writer ahead of the agent work the
// ordering exists to surface: the exact inversion of the requirement, and worst
// on the oldest rows. `IS NOT DISTINCT FROM` is never NULL, so the three-valued
// case disappears instead of being sorted around.
//
// The Go fake cannot see this. It mirrors the INTENT (`kind != nil && *kind ==
// agent`), which is what the SQL was meant to say, so the service-level test
// passed while the database returned the opposite order.
//
// NO COUNT. The queue is a landing-page list with a bounded limit, and a total
// over every type in the tenant would be a second full scan to render a number
// nobody acts on.
func (r *PostgresContentRepository) ListPendingReview(ctx context.Context, f PendingReviewFilter) ([]*domain.Entry, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = pendingReviewLimitDefault
	}
	if limit > pendingReviewLimitMax {
		limit = pendingReviewLimitMax
	}
	var items []*domain.Entry
	err := r.withTenant(ctx, f.TenantID, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT `+entrySelectColumns+`
			FROM entries
			WHERE tenant_id = $1 AND `+pendingReviewExpr+`
			  AND `+reviewDecisionNotSentBackExpr+`
			  AND `+pendingReviewVisibleExpr+`
			ORDER BY (updated_by_kind IS NOT DISTINCT FROM $2) DESC, updated_at DESC, id DESC
			LIMIT $3`,
			f.TenantID, domain.ActorKindAgent, limit, f.ViewerRole, f.ViewerUserID,
		)
		if err != nil {
			return fmt.Errorf("pending review list: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanEntry(rows)
			if err != nil {
				return fmt.Errorf("pending review scan: %w", err)
			}
			items = append(items, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

// SearchEntries is GET /api/v1/content/search (ADR-021 §3): the cross-type
// counterpart to ListEntries' `q`. It shares ListPendingReview's data-level
// visibility gate (pendingReviewVisibleExpr) — the only other query in this
// repository with no single content type to guard against — and, like it,
// carries NO COUNT: this is a search box's result list, not a page with a
// total.
//
// Always reads search_text, never published_search_text: this endpoint is
// staff-only (the service refuses sub.PublicDelivery before it ever reaches
// here — see SearchEntriesFilter's own comment), so the WORKING copy is what
// "search across everything" means for the audience that may see it.
func (r *PostgresContentRepository) SearchEntries(ctx context.Context, f SearchEntriesFilter) ([]SearchEntryRow, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = searchEntriesLimitDefault
	}
	if limit > searchEntriesLimitMax {
		limit = searchEntriesLimitMax
	}
	args := []any{f.TenantID}
	bind := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	// dataVisibleExpr, not the fixed pendingReviewVisibleExpr: this query's
	// argument layout is built incrementally through bind() below (term
	// clauses, then Locale, then Status, then Limit — an order this endpoint
	// owns, not ListPendingReview's fixed $1..$5), so the role/user id
	// placeholders must be whatever bind() actually assigns them, not a
	// hardcoded $4/$5 that would silently bind to the wrong argument.
	roleParam := bind(f.ViewerRole)
	useridParam := bind(f.ViewerUserID)
	clauses := []string{"tenant_id = $1", dataVisibleExpr(roleParam, useridParam)}

	for _, term := range strings.Fields(f.Query) {
		clauses = append(clauses, "search_text ILIKE "+bind("%"+likeEscape(term)+"%"))
	}
	if f.Locale != "" {
		clauses = append(clauses, "locale = "+bind(f.Locale))
	}
	if f.Status != "" {
		clauses = append(clauses, "status = "+bind(f.Status))
	}
	limitParam := bind(limit)

	var items []SearchEntryRow
	err := r.withTenant(ctx, f.TenantID, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT id, content_type_id, locale, status, updated_at, search_text
			FROM entries
			WHERE `+strings.Join(clauses, " AND ")+`
			ORDER BY updated_at DESC, id DESC
			LIMIT `+limitParam,
			args...,
		)
		if err != nil {
			return fmt.Errorf("search entries: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var row SearchEntryRow
			if err := rows.Scan(&row.ID, &row.ContentTypeID, &row.Locale, &row.Status, &row.UpdatedAt, &row.SearchText); err != nil {
				return fmt.Errorf("search entries scan: %w", err)
			}
			items = append(items, row)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

// ListLocales groups the tenant's (visible) entries by locale, optionally
// narrowed to one content type. One query, not one COUNT per locale — the
// same reasoning SearchEntries' comment gives for reading the whole result set
// in one call.
func (r *PostgresContentRepository) ListLocales(ctx context.Context, f ListLocalesFilter) ([]LocaleCountRow, error) {
	args := []any{f.TenantID}
	bind := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	roleParam := bind(f.ViewerRole)
	useridParam := bind(f.ViewerUserID)
	clauses := []string{"tenant_id = $1", dataVisibleExpr(roleParam, useridParam)}
	if f.ContentTypeID != nil {
		clauses = append(clauses, "content_type_id = "+bind(*f.ContentTypeID))
	}

	var items []LocaleCountRow
	err := r.withTenant(ctx, f.TenantID, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT locale, count(*)
			FROM entries
			WHERE `+strings.Join(clauses, " AND ")+`
			GROUP BY locale
			ORDER BY locale`,
			args...,
		)
		if err != nil {
			return fmt.Errorf("list locales: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var row LocaleCountRow
			if err := rows.Scan(&row.Locale, &row.Entries); err != nil {
				return fmt.Errorf("list locales scan: %w", err)
			}
			items = append(items, row)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

// buildWhere assembles the tenant + type scope plus any field filters into a
// parameterized WHERE clause. Both JSONB keys and values are bound as query
// parameters — never string-concatenated — so user input can never alter the
// JSONB path or inject SQL. Equality filters use the @> containment operator,
// which is served by the entries(payload jsonb_path_ops) GIN index.
func buildWhere(f ListEntriesFilter) (string, []any, error) {
	clauses := []string{"tenant_id = $1", "content_type_id = $2"}
	args := []any{f.TenantID, f.ContentTypeID}
	bind := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	col, err := predicateColumn(f)
	if err != nil {
		return "", nil, err
	}

	// Editorial state is a first-class column predicate, not a payload filter —
	// it is bound here (before any caller-supplied filter) so a public delivery
	// path's "published only" constraint can never be shadowed or widened by a
	// crafted ?filter= expression.
	if f.Status != "" {
		clauses = append(clauses, "status = "+bind(f.Status))
	}
	// Locale is likewise a column predicate, for the same reason: a public
	// reader asks for one language, and that choice must not be expressible
	// through the payload filter grammar.
	if f.Locale != "" {
		clauses = append(clauses, "locale = "+bind(f.Locale))
	}
	if f.TranslationGroupID != uuid.Nil {
		clauses = append(clauses, "translation_group_id = "+bind(f.TranslationGroupID))
	}
	// Confinement to one author (migration 000027). A column predicate, bound
	// here alongside status and locale and BEFORE any caller-supplied filter, for
	// the same reason they are: it must not be shadowable or widenable by a
	// crafted ?filter= expression, and it must be in the query that produces the
	// COUNT as well as the one that produces the page.
	//
	// `created_by = $n` and not `IS NOT DISTINCT FROM`: a NULL author matches
	// nothing, which is the fail-closed answer and the one the partial index on
	// (tenant_id, content_type_id, created_by) is built for.
	if f.CreatedBy != nil {
		clauses = append(clauses, "created_by = "+bind(*f.CreatedBy))
	}
	// `q` (ADR-021) is a column predicate too, and belongs beside Status/Locale/
	// CreatedBy for the same reason they are: composable with `filter` rather
	// than a substitute for it (design point 4), so it is ANDed in alongside
	// them rather than folded into the Filters loop below, which speaks a
	// different grammar (one key/op/value per caller-named field). Each
	// whitespace-separated term becomes its own ILIKE clause, so a multi-term
	// `q` reads as AND — every term must appear somewhere in the column,
	// though not necessarily contiguously or in order.
	if terms := strings.Fields(f.Query); len(terms) > 0 {
		searchCol := predicateSearchColumn(f)
		for _, term := range terms {
			p := bind("%" + likeEscape(term) + "%")
			clauses = append(clauses, fmt.Sprintf("%s ILIKE %s", searchCol, p))
		}
	}

	for _, flt := range f.Filters {
		switch flt.Op {
		case OpEq, OpNeq:
			doc, err := containmentDoc(flt.Field, flt.Value)
			if err != nil {
				return "", nil, err
			}
			// Bind as json.RawMessage so pgx sends jsonb; @> then uses the
			// jsonb_path_ops GIN index on whichever copy col names (000010 for
			// payload, 000039 for published_payload).
			p := bind(json.RawMessage(doc))
			if flt.Op == OpEq {
				clauses = append(clauses, fmt.Sprintf("%s @> %s", col, p))
			} else {
				clauses = append(clauses, fmt.Sprintf("NOT (%s @> %s)", col, p))
			}
		case OpGt, OpGte, OpLt, OpLte:
			// The value is checked BEFORE it is bound, because the cast that
			// makes the comparison correct is also what turns a malformed value
			// into a Postgres cast error — a 500 on a list page, from a string
			// the caller typed. Same guard number has always had.
			if err := checkOrderedValue(flt.Field, flt.Value); err != nil {
				return "", nil, err
			}
			kp := bind(flt.Field.Key)
			vp := bind(flt.Value)
			clauses = append(clauses, fmt.Sprintf("%s %s %s",
				orderedExpr(col, kp, flt.Field.Type), sqlCmp(flt.Op), orderedValue(vp, flt.Field.Type)))
		case OpIn:
			kp := bind(flt.Field.Key)
			vp := bind(splitCSV(flt.Value))
			clauses = append(clauses, fmt.Sprintf("(%s ->> %s) = ANY(%s::text[])", col, kp, vp))
		case OpContains:
			kp := bind(flt.Field.Key)
			vp := bind("%" + flt.Value + "%")
			clauses = append(clauses, fmt.Sprintf("(%s ->> %s) ILIKE %s", col, kp, vp))
		case OpHas, OpNhas:
			// Set membership on a multi-valued field. The containment document
			// wraps the value in a one-element ARRAY, because containment at a
			// nested key requires like-for-like: {"tags":["ai"]} matches, the bare
			// scalar {"tags":"ai"} never does.
			doc, err := containmentDoc(flt.Field, flt.Value)
			if err != nil {
				return "", nil, err
			}
			p := bind(json.RawMessage(doc))
			if flt.Op == OpHas {
				clauses = append(clauses, fmt.Sprintf("%s @> %s", col, p))
			} else {
				// NOT(@>) is true for a row holding other values, for an empty
				// array, AND for a row with no such key at all. That last one is
				// the intended reading of "does not have this tag" but surprises
				// people, so: it is deliberate, and it is NOT the same as
				// `key IS NOT NULL AND NOT (@>)`. No three-valued-logic trap —
				// payload is JSONB NOT NULL DEFAULT '{}', so @> never yields NULL;
				// published_payload is NULL only where status <> 'published'
				// (000033's CHECK), and predicateColumn admits it only under
				// Status = published, so those rows are gone before this clause
				// could see a NULL.
				//
				// Not index-servable: a negation over containment is a scan by
				// nature, same as OpNeq above.
				clauses = append(clauses, fmt.Sprintf("NOT (%s @> %s)", col, p))
			}
		default:
			return "", nil, apperrors.New("CONTENT_FILTER_OP_INVALID", "unsupported filter operator", 400).
				WithDetails(map[string]any{"op": string(flt.Op)})
		}
	}
	return strings.Join(clauses, " AND "), args, nil
}

// orderedExpr renders the JSONB extraction for a field that is being ORDERED or
// RANGE-COMPARED, casting only where the field's text form is not its real
// order. orderedValue casts the bound literal to match; the two are separate
// functions used together, and a cast added to one without the other compares
// timestamptz against text, which Postgres refuses outright rather than
// silently — the failure is loud by construction.
//
//   - number — '10' sorts before '9' as text.
//   - datetime — RFC3339 carries an OFFSET, and validateScalar accepts any
//     valid one. "2026-08-01T09:00:00+08:00" and "2026-08-01T01:00:00Z" are the
//     same instant, but as text the first is the greater. Text comparison here
//     did not merely misorder: it answered range queries WRONGLY and silently,
//     which is why this cast is not an optimisation.
//
// date is deliberately NOT cast. "2006-01-02" is fixed-width and carries no
// offset, so its text order already IS its chronological order; a cast would
// buy nothing and would extend the malformed-data exposure below to a third
// type for free.
//
// The exposure, stated plainly: a cast raises SQLSTATE 22P02 against a stored
// value that does not parse, which surfaces as a 500 on a list page from data
// the operator cannot see. validateScalar enforces RFC3339 at write time, so
// the only way in is residual drift — a hand-repaired row, a restored backup.
// This is the SAME exposure `number` has carried since the filter grammar
// existed, on one more type; it is not a new class of risk. It cannot be closed
// with a DB CHECK: which format applies depends on the field's type, which
// lives in content_type_fields, and a CHECK constraint cannot join.
func orderedExpr(col, keyParam, fieldType string) string {
	switch fieldType {
	case domain.FieldTypeNumber:
		return fmt.Sprintf("(%s ->> %s)::numeric", col, keyParam)
	case domain.FieldTypeDateTime:
		return fmt.Sprintf("(%s ->> %s)::timestamptz", col, keyParam)
	default:
		return fmt.Sprintf("(%s ->> %s)", col, keyParam)
	}
}

// predicateColumn names the JSONB copy every caller-supplied predicate and
// ORDER BY in one ListEntries call reads: payload for the admin audience,
// published_payload when MatchPublished is set. One function for both the WHERE
// and the ORDER BY so the two can never disagree — a filter on the snapshot
// ordered by the working copy is the leak in a different coat.
//
// MatchPublished without Status = published is refused as a programming error,
// not a 4xx: no caller input can produce it, only a service that forgot the
// status override — and answering it would evaluate predicates over NULL (a
// draft) or a retained post-unpublish snapshot (000033), both copies nobody
// may observe through this flag.
func predicateColumn(f ListEntriesFilter) (string, error) {
	if !f.MatchPublished {
		return "payload", nil
	}
	if f.Status != domain.StatusPublished {
		return "", fmt.Errorf("entries list: MatchPublished requires Status = %q, got %q", domain.StatusPublished, f.Status)
	}
	return "published_payload", nil
}

// predicateSearchColumn names the plain-text column the `q` parameter
// (ADR-021) matches against — search_text for the admin audience,
// published_search_text when MatchPublished is set. A second function rather
// than a second branch inside predicateColumn because the two columns are
// independent of the JSONB copy predicateColumn names (payload vs.
// published_payload each have their OWN search_text sibling), but they must
// still split on the exact same flag: a `q` term confirming the presence of
// text in the copy the caller may not see through this audience would be the
// same leak predicateColumn's own split exists to prevent.
//
// Unlike predicateColumn this never errors: by the time buildWhere reaches
// the `q` clause it has already called predicateColumn once and returned any
// MatchPublished-without-Status-published error from that call, so this can
// simply mirror the same flag.
func predicateSearchColumn(f ListEntriesFilter) string {
	if f.MatchPublished {
		return "published_search_text"
	}
	return "search_text"
}

// orderedValue casts the bound comparison literal to match orderedExpr.
func orderedValue(valueParam, fieldType string) string {
	switch fieldType {
	case domain.FieldTypeNumber:
		return valueParam + "::numeric"
	case domain.FieldTypeDateTime:
		return valueParam + "::timestamptz"
	default:
		return valueParam
	}
}

// checkOrderedValue rejects a comparison value the cast would choke on, so the
// caller gets a 400 naming the field and the value instead of a 500 naming
// nothing.
//
// Equality is NOT routed through here, and that asymmetry is deliberate rather
// than an oversight: OpEq/OpNeq/OpIn/OpContains/OpHas run on the raw text via
// jsonb containment, which is what the entries(payload jsonb_path_ops) GIN
// index serves. Casting them would drop the index. The consequence is worth
// naming: for a datetime field, `=` remains SPELLING-sensitive — the two
// spellings above are one instant to `>=` and two distinct values to `=`.
// Range queries are what a booking list asks; exact-instant equality is not,
// and buying it would cost every equality filter its index.
func checkOrderedValue(f domain.Field, raw string) error {
	switch f.Type {
	case domain.FieldTypeNumber:
		if _, err := strconv.ParseFloat(raw, 64); err != nil {
			return apperrors.New("CONTENT_FILTER_VALUE_INVALID", "non-numeric value for numeric comparison", 400).
				WithDetails(map[string]any{"field": f.Key, "value": raw})
		}
	case domain.FieldTypeDateTime:
		if _, err := time.Parse(time.RFC3339, raw); err != nil {
			return apperrors.New("CONTENT_FILTER_VALUE_INVALID", "non-RFC3339 value for datetime comparison", 400).
				WithDetails(map[string]any{"field": f.Key, "value": raw})
		}
	}
	return nil
}

// cmpFor is the strict comparison that means "further along the page" for a
// sort direction: a descending page advances towards SMALLER values.
func cmpFor(desc bool) string {
	if desc {
		return "<"
	}
	return ">"
}

func sqlCmp(op Op) string {
	switch op {
	case OpGt:
		return ">"
	case OpGte:
		return ">="
	case OpLt:
		return "<"
	case OpLte:
		return "<="
	default:
		return "="
	}
}

// containmentDoc builds the {"key": value} JSON document for an @> filter,
// typing the value per the field definition so JSON types line up with what was
// stored (a number stays a number, a bool stays a bool).
func containmentDoc(f domain.Field, raw string) ([]byte, error) {
	v, err := typedValue(f, raw)
	if err != nil {
		return nil, err
	}
	if f.Multiple {
		// A one-element array, not the bare value. jsonb containment descends
		// like for like: '{"tags":["ai","ml"]}' @> '{"tags":["ai"]}' is true,
		// while the same document against '{"tags":"ai"}' is FALSE — the
		// array-contains-scalar shorthand only applies at the top level, never at
		// a nested key. Getting this wrong returns zero rows silently.
		return json.Marshal(map[string]any{f.Key: []any{v}})
	}
	return json.Marshal(map[string]any{f.Key: v})
}

func typedValue(f domain.Field, raw string) (any, error) {
	switch f.Type {
	case domain.FieldTypeNumber:
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, apperrors.New("CONTENT_FILTER_VALUE_INVALID", "non-numeric value", 400).
				WithDetails(map[string]any{"field": f.Key, "value": raw})
		}
		return n, nil
	case domain.FieldTypeBoolean:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, apperrors.New("CONTENT_FILTER_VALUE_INVALID", "non-boolean value", 400).
				WithDetails(map[string]any{"field": f.Key, "value": raw})
		}
		return b, nil
	default:
		return raw, nil
	}
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

var _ ContentRepository = (*PostgresContentRepository)(nil)

// AddDeliveryReads folds an aggregated read count into the tenant's daily
// bucket. Called by the flusher once per tenant per interval — never per
// request (migration 000017 explains the trade-off).
func (r *PostgresContentRepository) AddDeliveryReads(ctx context.Context, tenantID string, day time.Time, n int64) error {
	if n <= 0 {
		return nil
	}
	return r.withTenant(ctx, tenantID, func(q querier) error {
		if _, err := q.Exec(ctx, `
			INSERT INTO content_delivery_usage (tenant_id, day, reads, updated_at)
			VALUES ($1, $2, $3, NOW())
			ON CONFLICT (tenant_id, day)
			DO UPDATE SET reads = content_delivery_usage.reads + EXCLUDED.reads, updated_at = NOW()`,
			tenantID, day.UTC().Format("2006-01-02"), n,
		); err != nil {
			return fmt.Errorf("add delivery reads: %w", err)
		}
		return nil
	})
}

// DeliveryReadsForDay returns the tenant's recorded reads for one day. Absent
// row = 0 (a tenant with no public traffic).
func (r *PostgresContentRepository) DeliveryReadsForDay(ctx context.Context, tenantID string, day time.Time) (int64, error) {
	var n int64
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		return q.QueryRow(ctx, `
			SELECT COALESCE(SUM(reads), 0) FROM content_delivery_usage
			WHERE tenant_id = $1 AND day = $2`,
			tenantID, day.UTC().Format("2006-01-02"),
		).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("delivery reads: %w", err)
	}
	return n, nil
}

// --- media assets (ADR-005) --------------------------------------------------

// mediaAssetColumns is the one select list for media_assets, shared by every
// read path. Written once because the alternative has already cost this codebase
// a silent bug class: a column added to one statement and forgotten in another
// returns a zero value, which reads as data rather than as an error.
const mediaAssetColumns = `id, tenant_id, storage_key, content_type, size_bytes,
	uploaded_at, created_at, filename, alt_text, width_px, height_px`

func scanMediaAsset(row pgx.Row, a *domain.MediaAsset) error {
	return row.Scan(&a.ID, &a.TenantID, &a.StorageKey, &a.ContentType, &a.SizeBytes,
		&a.UploadedAt, &a.CreatedAt, &a.Filename, &a.AltText, &a.WidthPx, &a.HeightPx)
}

func (r *PostgresContentRepository) CreateMediaAsset(ctx context.Context, a *domain.MediaAsset) error {
	return r.withTenant(ctx, a.TenantID, func(q querier) error {
		// The declared metadata is written HERE, at reservation, rather than at
		// completion. MarkMediaUploaded's contract is "what actually landed, never
		// from the client"; carrying a client claim through it would make that
		// sentence false. Reservation is also the only moment the client is
		// actually holding the file, so it is when it knows the name and the size
		// of the picture.
		if _, err := q.Exec(ctx, `
			INSERT INTO media_assets (id, tenant_id, storage_key, content_type, size_bytes, uploaded_at, created_at,
			                          filename, alt_text, width_px, height_px)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			a.ID, a.TenantID, a.StorageKey, a.ContentType, a.SizeBytes, a.UploadedAt, a.CreatedAt,
			a.Filename, a.AltText, a.WidthPx, a.HeightPx,
		); err != nil {
			return fmt.Errorf("insert media_asset: %w", err)
		}
		return nil
	})
}

func (r *PostgresContentRepository) GetMediaAsset(ctx context.Context, tenantID string, id uuid.UUID) (*domain.MediaAsset, error) {
	var a domain.MediaAsset
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		return scanMediaAsset(q.QueryRow(ctx, `
			SELECT `+mediaAssetColumns+`
			FROM media_assets WHERE tenant_id = $1 AND id = $2`, tenantID, id), &a)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperrors.ErrNotFound
		}
		return nil, fmt.Errorf("media_asset scan: %w", err)
	}
	return &a, nil
}

// likeEscape backslash-escapes the two characters ILIKE treats as wildcards,
// so a caller's literal `%` or `_` in a search term matches itself rather than
// "zero or more characters" or "any one character". Backslash is escaped
// first, so the escaping added for `%` and `_` is not itself re-escaped.
// Postgres's default LIKE/ILIKE escape character is already backslash, so no
// ESCAPE clause is needed alongside it.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// ListMediaAssets is the admin media library. Like ListEntries it runs the
// COUNT(*) and the page inside one tenant-scoped transaction so RLS applies to
// both and they see a consistent snapshot; unlike ListEntries there is no sort
// key or keyset mode to carry — this list is always newest-first, offset-paged.
func (r *PostgresContentRepository) ListMediaAssets(ctx context.Context, tenantID string, f MediaListFilter) ([]*domain.MediaAsset, int, error) {
	limit := f.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	// The library lists uploaded assets only — a row with no bytes behind it
	// is not content yet — EXCEPT under Orphan=true, where overdue
	// reservations are sweep candidates: an uncompleted reservation names no
	// link table, so it satisfies the NOT EXISTS definition trivially. Fresh
	// ones surface here too but are excluded by the sweep's age filter, not
	// by this list. memRepo.ListMediaAssets mirrors this exactly.
	where := "tenant_id = $1"
	if !f.Orphan {
		where += " AND uploaded_at IS NOT NULL"
	}
	args := []any{tenantID}
	bind := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }

	if q := strings.TrimSpace(f.Query); q != "" {
		where += fmt.Sprintf(" AND filename ILIKE %s", bind("%"+likeEscape(q)+"%"))
	}
	if f.ContentTypePrefix != "" {
		where += fmt.Sprintf(" AND content_type LIKE %s", bind(f.ContentTypePrefix+"%"))
	}
	if f.Orphan {
		// NOT EXISTS against BOTH link tables: an asset referenced only by a
		// draft (entry_media) or only by a stale published snapshot
		// (entry_media_published, retained across retract since ADR-014
		// §5.1/000033) is still referenced and must not show up here — ADR-005's
		// "orphan asset" is one neither table names at all.
		where += fmt.Sprintf(`
			AND NOT EXISTS (SELECT 1 FROM entry_media em WHERE em.tenant_id = %[1]s AND em.asset_id = media_assets.id)
			AND NOT EXISTS (SELECT 1 FROM entry_media_published emp WHERE emp.tenant_id = %[1]s AND emp.asset_id = media_assets.id)`,
			"$1")
	}

	var (
		items []*domain.MediaAsset
		total int
	)
	if err := r.withTenant(ctx, tenantID, func(q querier) error {
		if err := q.QueryRow(ctx, "SELECT COUNT(*) FROM media_assets WHERE "+where, args...).Scan(&total); err != nil {
			return fmt.Errorf("media_assets count: %w", err)
		}
		selArgs := append([]any{}, args...)
		selBind := func(v any) string { selArgs = append(selArgs, v); return fmt.Sprintf("$%d", len(selArgs)) }
		lp := selBind(limit)
		op := selBind(offset)
		rows, err := q.Query(ctx, "SELECT "+mediaAssetColumns+
			" FROM media_assets WHERE "+where+
			" ORDER BY created_at DESC, id DESC LIMIT "+lp+" OFFSET "+op, selArgs...)
		if err != nil {
			return fmt.Errorf("media_assets list: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var a domain.MediaAsset
			if err := scanMediaAsset(rows, &a); err != nil {
				return fmt.Errorf("media_asset scan: %w", err)
			}
			items = append(items, &a)
		}
		return rows.Err()
	}); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// UpdateMediaAssetMetadata writes only the fields the patch actually names.
//
// The SET list is built from the flags rather than assigning all four columns
// every time. That is not an optimisation: assigning all four would make a PATCH
// that mentions alt_text blank a filename it never saw, which is the lost-update
// failure MediaAssetPatch exists to prevent. Column names come from this
// function's own literals and never from caller input; only values are bound.
func (r *PostgresContentRepository) UpdateMediaAssetMetadata(ctx context.Context, tenantID string, id uuid.UUID, p MediaAssetPatch) (*domain.MediaAsset, error) {
	// An empty patch is a legitimate request — a client that diffed its state and
	// found nothing to send is behaving correctly — so it reads back the current
	// row instead of issuing a SET-less UPDATE, which is a syntax error.
	if p.IsEmpty() {
		return r.GetMediaAsset(ctx, tenantID, id)
	}
	args := []any{tenantID, id}
	var sets []string
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if p.SetFilename {
		add("filename", p.Filename)
	}
	if p.SetAltText {
		add("alt_text", p.AltText)
	}
	if p.SetDimensions {
		add("width_px", p.WidthPx)
		add("height_px", p.HeightPx)
	}

	var a domain.MediaAsset
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		return scanMediaAsset(q.QueryRow(ctx, `
			UPDATE media_assets SET `+strings.Join(sets, ", ")+`
			WHERE tenant_id = $1 AND id = $2
			RETURNING `+mediaAssetColumns, args...), &a)
	})
	if err != nil {
		// No row came back: RLS or the id. Either way the caller has no such asset,
		// and RETURNING is what lets one statement both write and prove it wrote.
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperrors.ErrNotFound
		}
		return nil, fmt.Errorf("update media_asset metadata: %w", err)
	}
	return &a, nil
}

// MarkMediaUploaded records what ACTUALLY landed in storage — size and content
// type come from a Stat against the bucket, never from the client.
func (r *PostgresContentRepository) MarkMediaUploaded(ctx context.Context, tenantID string, id uuid.UUID, size int64, contentType string) error {
	return r.withTenant(ctx, tenantID, func(q querier) error {
		tag, err := q.Exec(ctx, `
			UPDATE media_assets SET uploaded_at = NOW(), size_bytes = $3, content_type = $4
			WHERE tenant_id = $1 AND id = $2`, tenantID, id, size, contentType)
		if err != nil {
			return fmt.Errorf("mark uploaded: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return apperrors.ErrNotFound
		}
		return nil
	})
}

func (r *PostgresContentRepository) DeleteMediaAsset(ctx context.Context, tenantID string, id uuid.UUID) ([]string, error) {
	var keys []string
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		// Variant objects to remove, gathered in the SAME transaction as the
		// delete and FOR UPDATE: if the transform worker is mid-write on this
		// asset it holds these rows locked, and waiting here means the keys it
		// commits are the keys we return. Listing them from outside the
		// transaction would race the worker and leak its freshly written
		// object. The `original` row's key is the asset's own, deleted by the
		// caller anyway.
		rows, err := q.Query(ctx, `
			SELECT storage_key FROM media_variants
			WHERE tenant_id = $1 AND asset_id = $2 AND preset <> $3 AND storage_key IS NOT NULL
			FOR UPDATE`, tenantID, id, domain.MediaPresetOriginal)
		if err != nil {
			return fmt.Errorf("list variant keys: %w", err)
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, k)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		tag, err := q.Exec(ctx, `DELETE FROM media_assets WHERE tenant_id = $1 AND id = $2`, tenantID, id)
		if err != nil {
			return fmt.Errorf("delete media_asset: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return apperrors.ErrNotFound
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// ReplaceEntryMedia rewrites the entry's links in one transaction: delete-then-
// insert rather than diffing, because the payload is the whole truth and the
// link set is tiny.
func (r *PostgresContentRepository) ReplaceEntryMedia(ctx context.Context, tenantID string, entryID uuid.UUID, assetIDs []uuid.UUID) error {
	return r.withTenant(ctx, tenantID, func(q querier) error {
		if _, err := q.Exec(ctx, `DELETE FROM entry_media WHERE tenant_id = $1 AND entry_id = $2`, tenantID, entryID); err != nil {
			return fmt.Errorf("clear entry_media: %w", err)
		}
		for _, id := range assetIDs {
			if _, err := q.Exec(ctx, `
				INSERT INTO entry_media (entry_id, asset_id, tenant_id) VALUES ($1, $2, $3)
				ON CONFLICT DO NOTHING`, entryID, id, tenantID); err != nil {
				return fmt.Errorf("link entry_media: %w", err)
			}
		}
		return nil
	})
}

// ReplaceEntryRelations rewrites the entry's reverse-reference links in one
// transaction, mirroring ReplaceEntryMedia exactly: delete-then-insert, since
// the validated payload is the whole truth and the link set is tiny. Two refs
// naming the same target through different fields ("author" and "reviewer"
// both pointing at one person) are two distinct rows — see 000050's header —
// so nothing here dedupes by target alone.
func (r *PostgresContentRepository) ReplaceEntryRelations(ctx context.Context, tenantID string, entryID uuid.UUID, refs []EntryRelationRef) error {
	return r.withTenant(ctx, tenantID, func(q querier) error {
		if _, err := q.Exec(ctx, `DELETE FROM entry_relations WHERE tenant_id = $1 AND entry_id = $2`, tenantID, entryID); err != nil {
			return fmt.Errorf("clear entry_relations: %w", err)
		}
		for _, ref := range refs {
			if _, err := q.Exec(ctx, `
				INSERT INTO entry_relations (entry_id, field_path, target_entry_id, tenant_id) VALUES ($1, $2, $3, $4)
				ON CONFLICT DO NOTHING`, entryID, ref.FieldPath, ref.TargetID, tenantID); err != nil {
				return fmt.Errorf("link entry_relations: %w", err)
			}
		}
		return nil
	})
}

// referencedByQuery is EntryReferencedBy and MediaReferencedBy's shared shape:
// union the draft and published link tables, merge to one row per (referring
// entry [, field]) with bool_or across the two sources, join entries/
// content_types for the type name and title fields, and page the result. The
// two callers differ only in which link tables and which match column they
// scan, so the SQL lives once here rather than twice with a chance to drift.
//
// draftTable/publishedTable are both a fixed pair of literals from this
// file's own two call sites, never caller input, so building the query
// string with fmt.Sprintf carries no injection risk despite not being a bind
// parameter.
func (r *PostgresContentRepository) referencedByQuery(
	ctx context.Context, tenantID string, draftTable, publishedTable, matchColumn string, matchValue any,
	includeFieldPath bool, limit, offset int,
) (ReferencedByResult, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	fieldPathSelect := "''"
	if includeFieldPath {
		fieldPathSelect = "field_path"
	}
	// field_path is always in the GROUP BY, even when includeFieldPath is
	// false (MediaReferencedBy): it is still in the SELECT list as the '' AS
	// field_path literal from the links CTE, and Postgres has no way to know
	// that literal is constant per entry_id once it has passed through a CTE
	// column — it demands the same functional-dependency proof for a literal
	// as for a real column. Grouping by it too is a no-op for MediaReferencedBy
	// (every row for a given entry_id already carries the identical '' value,
	// so it can never split a group), and is exactly what EntryReferencedBy
	// already needs.
	fieldPathGroup := ", field_path"
	q := fmt.Sprintf(`
		WITH links AS (
			SELECT entry_id, %[1]s AS field_path, TRUE AS draft, FALSE AS published
			FROM %[2]s WHERE tenant_id = $1 AND %[3]s = $2
			UNION ALL
			SELECT entry_id, %[1]s AS field_path, FALSE AS draft, TRUE AS published
			FROM %[4]s WHERE tenant_id = $1 AND %[3]s = $2
		),
		merged AS (
			SELECT entry_id, field_path, bool_or(draft) AS draft, bool_or(published) AS published
			FROM links
			GROUP BY entry_id%[5]s
		)
		SELECT m.entry_id, e.content_type_id, ct.name, e.payload, e.published_payload,
		       m.field_path, m.draft, m.published, COUNT(*) OVER () AS total
		FROM merged m
		JOIN entries e ON e.id = m.entry_id
		JOIN content_types ct ON ct.id = e.content_type_id
		ORDER BY m.entry_id, m.field_path
		LIMIT $3 OFFSET $4`,
		fieldPathSelect, draftTable, matchColumn, publishedTable, fieldPathGroup)

	var out ReferencedByResult
	type rawRow struct {
		row              ReferencedByRow
		payload, pubJSON []byte
	}
	var raws []rawRow
	err := r.withTenant(ctx, tenantID, func(qr querier) error {
		rows, err := qr.Query(ctx, q, tenantID, matchValue, limit, offset)
		if err != nil {
			return fmt.Errorf("referenced-by query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var rr rawRow
			if err := rows.Scan(&rr.row.EntryID, &rr.row.ContentTypeID, &rr.row.TypeName, &rr.payload, &rr.pubJSON,
				&rr.row.FieldPath, &rr.row.Draft, &rr.row.Published, &out.Total); err != nil {
				return fmt.Errorf("referenced-by scan: %w", err)
			}
			raws = append(raws, rr)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		// One query for every distinct type on the page — never one per row —
		// the same batching loadFieldsByType exists for.
		ids := make([]uuid.UUID, 0, len(raws))
		seen := map[uuid.UUID]bool{}
		for _, rr := range raws {
			if !seen[rr.row.ContentTypeID] {
				seen[rr.row.ContentTypeID] = true
				ids = append(ids, rr.row.ContentTypeID)
			}
		}
		fieldsByType, ferr := loadFieldsByTypeWith(ctx, qr, ids)
		if ferr != nil {
			return ferr
		}
		for _, rr := range raws {
			// Title prefers the WORKING copy — the admin's normal frame of
			// reference for "what is this entry" — falling back to the
			// published snapshot for a row this lookup found only through
			// entry_relations_published/entry_media_published (draft = FALSE),
			// where the working payload may since have moved on or never
			// existed at all in the caller's view.
			raw := rr.payload
			if !rr.row.Draft && len(rr.pubJSON) > 0 {
				raw = rr.pubJSON
			}
			rr.row.Title = domain.TitleFor(fieldsByType[rr.row.ContentTypeID], raw)
			out.Items = append(out.Items, rr.row)
		}
		return nil
	})
	if err != nil {
		return ReferencedByResult{}, err
	}
	return out, nil
}

// EntryReferencedBy is entry_relations' reverse lookup.
func (r *PostgresContentRepository) EntryReferencedBy(ctx context.Context, tenantID string, targetID uuid.UUID, limit, offset int) (ReferencedByResult, error) {
	return r.referencedByQuery(ctx, tenantID, "entry_relations", "entry_relations_published", "target_entry_id", targetID, true, limit, offset)
}

// MediaReferencedBy is entry_media's reverse lookup. field_path is always ""
// on every row: entry_media carries no per-field column (000019 predates
// entry_relations), so this cannot say WHICH field references the asset,
// only which entries do — see ADR-024's known limitations.
func (r *PostgresContentRepository) MediaReferencedBy(ctx context.Context, tenantID string, assetID uuid.UUID, limit, offset int) (ReferencedByResult, error) {
	return r.referencedByQuery(ctx, tenantID, "entry_media", "entry_media_published", "asset_id", assetID, false, limit, offset)
}

// AssetIsPublished is the gate the delivery path depends on: bytes are readable
// only while something published points at them. Unpublishing the last
// referencing entry therefore revokes access as soon as the signed URL expires.
//
// It reads entry_media_published — the references of the PUBLISHED SNAPSHOT —
// not entry_media, which tracks the working copy. Using the working copy would
// mean removing an image from a draft instantly revokes bytes that the live
// snapshot still renders, breaking a public page on an unsaved-to-public edit
// (ADR-006).
func (r *PostgresContentRepository) AssetIsPublished(ctx context.Context, tenantID string, assetID uuid.UUID) (bool, error) {
	var ok bool
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		return q.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM entry_media_published emp
				JOIN entries e ON e.id = emp.entry_id
				WHERE emp.tenant_id = $1 AND emp.asset_id = $2 AND e.status = $3
			)`, tenantID, assetID, domain.StatusPublished).Scan(&ok)
	})
	if err != nil {
		return false, fmt.Errorf("asset published check: %w", err)
	}
	return ok, nil
}
