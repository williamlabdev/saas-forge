package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Reusable components (ADR-020). See migration 000044 for the storage shape:
// content_components holds the definitions under RLS; sub-fields reuse
// content_type_fields with component_id set; a referencing field carries
// ref_component_id. Values never leave entries.payload.

const componentColumns = `id, tenant_id, name, label, created_at, updated_at`

func (r *PostgresContentRepository) CreateComponent(ctx context.Context, c *domain.Component) error {
	return r.withTenant(ctx, c.TenantID, func(q querier) error {
		if _, err := q.Exec(ctx, `
			INSERT INTO content_components (`+componentColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			c.ID, c.TenantID, c.Name, c.Label, c.CreatedAt, c.UpdatedAt,
		); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
				return errComponentExists(c.Name)
			}
			return fmt.Errorf("insert content_component: %w", err)
		}
		for i := range c.Fields {
			if err := insertComponentField(ctx, q, c.ID, &c.Fields[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

func errComponentExists(name string) error {
	return apperrors.New("CONTENT_COMPONENT_EXISTS", "component already exists", 409).
		WithDetails(map[string]any{"name": name})
}

func (r *PostgresContentRepository) GetComponentByName(ctx context.Context, tenantID, name string) (*domain.Component, error) {
	return r.getComponent(ctx, tenantID, `name = $2`, name)
}

func (r *PostgresContentRepository) GetComponentByID(ctx context.Context, tenantID string, id uuid.UUID) (*domain.Component, error) {
	return r.getComponent(ctx, tenantID, `id = $2`, id)
}

func (r *PostgresContentRepository) getComponent(ctx context.Context, tenantID, pred string, arg any) (*domain.Component, error) {
	var c domain.Component
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		return q.QueryRow(ctx, `
			SELECT `+componentColumns+`
			FROM content_components
			WHERE tenant_id = $1 AND `+pred, tenantID, arg,
		).Scan(&c.ID, &c.TenantID, &c.Name, &c.Label, &c.CreatedAt, &c.UpdatedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperrors.ErrNotFound
		}
		return nil, fmt.Errorf("content_component scan: %w", err)
	}
	subs, err := r.loadComponentFields(ctx, []uuid.UUID{c.ID})
	if err != nil {
		return nil, err
	}
	c.Fields = subs[c.ID]
	return &c, nil
}

func (r *PostgresContentRepository) ListComponents(ctx context.Context, tenantID string) ([]*domain.Component, error) {
	var out []*domain.Component
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT `+componentColumns+`
			FROM content_components
			WHERE tenant_id = $1
			ORDER BY name ASC`, tenantID)
		if err != nil {
			return fmt.Errorf("content_components list: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var c domain.Component
			if err := rows.Scan(&c.ID, &c.TenantID, &c.Name, &c.Label, &c.CreatedAt, &c.UpdatedAt); err != nil {
				return fmt.Errorf("content_components scan: %w", err)
			}
			out = append(out, &c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(out))
	for i, c := range out {
		ids[i] = c.ID
	}
	subs, err := r.loadComponentFields(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, c := range out {
		c.Fields = subs[c.ID]
	}
	return out, nil
}

func (r *PostgresContentRepository) CountComponents(ctx context.Context, tenantID string) (int, error) {
	var n int
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		return q.QueryRow(ctx, `SELECT COUNT(*) FROM content_components WHERE tenant_id = $1`, tenantID).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("count content_components: %w", err)
	}
	return n, nil
}

// loadComponentFields is loadFieldsByType for sub-field rows, keyed by the
// owning component. Same column order, same scanner, same total ORDER BY.
func (r *PostgresContentRepository) loadComponentFields(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]domain.Field, error) {
	return loadComponentFieldsWith(ctx, r.pool, ids)
}

// loadComponentFieldsWith is loadComponentFields on a caller-chosen connection.
func loadComponentFieldsWith(ctx context.Context, q querier, ids []uuid.UUID) (map[uuid.UUID][]domain.Field, error) {
	out := make(map[uuid.UUID][]domain.Field, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		SELECT f.id, f.content_type_id, f.key, f.field_type, f.label, f.required, f.multiple, f.enum_values,
		       f.read_roles, f.write_roles, f.relation_entity, f.ordinal, f.created_at,
		       f.is_unique, f.format, f.pattern, f.min_value, f.max_value, f.description,
		       f.ref_component_id, '', f.component_id
		FROM content_type_fields f
		WHERE f.component_id = ANY($1::uuid[])
		ORDER BY f.ordinal ASC, f.created_at ASC, f.key ASC`, ids)
	if err != nil {
		return nil, fmt.Errorf("component fields list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		f, owner, err := scanComponentField(rows)
		if err != nil {
			return nil, err
		}
		out[owner] = append(out[owner], f)
	}
	return out, rows.Err()
}

func scanComponentField(row fieldScanner) (domain.Field, uuid.UUID, error) {
	var f domain.Field
	var ctID uuid.NullUUID
	var owner uuid.UUID
	if err := row.Scan(&f.ID, &ctID, &f.Key, &f.Type, &f.Label, &f.Required, &f.Multiple, &f.EnumValues,
		&f.ReadRoles, &f.WriteRoles, &f.RelationEntity, &f.Ordinal, &f.CreatedAt,
		&f.Unique, &f.Format, &f.Pattern, &f.Min, &f.Max, &f.Description,
		&f.ComponentID, &f.ComponentName, &owner); err != nil {
		return domain.Field{}, uuid.Nil, fmt.Errorf("component field scan: %w", err)
	}
	return f, owner, nil
}

func (r *PostgresContentRepository) UpdateComponentDefinition(ctx context.Context, tenantID string, c *domain.Component, now time.Time) error {
	return r.withTenant(ctx, tenantID, func(q querier) error {
		tag, err := q.Exec(ctx,
			`UPDATE content_components SET label = $3, updated_at = $4 WHERE tenant_id = $1 AND id = $2`,
			tenantID, c.ID, c.Label, now)
		if err != nil {
			return fmt.Errorf("update content_component: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return apperrors.ErrNotFound
		}
		return nil
	})
}

func (r *PostgresContentRepository) DeleteComponent(ctx context.Context, tenantID string, id uuid.UUID) error {
	return r.withTenant(ctx, tenantID, func(q querier) error {
		// The zone half of "still referenced", checked HERE and not only in
		// the service, because a zone names its component in a TEXT[] and
		// therefore has no FK to be the second net (000045). The service's own
		// check reads referrers in one transaction and deletes in another; a
		// zone created in between would be left naming a component that no
		// longer exists, and every item in it would fail validation with no
		// definition to point at. Same code and same shape as the service's
		// refusal, so a client cannot tell which layer caught it.
		var users []string
		if err := q.QueryRow(ctx, `
			SELECT COALESCE(array_agg(DISTINCT ct.name ORDER BY ct.name), '{}')
			FROM content_type_fields f
			JOIN content_types ct ON ct.id = f.content_type_id
			JOIN content_components c ON c.id = $2 AND c.tenant_id = $1
			WHERE ct.tenant_id = $1
			  AND f.field_type = 'dynamiczone'
			  AND c.name = ANY(f.zone_components)`, tenantID, id).Scan(&users); err != nil {
			return fmt.Errorf("zone referrers of component: %w", err)
		}
		if len(users) > 0 {
			return apperrors.New("CONTENT_COMPONENT_IN_USE", "component is still used by content types", 409).
				WithDetails(map[string]any{"used_by": users})
		}
		// Sub-fields first: the FK is RESTRICT on purpose (000044), so the
		// definition row cannot go while anything points at it — including
		// its own sub-fields. A referencing type field, which the service
		// already refused on, would surface here as a foreign-key error rather
		// than a cascade.
		if _, err := q.Exec(ctx, `DELETE FROM content_type_fields WHERE component_id = $1`, id); err != nil {
			return fmt.Errorf("delete component fields: %w", err)
		}
		tag, err := q.Exec(ctx, `DELETE FROM content_components WHERE tenant_id = $1 AND id = $2`, tenantID, id)
		if err != nil {
			return fmt.Errorf("delete content_component: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return apperrors.ErrNotFound
		}
		return nil
	})
}

func (r *PostgresContentRepository) ListComponentReferrers(ctx context.Context, tenantID string, componentID uuid.UUID) ([]ComponentRef, error) {
	var out []ComponentRef
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		// The join through content_types is the tenant scope, as everywhere
		// else over content_type_fields (000014).
		// Two halves, because a component is reachable two ways: a `component`
		// field points at it by ID (an FK the database enforces), and a
		// dynamiczone field names it in an allowed-list of TEXT (no FK — see
		// 000045 for why). Both are referrers in every sense the callers care
		// about, so both come back from one query: used_by lists them, the
		// delete guard counts them, and every sub-field rewrite fans out to
		// them. Splitting this into two calls would mean four call sites each
		// having to remember the second one.
		rows, err := q.Query(ctx, `
			SELECT ct.id, ct.name, f.key, f.multiple, FALSE AS zone
			FROM content_type_fields f
			JOIN content_types ct ON ct.id = f.content_type_id
			WHERE ct.tenant_id = $1 AND f.ref_component_id = $2
			UNION ALL
			SELECT ct.id, ct.name, f.key, f.multiple, TRUE AS zone
			FROM content_type_fields f
			JOIN content_types ct ON ct.id = f.content_type_id
			JOIN content_components c ON c.id = $2 AND c.tenant_id = $1
			WHERE ct.tenant_id = $1
			  AND f.field_type = 'dynamiczone'
			  AND c.name = ANY(f.zone_components)
			ORDER BY 2, 3`,
			tenantID, componentID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ref ComponentRef
			if err := rows.Scan(&ref.TypeID, &ref.TypeName, &ref.FieldKey, &ref.Multiple, &ref.Zone); err != nil {
				return err
			}
			out = append(out, ref)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list component referrers: %w", err)
	}
	return out, nil
}

func (r *PostgresContentRepository) AddComponentField(ctx context.Context, tenantID string, componentID uuid.UUID, f *domain.Field) error {
	return r.withTenant(ctx, tenantID, func(q querier) error {
		if err := insertComponentField(ctx, q, componentID, f); err != nil {
			return err
		}
		return touchComponent(ctx, q, tenantID, componentID, f.CreatedAt)
	})
}

// touchComponent is touchContentType for content_components. The tenant is
// in the WHERE for the reason AddField gives: FORCE RLS turns a missing
// tenant into a silent zero-row success.
func touchComponent(ctx context.Context, q execer, tenantID string, id uuid.UUID, now time.Time) error {
	tag, err := q.Exec(ctx,
		`UPDATE content_components SET updated_at = $3 WHERE tenant_id = $1 AND id = $2`, tenantID, id, now)
	if err != nil {
		return fmt.Errorf("touch content_component: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return apperrors.ErrNotFound
	}
	return nil
}

func (r *PostgresContentRepository) UpdateComponentFieldDefinition(ctx context.Context, tenantID string, c *domain.Component, f domain.Field, now time.Time) error {
	return r.withTenant(ctx, tenantID, func(q querier) error {
		// The mutable set is what UpdateFieldDefinition persists minus the
		// attributes a sub-field cannot carry (unique, roles) — the service
		// refused those upstream; writing them here anyway would be a second
		// place for the rule to drift.
		tag, err := q.Exec(ctx, `
			UPDATE content_type_fields
			SET label = $3, required = $4, enum_values = $5,
			    format = $6, pattern = $7, min_value = $8, max_value = $9,
			    description = $10
			WHERE component_id = $1 AND key = $2`,
			c.ID, f.Key, f.Label, f.Required, f.EnumValues,
			f.Format, f.Pattern, f.Min, f.Max, f.Description)
		if err != nil {
			return fmt.Errorf("update component field: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return apperrors.ErrNotFound
		}
		return touchComponent(ctx, q, tenantID, c.ID, now)
	})
}

func (r *PostgresContentRepository) RenameComponentField(ctx context.Context, tenantID string, c *domain.Component, refs []ComponentRef, oldKey, newKey string, actor domain.WriteActor, now time.Time) error {
	return r.withTenant(ctx, tenantID, func(q querier) error {
		for _, ref := range refs {
			if err := rewriteComponentItems(ctx, q, tenantID, ref, c.Name, oldKey, &newKey, actor, now); err != nil {
				return err
			}
		}
		tag, err := q.Exec(ctx,
			`UPDATE content_type_fields SET key = $3 WHERE component_id = $1 AND key = $2`,
			c.ID, oldKey, newKey)
		if err != nil {
			return fieldInsertError(err, newKey)
		}
		if tag.RowsAffected() == 0 {
			return apperrors.ErrNotFound
		}
		if err := touchReferrers(ctx, q, tenantID, refs, now); err != nil {
			return err
		}
		return touchComponent(ctx, q, tenantID, c.ID, now)
	})
}

func (r *PostgresContentRepository) DeleteComponentField(ctx context.Context, tenantID string, c *domain.Component, refs []ComponentRef, key string, actor domain.WriteActor, now time.Time) error {
	return r.withTenant(ctx, tenantID, func(q querier) error {
		for _, ref := range refs {
			if err := rewriteComponentItems(ctx, q, tenantID, ref, c.Name, key, nil, actor, now); err != nil {
				return err
			}
		}
		tag, err := q.Exec(ctx,
			`DELETE FROM content_type_fields WHERE component_id = $1 AND key = $2`, c.ID, key)
		if err != nil {
			return fmt.Errorf("delete component field: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return apperrors.ErrNotFound
		}
		// A file or richtext sub-field links media exactly as a top-level one
		// does, so its delete re-syncs entry_media the same way — through the
		// same rebuild, on every referring type, inside this transaction. The
		// survivor set is read back AFTER the row delete above so the loader
		// hands over each type as it now stands, sub-field gone.
		if sf, ok := c.FieldByKey(key); ok && linksMedia(sf) && len(refs) > 0 {
			ids := make([]uuid.UUID, 0, len(refs))
			for _, ref := range refs {
				ids = append(ids, ref.TypeID)
			}
			byType, err := loadFieldsByTypeWith(ctx, q, ids)
			if err != nil {
				return err
			}
			for _, id := range ids {
				if err := relinkEntryMedia(ctx, q, tenantID, id, byType[id]); err != nil {
					return err
				}
			}
		}
		if err := touchReferrers(ctx, q, tenantID, refs, now); err != nil {
			return err
		}
		return touchComponent(ctx, q, tenantID, c.ID, now)
	})
}

// rewriteComponentItems is RenameField / DeleteField for the INSIDE of a
// component value: one set-based statement over every entry of one referring
// type, renaming (newKey set) or dropping (newKey nil) subKey in every item
// under ref.FieldKey, in both payload copies. The helpers it calls are
// defined in migration 000044; the CASE guards, the `working` CTE and the
// entry_revisions insert are the same as the top-level verbs, for the same
// reasons DeleteField spells out (per-copy version bumps, provenance only
// where the working copy changed, revisions keyed by version).
func rewriteComponentItems(ctx context.Context, q execer, tenantID string, ref ComponentRef,
	componentName, subKey string, newKey *string, actor domain.WriteActor, now time.Time) error {
	// Which helper pair finds the items is the ONLY difference between a
	// `component` field and a dynamic zone here. A zone's items are tagged and
	// heterogeneous, so the zone helpers take the component name ($10) and act
	// on exactly the items that are that component — a `hero` sub-field rename
	// must not touch a `cta` item that happens to share the key. Everything
	// else — the CASE guards, the per-copy version bumps, the provenance, the
	// revision insert — is the same statement, on purpose: two copies of it
	// would be two places for the revision rules to drift.
	has, rewrite := "content_component_has_subkey(%s, $3, $4)", "content_component_rewrite_subkey(%s, $3, $4, $5::text)"
	args := []any{tenantID, ref.TypeID, ref.FieldKey, subKey, newKey, now, actor.UserID, actor.Kind, actor.AgentID}
	if ref.Zone {
		has, rewrite = "content_zone_has_subkey(%s, $3, $10, $4)", "content_zone_rewrite_subkey(%s, $3, $10, $4, $5::text)"
		args = append(args, componentName)
	}
	hasWorking := fmt.Sprintf(has, "payload")
	hasPublished := fmt.Sprintf(has, "published_payload")
	rewrittenPayload := fmt.Sprintf(rewrite, "payload")
	rewrittenPublished := fmt.Sprintf(rewrite, "published_payload")
	if _, err := q.Exec(ctx, `
		WITH working AS (
			SELECT id FROM entries
			WHERE tenant_id = $1 AND content_type_id = $2
			  AND `+hasWorking+`
		),
		rewritten AS (
			UPDATE entries
			SET payload = `+rewrittenPayload+`,
			    published_payload = `+rewrittenPublished+`,
			    -- search_text tracks the rewrite unconditionally, whether this call
			    -- renames a sub-key (newKey set — the extracted TEXT is unchanged,
			    -- only the JSON key is) or drops one (newKey nil — the extracted
			    -- text DOES change). Recomputing in both cases costs one function
			    -- call this statement already pays for building rewrittenPayload,
			    -- and keeps this one statement correct for both callers rather than
			    -- branching SQL on which one is running.
			    search_text = CASE WHEN `+hasWorking+`
			        THEN content_entry_search_text_approx(`+rewrittenPayload+`) ELSE search_text END,
			    published_search_text = CASE WHEN `+hasPublished+`
			        THEN content_entry_search_text_approx(`+rewrittenPublished+`) ELSE published_search_text END,
			    version = CASE WHEN `+hasWorking+` THEN version + 1 ELSE version END,
			    published_version = CASE WHEN `+hasPublished+` THEN published_version + 1 ELSE published_version END,
			    updated_at = $6,
			    updated_by = CASE WHEN `+hasWorking+` THEN $7::uuid ELSE updated_by END,
			    updated_by_kind = CASE WHEN `+hasWorking+` THEN $8::text ELSE updated_by_kind END,
			    updated_by_agent = CASE WHEN `+hasWorking+` THEN $9::text ELSE updated_by_agent END
			WHERE tenant_id = $1 AND content_type_id = $2
			  AND `+bothCopies(fmt.Sprintf(has, "{c}"))+`
			RETURNING id, version, tenant_id, payload,
			          updated_by_kind, updated_by, updated_by_agent, updated_at
		)
		INSERT INTO entry_revisions
			(entry_id, version, tenant_id, payload, author_kind, author_user_id, author_agent_id, created_at)
		SELECT rw.id, rw.version, rw.tenant_id, rw.payload,
		       rw.updated_by_kind, rw.updated_by, rw.updated_by_agent, rw.updated_at
		FROM rewritten rw JOIN working w ON w.id = rw.id`,
		args...,
	); err != nil {
		return fmt.Errorf("rewrite component items in %s.%s: %w", ref.TypeName, ref.FieldKey, err)
	}
	return nil
}

// RenameComponent is the verb dynamic zones made necessary (ADR-020 deferred
// it while a component's name was referenced only by an id-carrying FK). The
// name is now STORED DATA — in every zone item's __component and in every
// zone's allowed-list — so a rename is a data migration, and it runs in one
// transaction with the definition change.
//
// Revisions are NOT rewritten, consistent with the sub-field verbs: a revision
// is what the document looked like then, and rewriting history to match a
// definition that changed afterwards would make the audit trail a derived
// view of the present.
func (r *PostgresContentRepository) RenameComponent(ctx context.Context, tenantID string, c *domain.Component,
	refs []ComponentRef, newName string, actor domain.WriteActor, now time.Time) error {
	return r.withTenant(ctx, tenantID, func(q querier) error {
		for _, ref := range refs {
			if !ref.Zone {
				continue // a `component` field stores the id; nothing to rewrite
			}
			if err := rewriteZoneComponent(ctx, q, tenantID, ref, c.Name, newName, actor, now); err != nil {
				return err
			}
		}
		// Every allowed-list that names it, across the tenant. array_replace
		// preserves position, which is the order an editor's block menu offers
		// — a rename must not reshuffle the menu.
		if _, err := q.Exec(ctx, `
			UPDATE content_type_fields f
			SET zone_components = array_replace(f.zone_components, $2, $3)
			FROM content_types ct
			WHERE ct.id = f.content_type_id AND ct.tenant_id = $1
			  AND f.field_type = 'dynamiczone' AND $2 = ANY(f.zone_components)`,
			tenantID, c.Name, newName); err != nil {
			return fmt.Errorf("rewrite zone allowed-lists: %w", err)
		}
		tag, err := q.Exec(ctx,
			`UPDATE content_components SET name = $3, updated_at = $4 WHERE tenant_id = $1 AND id = $2`,
			tenantID, c.ID, newName, now)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
				return errComponentExists(newName)
			}
			return fmt.Errorf("rename content_component: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return apperrors.ErrNotFound
		}
		return touchReferrers(ctx, q, tenantID, refs, now)
	})
}

// rewriteZoneComponent retags every item of one zone field, in both copies,
// with the same version / provenance / revision rules rewriteComponentItems
// uses — the shape is shared deliberately, so "a schema change that rewrites
// entries" means one thing in this file.
func rewriteZoneComponent(ctx context.Context, q execer, tenantID string, ref ComponentRef,
	oldName, newName string, actor domain.WriteActor, now time.Time) error {
	// No search_text / published_search_text touch here, deliberately:
	// content_zone_rewrite_component only retags the __component discriminator
	// on matching zone items (ADR-020 Amendment 1 §2) — it does not add,
	// remove, or otherwise change any sub-field VALUE, so the plain text
	// domain.ExtractSearchText (or content_entry_search_text_approx) would
	// extract from this row is unchanged by the rename.
	if _, err := q.Exec(ctx, `
		WITH working AS (
			SELECT id FROM entries
			WHERE tenant_id = $1 AND content_type_id = $2
			  AND content_zone_has_component(payload, $3, $4)
		),
		rewritten AS (
			UPDATE entries
			SET payload = content_zone_rewrite_component(payload, $3, $4, $5),
			    published_payload = content_zone_rewrite_component(published_payload, $3, $4, $5),
			    version = CASE WHEN content_zone_has_component(payload, $3, $4) THEN version + 1 ELSE version END,
			    published_version = CASE WHEN content_zone_has_component(published_payload, $3, $4) THEN published_version + 1 ELSE published_version END,
			    updated_at = $6,
			    updated_by = CASE WHEN content_zone_has_component(payload, $3, $4) THEN $7::uuid ELSE updated_by END,
			    updated_by_kind = CASE WHEN content_zone_has_component(payload, $3, $4) THEN $8::text ELSE updated_by_kind END,
			    updated_by_agent = CASE WHEN content_zone_has_component(payload, $3, $4) THEN $9::text ELSE updated_by_agent END
			WHERE tenant_id = $1 AND content_type_id = $2
			  AND `+bothCopies("content_zone_has_component({c}, $3, $4)")+`
			RETURNING id, version, tenant_id, payload,
			          updated_by_kind, updated_by, updated_by_agent, updated_at
		)
		INSERT INTO entry_revisions
			(entry_id, version, tenant_id, payload, author_kind, author_user_id, author_agent_id, created_at)
		SELECT rw.id, rw.version, rw.tenant_id, rw.payload,
		       rw.updated_by_kind, rw.updated_by, rw.updated_by_agent, rw.updated_at
		FROM rewritten rw JOIN working w ON w.id = rw.id`,
		tenantID, ref.TypeID, ref.FieldKey, oldName, newName, now, actor.UserID, actor.Kind, actor.AgentID,
	); err != nil {
		return fmt.Errorf("retag zone items in %s.%s: %w", ref.TypeName, ref.FieldKey, err)
	}
	return nil
}

// touchReferrers moves updated_at on every referring type: their DTOs
// changed shape (the sub-field list a client renders came from the
// component), same reasoning as RenameContentType's touch of referrers.
func touchReferrers(ctx context.Context, q execer, tenantID string, refs []ComponentRef, now time.Time) error {
	for _, ref := range refs {
		if _, err := q.Exec(ctx,
			`UPDATE content_types SET updated_at = $3 WHERE tenant_id = $1 AND id = $2`,
			tenantID, ref.TypeID, now); err != nil {
			return fmt.Errorf("touch referring type: %w", err)
		}
	}
	return nil
}

func (r *PostgresContentRepository) CountEntriesWithComponentSubKey(ctx context.Context, tenantID string,
	ref ComponentRef, componentName, subKey string) (int, error) {
	if ref.Zone {
		return r.countEntries(ctx, tenantID, ref.TypeID,
			bothCopies("content_zone_has_subkey({c}, $3, $4, $5)"), ref.FieldKey, componentName, subKey)
	}
	return r.countEntries(ctx, tenantID, ref.TypeID,
		bothCopies("content_component_has_subkey({c}, $3, $4)"), ref.FieldKey, subKey)
}

func (r *PostgresContentRepository) CountEntriesWithZoneComponent(ctx context.Context, tenantID string,
	ref ComponentRef, componentName string) (int, error) {
	return r.countEntries(ctx, tenantID, ref.TypeID,
		bothCopies("content_zone_has_component({c}, $3, $4)"), ref.FieldKey, componentName)
}
