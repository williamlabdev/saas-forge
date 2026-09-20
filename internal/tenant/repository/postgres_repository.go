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
		INSERT INTO tenant_invites
			(id, tenant_id, email_lookup_hash, email_encrypted, email_encrypted_nonce, role, token_hash, invited_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, inv.ID, inv.TenantID, inv.EmailLookupHash, inv.EmailEncrypted, inv.EmailEncryptedNonce, inv.Role, inv.TokenHash, inv.InvitedBy, inv.ExpiresAt)
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
		revokedAt  *time.Time
		emailMatch bool
		inviterOK  bool
	)
	// inviter_ok re-checks at accept time that the inviter STILL holds
	// owner/admin membership — a demoted admin's pending invites die with
	// their privileges instead of surviving as escalation artifacts.
	// u.status guard: soft-deleted accounts cannot mint memberships.
	err = tx.QueryRow(ctx, `
		SELECT i.id, i.tenant_id, t.slug, t.name, i.role, i.expires_at, i.accepted_at, i.revoked_at,
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
	`, tokenHash, userID).Scan(&inviteID, &tenantID, &out.TenantSlug, &out.TenantName, &out.Role, &expiresAt, &acceptedAt, &revokedAt, &emailMatch, &inviterOK)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.AcceptedInvite{}, domain.ErrInviteNotFound
	}
	if err != nil {
		return domain.AcceptedInvite{}, fmt.Errorf("accept invite: lookup: %w", err)
	}
	if acceptedAt != nil {
		return domain.AcceptedInvite{}, domain.ErrInviteUsed
	}
	// Checked before expiry deliberately: a deliberate owner/admin
	// cancellation is a more specific, more actionable answer than "time ran
	// out" for an invite that happens to also be past its TTL by the time
	// the invitee tries it.
	if revokedAt != nil {
		return domain.AcceptedInvite{}, domain.ErrInviteRevoked
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

// memberListCap bounds the member roster and pending-invite list. This
// repository has no established pagination convention (grep confirms no
// other list method here takes a cursor/offset), so 2.2c documents a fixed
// cap instead of inventing one (ADR-022) — 200 members/invites covers every
// tenant shape this product serves today, and revisiting it is one line
// here plus the ADR's trigger condition.
const memberListCap = 200

// Members lists a tenant's roster (2.2c). Email is intentionally NOT
// resolved here — see the interface doc.
func (r *PostgresTenantRepository) Members(ctx context.Context, tenantID uuid.UUID) ([]domain.Member, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT user_id, role, created_at
		FROM memberships
		WHERE tenant_id = $1
		ORDER BY created_at ASC, user_id ASC
		LIMIT $2
	`, tenantID, memberListCap)
	if err != nil {
		return nil, fmt.Errorf("members: %w", err)
	}
	defer rows.Close()

	var out []domain.Member
	for rows.Next() {
		var m domain.Member
		if err := rows.Scan(&m.UserID, &m.Role, &m.JoinedAt); err != nil {
			return nil, fmt.Errorf("scan member: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate members: %w", err)
	}
	return out, nil
}

// UpdateMemberRole atomically changes a member's role, protecting the
// tenant's last owner. See the interface doc for the locking shape.
//
// The owner row-set is locked first, in a fixed order, via
// lockOwnersOrdered — see that method's doc comment for why: two owners
// concurrently demoting EACH OTHER is the one path that reaches this method
// without going through the service-layer admin-vs-owner check (an owner
// acting on another owner isn't "an admin touching the owner role", so the
// service lets it straight through — see ADR-022 §3), and without a fixed
// lock order that race deadlocks instead of serializing.
func (r *PostgresTenantRepository) UpdateMemberRole(ctx context.Context, tenantID, targetUserID uuid.UUID, newRole string) (domain.Member, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.Member{}, fmt.Errorf("update member role: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ownerCount, err := r.lockOwnersOrdered(ctx, tx, tenantID)
	if err != nil {
		return domain.Member{}, err
	}

	var currentRole string
	err = tx.QueryRow(ctx, `
		SELECT role FROM memberships WHERE tenant_id = $1 AND user_id = $2 FOR UPDATE
	`, tenantID, targetUserID).Scan(&currentRole)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Member{}, apperrors.ErrNotFound
	}
	if err != nil {
		return domain.Member{}, fmt.Errorf("update member role: lookup: %w", err)
	}

	if currentRole == domain.RoleOwner && newRole != domain.RoleOwner {
		if ownerCount <= 1 {
			return domain.Member{}, domain.ErrLastOwner
		}
	}

	var joinedAt time.Time
	err = tx.QueryRow(ctx, `
		UPDATE memberships SET role = $3 WHERE tenant_id = $1 AND user_id = $2
		RETURNING created_at
	`, tenantID, targetUserID, newRole).Scan(&joinedAt)
	if err != nil {
		return domain.Member{}, fmt.Errorf("update member role: update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Member{}, fmt.Errorf("update member role: commit: %w", err)
	}
	return domain.Member{UserID: targetUserID, Role: newRole, JoinedAt: joinedAt}, nil
}

// RemoveMember atomically evicts a member, with the same last-owner
// protection — and the same fixed owner-lock ordering, for the same
// concurrent-owners-vs-each-other reason — as UpdateMemberRole.
func (r *PostgresTenantRepository) RemoveMember(ctx context.Context, tenantID, targetUserID uuid.UUID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("remove member: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ownerCount, err := r.lockOwnersOrdered(ctx, tx, tenantID)
	if err != nil {
		return err
	}

	var currentRole string
	err = tx.QueryRow(ctx, `
		SELECT role FROM memberships WHERE tenant_id = $1 AND user_id = $2 FOR UPDATE
	`, tenantID, targetUserID).Scan(&currentRole)
	if errors.Is(err, pgx.ErrNoRows) {
		return apperrors.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("remove member: lookup: %w", err)
	}

	if currentRole == domain.RoleOwner {
		if ownerCount <= 1 {
			return domain.ErrLastOwner
		}
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2
	`, tenantID, targetUserID); err != nil {
		return fmt.Errorf("remove member: delete: %w", err)
	}
	return tx.Commit(ctx)
}

// lockOwnersOrdered row-locks every owner membership of tenantID, in a
// fixed (user_id) order, and returns how many there are. It must be called
// FIRST in UpdateMemberRole and RemoveMember, before either method's
// per-target row lock.
//
// Why: UpdateMemberRole/RemoveMember also lock the specific target row
// (FOR UPDATE) to read its current role. When two owners A and B act on
// EACH OTHER concurrently — the one HTTP-reachable path to this method that
// the service layer doesn't otherwise serialize (see ADR-022 §3) — the
// transaction started on A's behalf locks target row B first, while the one
// started on B's behalf locks target row A first. If the owner-set lock
// were then taken second (as an earlier version of this code did, via a
// COUNT-over-a-locked-subquery helper), each transaction would already hold
// one of the two rows the owner-set query needs and block waiting for the
// other — a real SQLSTATE 40P01, reproduced 5/5 times under forced
// interleaving (ADR-022 §3b). Locking the owner set FIRST, in the same
// user_id order, in every transaction that touches a tenant's owners closes
// that: whichever transaction gets there first takes every owner row it
// needs before the other transaction can take any of them, so the second
// transaction blocks cleanly on the first lock it doesn't hold instead of
// forming a cycle. It then proceeds — never deadlocks, just serializes —
// and reads the now-current (possibly-decremented) owner count.
//
// The target-row lock taken afterward is redundant, not conflicting, when
// the target is itself an owner: it's already held by this same
// transaction from the loop below, so re-acquiring it is a same-transaction
// no-op.
//
// This does mean every UpdateMemberRole/RemoveMember call contends on the
// tenant's owner rows, even ones that touch no owner (e.g. admin editor→
// viewer) — a deliberate correctness-over-throughput tradeoff for a
// low-frequency admin action (ADR-022 §3b).
func (r *PostgresTenantRepository) lockOwnersOrdered(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (int, error) {
	rows, err := tx.Query(ctx, `
		SELECT user_id FROM memberships
		WHERE tenant_id = $1 AND role = 'owner'
		ORDER BY user_id
		FOR UPDATE
	`, tenantID)
	if err != nil {
		return 0, fmt.Errorf("lock owners: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("lock owners: scan: %w", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("lock owners: iterate: %w", err)
	}
	return count, nil
}

// ListInvites returns a tenant's PENDING invites (2.2c) — neither accepted
// nor revoked — most recent first. The raw token never appears here.
func (r *PostgresTenantRepository) ListInvites(ctx context.Context, tenantID uuid.UUID) ([]domain.Invite, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, role, invited_by, created_at, expires_at,
		       email_encrypted, email_encrypted_nonce
		FROM tenant_invites
		WHERE tenant_id = $1 AND accepted_at IS NULL AND revoked_at IS NULL
		ORDER BY created_at DESC
		LIMIT $2
	`, tenantID, memberListCap)
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	defer rows.Close()

	var out []domain.Invite
	for rows.Next() {
		var inv domain.Invite
		if err := rows.Scan(&inv.ID, &inv.TenantID, &inv.Role, &inv.InvitedBy, &inv.CreatedAt, &inv.ExpiresAt,
			&inv.EmailEncrypted, &inv.EmailEncryptedNonce); err != nil {
			return nil, fmt.Errorf("scan invite: %w", err)
		}
		out = append(out, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate invites: %w", err)
	}
	return out, nil
}

// RevokeInvite marks a pending invite cancelled (2.2c). Revoking an
// already-revoked invite is a no-op success — see the interface doc.
func (r *PostgresTenantRepository) RevokeInvite(ctx context.Context, tenantID, inviteID, revokedBy uuid.UUID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("revoke invite: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var acceptedAt, revokedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT accepted_at, revoked_at FROM tenant_invites
		WHERE id = $1 AND tenant_id = $2
		FOR UPDATE
	`, inviteID, tenantID).Scan(&acceptedAt, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrInviteNotFound
	}
	if err != nil {
		return fmt.Errorf("revoke invite: lookup: %w", err)
	}
	if acceptedAt != nil {
		return domain.ErrInviteUsed
	}
	if revokedAt != nil {
		return tx.Commit(ctx) // idempotent: already revoked, nothing to do
	}

	if _, err := tx.Exec(ctx, `
		UPDATE tenant_invites SET revoked_at = now(), revoked_by = $2 WHERE id = $1
	`, inviteID, revokedBy); err != nil {
		return fmt.Errorf("revoke invite: update: %w", err)
	}
	return tx.Commit(ctx)
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
