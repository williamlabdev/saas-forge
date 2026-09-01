package service

import (
	"context"
	"strings"
	"time"

	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/validate"
	"github.com/williamlabdev/saas-forge/internal/platformops/repository"
)

type PlatformConsoleService interface {
	GetBillingSummary(ctx context.Context) (BillingSummaryDTO, error)
	ListInvoices(ctx context.Context, limit int) ([]InvoiceDTO, error)
	ListStaff(ctx context.Context) ([]StaffMemberDTO, error)
	CreateStaff(ctx context.Context, in CreateStaffInput) (StaffMemberDTO, error)
	ListAlerts(ctx context.Context, limit int) ([]AlertDTO, error)
	GetReportsSummary(ctx context.Context) (ReportsSummaryDTO, error)
}

type BillingSummaryDTO struct {
	PlanName      string `json:"plan_name"`
	RenewsAt      string `json:"renews_at"`
	PaymentStatus string `json:"payment_status"`
	AppsUsed      int    `json:"apps_used"`
	AppsQuota     int    `json:"apps_quota"`
	SeatsUsed     int    `json:"seats_used"`
	SeatsQuota    int    `json:"seats_quota"`
}

type InvoiceDTO struct {
	ID       string `json:"id"`
	IssuedAt string `json:"issued_at"`
	Amount   string `json:"amount"`
	Status   string `json:"status"`
}

type StaffMemberDTO struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

type AlertDTO struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	AlertType string `json:"alert_type"`
	Read      bool   `json:"read"`
	CreatedAt string `json:"created_at"`
}

type ReportsSummaryDTO struct {
	MRR        string `json:"mrr"`
	ActiveApps int    `json:"active_apps"`
	PausedApps int    `json:"paused_apps"`
}

type CreateStaffInput struct {
	Email string `json:"email" validate:"required,email,max=200"`
	Role  string `json:"role" validate:"required,max=50"`
	Name  string `json:"name" validate:"omitempty,max=200"`
}

type platformConsoleService struct {
	console repository.PlatformConsoleRepository
	authz   authz.Authorizer
}

func NewPlatformConsoleService(console repository.PlatformConsoleRepository, authz authz.Authorizer) PlatformConsoleService {
	return &platformConsoleService{console: console, authz: authz}
}

func (s *platformConsoleService) allowList(ctx context.Context) error {
	if _, ok := authn.SubjectFromContext(ctx); !ok {
		return apperrors.ErrUnauthorized
	}
	return s.authz.Allow(ctx, authz.Input{
		Action:   authz.ActionPlatformAppList,
		Resource: authz.Resource{Type: "platform_console", ID: "collection"},
	})
}

func (s *platformConsoleService) allowCreate(ctx context.Context) error {
	if _, ok := authn.SubjectFromContext(ctx); !ok {
		return apperrors.ErrUnauthorized
	}
	return s.authz.Allow(ctx, authz.Input{
		Action:   authz.ActionPlatformAppCreate,
		Resource: authz.Resource{Type: "platform_staff", ID: "collection"},
	})
}

func (s *platformConsoleService) GetBillingSummary(ctx context.Context) (BillingSummaryDTO, error) {
	if err := s.allowList(ctx); err != nil {
		return BillingSummaryDTO{}, err
	}
	cfg, err := s.console.GetBillingConfig(ctx)
	if err != nil {
		return BillingSummaryDTO{}, err
	}
	appsUsed, err := s.console.CountPlatformApps(ctx)
	if err != nil {
		return BillingSummaryDTO{}, err
	}
	seatsUsed, err := s.console.CountPlatformStaff(ctx)
	if err != nil {
		return BillingSummaryDTO{}, err
	}
	return BillingSummaryDTO{
		PlanName:      cfg.PlanName,
		RenewsAt:      cfg.RenewsAt.Format("2006-01-02"),
		PaymentStatus: cfg.PaymentStatus,
		AppsUsed:      appsUsed,
		AppsQuota:     cfg.AppsQuota,
		SeatsUsed:     seatsUsed,
		SeatsQuota:    cfg.SeatsQuota,
	}, nil
}

func (s *platformConsoleService) ListInvoices(ctx context.Context, limit int) ([]InvoiceDTO, error) {
	if err := s.allowList(ctx); err != nil {
		return nil, err
	}
	rows, err := s.console.ListInvoices(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]InvoiceDTO, len(rows))
	for i, inv := range rows {
		out[i] = InvoiceDTO{
			ID:       inv.ID,
			IssuedAt: inv.IssuedAt.Format("2006-01-02"),
			Amount:   inv.Amount,
			Status:   inv.Status,
		}
	}
	return out, nil
}

func (s *platformConsoleService) ListStaff(ctx context.Context) ([]StaffMemberDTO, error) {
	if err := s.allowList(ctx); err != nil {
		return nil, err
	}
	rows, err := s.console.ListStaff(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]StaffMemberDTO, len(rows))
	for i, m := range rows {
		out[i] = StaffMemberDTO{
			ID:    m.ID.String(),
			Name:  m.Name,
			Email: m.Email,
			Role:  m.Role,
		}
	}
	return out, nil
}

func (s *platformConsoleService) CreateStaff(ctx context.Context, in CreateStaffInput) (StaffMemberDTO, error) {
	if err := validate.Struct(in); err != nil {
		return StaffMemberDTO{}, apperrors.Wrap("VALIDATION_FAILED", err.Error(), 400, err)
	}
	if err := s.allowCreate(ctx); err != nil {
		return StaffMemberDTO{}, err
	}
	email := strings.TrimSpace(strings.ToLower(in.Email))
	role := strings.TrimSpace(in.Role)
	name := strings.TrimSpace(in.Name)
	if name == "" {
		parts := strings.Split(email, "@")
		name = parts[0]
	}
	exists, err := s.console.StaffEmailExists(ctx, email)
	if err != nil {
		return StaffMemberDTO{}, err
	}
	if exists {
		return StaffMemberDTO{}, apperrors.Wrap("CONFLICT", "email already on the team", 409, nil)
	}
	row, err := s.console.CreateStaff(ctx, name, email, role)
	if err != nil {
		return StaffMemberDTO{}, err
	}
	return StaffMemberDTO{
		ID:    row.ID.String(),
		Name:  row.Name,
		Email: row.Email,
		Role:  row.Role,
	}, nil
}

func (s *platformConsoleService) ListAlerts(ctx context.Context, limit int) ([]AlertDTO, error) {
	if err := s.allowList(ctx); err != nil {
		return nil, err
	}
	rows, err := s.console.ListAlerts(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]AlertDTO, len(rows))
	for i, a := range rows {
		out[i] = AlertDTO{
			ID:        a.ID.String(),
			Title:     a.Title,
			AlertType: a.AlertType,
			Read:      a.Read,
			CreatedAt: a.CreatedAt.UTC().Format(time.RFC3339),
		}
	}
	return out, nil
}

func (s *platformConsoleService) GetReportsSummary(ctx context.Context) (ReportsSummaryDTO, error) {
	if err := s.allowList(ctx); err != nil {
		return ReportsSummaryDTO{}, err
	}
	counts, err := s.console.AppStatusCounts(ctx)
	if err != nil {
		return ReportsSummaryDTO{}, err
	}
	return ReportsSummaryDTO{
		MRR:        "$12.4k",
		ActiveApps: counts.Active,
		PausedApps: counts.Paused,
	}, nil
}
