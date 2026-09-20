package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type BillingConfig struct {
	PlanName      string
	RenewsAt      time.Time
	PaymentStatus string
	AppsQuota     int
	SeatsQuota    int
}

type Invoice struct {
	ID       string
	IssuedAt time.Time
	Amount   string
	Status   string
}

type StaffMember struct {
	ID        uuid.UUID
	Name      string
	Email     string
	Role      string
	CreatedAt time.Time
}

type Alert struct {
	ID        uuid.UUID
	Title     string
	AlertType string
	Read      bool
	CreatedAt time.Time
}

type AppStatusCounts struct {
	Active   int
	Paused   int
	Archived int
}

type PlatformConsoleRepository interface {
	GetBillingConfig(ctx context.Context) (BillingConfig, error)
	CountPlatformApps(ctx context.Context) (int, error)
	CountPlatformStaff(ctx context.Context) (int, error)
	ListInvoices(ctx context.Context, limit int) ([]Invoice, error)
	ListStaff(ctx context.Context) ([]StaffMember, error)
	StaffEmailExists(ctx context.Context, email string) (bool, error)
	CreateStaff(ctx context.Context, name, email, role string) (StaffMember, error)
	ListAlerts(ctx context.Context, limit int) ([]Alert, error)
	AppStatusCounts(ctx context.Context) (AppStatusCounts, error)
}

type PostgresPlatformConsoleRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresPlatformConsoleRepository(pool *pgxpool.Pool) *PostgresPlatformConsoleRepository {
	return &PostgresPlatformConsoleRepository{pool: pool}
}

func (r *PostgresPlatformConsoleRepository) GetBillingConfig(ctx context.Context) (BillingConfig, error) {
	var c BillingConfig
	err := r.pool.QueryRow(ctx, `
		SELECT plan_name, renews_at, payment_status, apps_quota, seats_quota
		FROM platform_billing_config WHERE id = 1`).Scan(
		&c.PlanName, &c.RenewsAt, &c.PaymentStatus, &c.AppsQuota, &c.SeatsQuota,
	)
	if err != nil {
		return BillingConfig{}, fmt.Errorf("platform_billing_config: %w", err)
	}
	return c, nil
}

func (r *PostgresPlatformConsoleRepository) CountPlatformApps(ctx context.Context) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM platform_apps`).Scan(&n)
	return n, err
}

func (r *PostgresPlatformConsoleRepository) CountPlatformStaff(ctx context.Context) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM platform_staff`).Scan(&n)
	return n, err
}

func (r *PostgresPlatformConsoleRepository) ListInvoices(ctx context.Context, limit int) ([]Invoice, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, issued_at, amount, status
		FROM platform_invoices
		ORDER BY issued_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invoice
	for rows.Next() {
		var inv Invoice
		if err := rows.Scan(&inv.ID, &inv.IssuedAt, &inv.Amount, &inv.Status); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

func (r *PostgresPlatformConsoleRepository) ListStaff(ctx context.Context) ([]StaffMember, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, name, email, role, created_at
		FROM platform_staff
		ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StaffMember
	for rows.Next() {
		var s StaffMember
		if err := rows.Scan(&s.ID, &s.Name, &s.Email, &s.Role, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *PostgresPlatformConsoleRepository) CreateStaff(ctx context.Context, name, email, role string) (StaffMember, error) {
	id := uuid.New()
	row := r.pool.QueryRow(ctx, `
		INSERT INTO platform_staff (id, name, email, role)
		VALUES ($1, $2, $3, $4)
		RETURNING id, name, email, role, created_at`,
		id, name, email, role,
	)
	var s StaffMember
	err := row.Scan(&s.ID, &s.Name, &s.Email, &s.Role, &s.CreatedAt)
	if err != nil {
		return StaffMember{}, err
	}
	return s, nil
}

func (r *PostgresPlatformConsoleRepository) ListAlerts(ctx context.Context, limit int) ([]Alert, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, title, alert_type, read_at IS NOT NULL, created_at
		FROM platform_alerts
		ORDER BY created_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.Title, &a.AlertType, &a.Read, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *PostgresPlatformConsoleRepository) AppStatusCounts(ctx context.Context) (AppStatusCounts, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT status, COUNT(*)::int FROM platform_apps GROUP BY status`)
	if err != nil {
		return AppStatusCounts{}, err
	}
	defer rows.Close()
	var c AppStatusCounts
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return AppStatusCounts{}, err
		}
		switch status {
		case "active":
			c.Active = n
		case "paused":
			c.Paused = n
		case "archived":
			c.Archived = n
		}
	}
	return c, rows.Err()
}

func (r *PostgresPlatformConsoleRepository) StaffEmailExists(ctx context.Context, email string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM platform_staff WHERE LOWER(email) = LOWER($1))`, email).Scan(&exists)
	return exists, err
}

var _ PlatformConsoleRepository = (*PostgresPlatformConsoleRepository)(nil)
