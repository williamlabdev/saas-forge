package repository

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/tenant/domain"
)

const uniqueViolation = "23505"

// slugAttempts bounds the retry on slug collision. With 160 bits of
// randomness a collision means something is broken, not unlucky.
const slugAttempts = 5

type PostgresTenantRepository struct {
	pool *pgxpool.Pool
	// slugFn is injectable so tests can force slug collisions; production
	// always uses newSlug.
	slugFn func() (string, error)
}

func NewPostgresTenantRepository(pool *pgxpool.Pool) *PostgresTenantRepository {
	return &PostgresTenantRepository{pool: pool, slugFn: newSlug}
}

var _ TenantRepository = (*PostgresTenantRepository)(nil)

func (r *PostgresTenantRepository) MembershipsForUser(ctx context.Context, userID uuid.UUID) ([]domain.UserMembership, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT t.id, t.slug, t.name, m.role, m.created_at
		FROM memberships m
		JOIN tenants t ON t.id = m.tenant_id
		WHERE m.user_id = $1
		ORDER BY m.created_at ASC, t.slug ASC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("memberships for user: %w", err)
	}
	defer rows.Close()

	var out []domain.UserMembership
	for rows.Next() {
		var m domain.UserMembership
		if err := rows.Scan(&m.TenantID, &m.Slug, &m.Name, &m.Role, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan membership: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memberships: %w", err)
	}
	return out, nil
}

func (r *PostgresTenantRepository) MembershipRole(ctx context.Context, userID uuid.UUID, slug string) (string, error) {
	var role string
	err := r.pool.QueryRow(ctx, `
		SELECT m.role
		FROM memberships m
		JOIN tenants t ON t.id = m.tenant_id
		WHERE m.user_id = $1 AND t.slug = $2
	`, userID, slug).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", apperrors.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("membership role: %w", err)
	}
	return role, nil
}

func (r *PostgresTenantRepository) TenantBySlug(ctx context.Context, slug string) (domain.Tenant, error) {
	var t domain.Tenant
	err := r.pool.QueryRow(ctx, `
		SELECT id, slug, name, created_at FROM tenants WHERE slug = $1
	`, slug).Scan(&t.ID, &t.Slug, &t.Name, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Tenant{}, apperrors.ErrNotFound
	}
	if err != nil {
		return domain.Tenant{}, fmt.Errorf("tenant by slug: %w", err)
	}
	return t, nil
}

// ProvisionOwnerTx inserts tenant + owner membership inside the caller's tx.
// Slug collisions are retried on a savepoint (nested tx) so a unique-violation
// does not poison the caller's transaction.
func (r *PostgresTenantRepository) ProvisionOwnerTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (string, error) {
	for attempt := 0; attempt < slugAttempts; attempt++ {
		slug, err := r.slugFn()
		if err != nil {
			return "", fmt.Errorf("generate tenant slug: %w", err)
		}

		inner, err := tx.Begin(ctx) // SAVEPOINT
		if err != nil {
			return "", fmt.Errorf("provision savepoint: %w", err)
		}

		var tenantID uuid.UUID
		err = inner.QueryRow(ctx,
			`INSERT INTO tenants (slug) VALUES ($1) RETURNING id`, slug,
		).Scan(&tenantID)
		if err != nil {
			_ = inner.Rollback(ctx)
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
				continue // astronomically unlikely; retry with a fresh slug
			}
			return "", fmt.Errorf("insert tenant: %w", err)
		}

		if _, err := inner.Exec(ctx,
			`INSERT INTO memberships (user_id, tenant_id, role) VALUES ($1, $2, $3)`,
			userID, tenantID, domain.RoleOwner,
		); err != nil {
			_ = inner.Rollback(ctx)
			return "", fmt.Errorf("insert owner membership: %w", err)
		}

		if err := inner.Commit(ctx); err != nil {
			return "", fmt.Errorf("provision commit: %w", err)
		}
		return slug, nil
	}
	return "", fmt.Errorf("provision tenant: slug collision persisted after %d attempts", slugAttempts)
}

func (r *PostgresTenantRepository) CreateInvite(ctx context.Context, inv domain.Invite) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO tenant_invites (id, tenant_id, email_lookup_hash, role, token_hash, invited_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, inv.ID, inv.TenantID, inv.EmailLookupHash, inv.Role, inv.TokenHash, inv.InvitedBy, inv.ExpiresAt)
	if err != nil {
		return fmt.Errorf("create invite: %w", err)
	}
	return nil
}

func (r *PostgresTenantRepository) AcceptInvite(ctx context.Context, tokenHash string, userID uuid.UUID) (domain.AcceptedInvite, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.AcceptedInvite{}, fmt.Errorf("accept invite: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Row-lock the invite so concurrent accepts serialize; compare the email
	// blind index in SQL so plaintext never surfaces here.
	var (
		inviteID   uuid.UUID
		tenantID   uuid.UUID
		out        domain.AcceptedInvite
		expiresAt  time.Time
		acceptedAt *time.Time
		emailMatch bool
		inviterOK  bool
	)
	// inviter_ok re-checks at accept time that the inviter STILL holds
	// owner/admin membership — a demoted admin's pending invites die with
	// their privileges instead of surviving as escalation artifacts.
	// u.status guard: soft-deleted accounts cannot mint memberships.
	err = tx.QueryRow(ctx, `
		SELECT i.id, i.tenant_id, t.slug, t.name, i.role, i.expires_at, i.accepted_at,
		       i.email_lookup_hash = u.email_lookup_hash,
		       EXISTS (
		           SELECT 1 FROM memberships im
		           WHERE im.user_id = i.invited_by AND im.tenant_id = i.tenant_id
		             AND im.role IN ('owner', 'admin')
		       )
		FROM tenant_invites i
		JOIN tenants t ON t.id = i.tenant_id
		CROSS JOIN users u
		WHERE i.token_hash = $1 AND u.id = $2 AND u.status <> 'deleted'
		FOR UPDATE OF i
	`, tokenHash, userID).Scan(&inviteID, &tenantID, &out.TenantSlug, &out.TenantName, &out.Role, &expiresAt, &acceptedAt, &emailMatch, &inviterOK)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.AcceptedInvite{}, domain.ErrInviteNotFound
	}
	if err != nil {
		return domain.AcceptedInvite{}, fmt.Errorf("accept invite: lookup: %w", err)
	}
	if acceptedAt != nil {
		return domain.AcceptedInvite{}, domain.ErrInviteUsed
	}
	if time.Now().After(expiresAt) {
		return domain.AcceptedInvite{}, domain.ErrInviteExpired
	}
	if !inviterOK {
		return domain.AcceptedInvite{}, domain.ErrInviteInviterRevoked
	}
	if !emailMatch {
		return domain.AcceptedInvite{}, domain.ErrInviteEmailMismatch
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO memberships (user_id, tenant_id, role) VALUES ($1, $2, $3)
		ON CONFLICT (user_id, tenant_id) DO NOTHING
	`, userID, tenantID, out.Role)
	if err != nil {
		return domain.AcceptedInvite{}, fmt.Errorf("accept invite: membership: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Existing membership: reject and roll back so the invite stays usable
		// for its intended purpose review rather than being silently burned.
		return domain.AcceptedInvite{}, domain.ErrAlreadyMember
	}

	if _, err := tx.Exec(ctx, `
		UPDATE tenant_invites SET accepted_at = now(), accepted_by = $2 WHERE id = $1
	`, inviteID, userID); err != nil {
		return domain.AcceptedInvite{}, fmt.Errorf("accept invite: consume: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.AcceptedInvite{}, fmt.Errorf("accept invite: commit: %w", err)
	}
	return out, nil
}

// PlanForTenant joins the tenant to its plan (TKT-R4b). An unknown slug — or a
// tenant somehow without a resolvable plan — degrades to the free plan read
// from the plans table, so callers always get a real, tightest-tier limit set.
func (r *PostgresTenantRepository) PlanForTenant(ctx context.Context, slug string) (domain.Plan, error) {
	var p domain.Plan
	err := r.pool.QueryRow(ctx, `
		SELECT p.name, p.max_types, p.max_entries, p.max_fields_per_type, p.max_entry_bytes, p.soft_threshold_pct
		FROM tenants t
		JOIN plans p ON p.name = t.plan
		WHERE t.slug = $1
	`, slug).Scan(&p.Name, &p.MaxTypes, &p.MaxEntries, &p.MaxFieldsPerType, &p.MaxEntryBytes, &p.SoftThresholdPct)
	if errors.Is(err, pgx.ErrNoRows) {
		return r.planByName(ctx, domain.DefaultPlanName)
	}
	if err != nil {
		return domain.Plan{}, fmt.Errorf("plan for tenant: %w", err)
	}
	return p, nil
}

func (r *PostgresTenantRepository) SetTenantPlan(ctx context.Context, slug, plan string) error {
	// Validate the plan explicitly for a clean 422 (vs a raw FK violation).
	var exists bool
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM plans WHERE name = $1)`, plan).Scan(&exists); err != nil {
		return fmt.Errorf("check plan: %w", err)
	}
	if !exists {
		return domain.ErrPlanUnknown
	}
	tag, err := r.pool.Exec(ctx, `UPDATE tenants SET plan = $2 WHERE slug = $1`, slug, plan)
	if err != nil {
		// The EXISTS check above races with a concurrent plan delete; if the
		// FK fires anyway, surface it as the clean 422, not a 500.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return domain.ErrPlanUnknown
		}
		return fmt.Errorf("set tenant plan: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return apperrors.ErrNotFound
	}
	return nil
}

func (r *PostgresTenantRepository) planByName(ctx context.Context, name string) (domain.Plan, error) {
	var p domain.Plan
	err := r.pool.QueryRow(ctx, `
		SELECT name, max_types, max_entries, max_fields_per_type, max_entry_bytes, soft_threshold_pct
		FROM plans WHERE name = $1
	`, name).Scan(&p.Name, &p.MaxTypes, &p.MaxEntries, &p.MaxFieldsPerType, &p.MaxEntryBytes, &p.SoftThresholdPct)
	if err != nil {
		return domain.Plan{}, fmt.Errorf("plan %q: %w", name, err)
	}
	return p, nil
}

// newSlug returns an opaque tenant identifier: "t_" + 32 chars of lowercase
// base32 over 20 random bytes. No PII — the slug flows into JWTs and
// content.tenant_id (D10). Matches ^[a-z0-9_]+$.
func newSlug() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	return "t_" + strings.ToLower(enc), nil
}
