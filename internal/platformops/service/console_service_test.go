package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/platformops/repository"
)

// denyAuthorizer always refuses, to exercise the authorization-denied branches.
type denyAuthorizer struct{}

func (denyAuthorizer) Allow(context.Context, authz.Input) error { return apperrors.ErrForbidden }

// authedCtx returns a context carrying a subject, as the authn middleware would.
func authedCtx() context.Context {
	return authn.WithSubject(context.Background(), authn.Subject{UserID: uuid.New()})
}

// fakeConsoleRepo is an in-memory PlatformConsoleRepository. Any non-nil err
// field is returned by the matching method to exercise error propagation.
type fakeConsoleRepo struct {
	billing      repository.BillingConfig
	appCount     int
	staffCount   int
	invoices     []repository.Invoice
	staff        []repository.StaffMember
	emailExists  bool
	created      *repository.StaffMember
	alerts       []repository.Alert
	statusCounts repository.AppStatusCounts

	errBilling, errAppCount, errStaffCount, errInvoices, errStaff error
	errEmailExists, errCreate, errAlerts, errStatusCounts         error
}

func (f *fakeConsoleRepo) GetBillingConfig(context.Context) (repository.BillingConfig, error) {
	return f.billing, f.errBilling
}
func (f *fakeConsoleRepo) CountPlatformApps(context.Context) (int, error) {
	return f.appCount, f.errAppCount
}
func (f *fakeConsoleRepo) CountPlatformStaff(context.Context) (int, error) {
	return f.staffCount, f.errStaffCount
}
func (f *fakeConsoleRepo) ListInvoices(_ context.Context, _ int) ([]repository.Invoice, error) {
	return f.invoices, f.errInvoices
}
func (f *fakeConsoleRepo) ListStaff(context.Context) ([]repository.StaffMember, error) {
	return f.staff, f.errStaff
}
func (f *fakeConsoleRepo) StaffEmailExists(_ context.Context, _ string) (bool, error) {
	return f.emailExists, f.errEmailExists
}
func (f *fakeConsoleRepo) CreateStaff(_ context.Context, name, email, role string) (repository.StaffMember, error) {
	if f.errCreate != nil {
		return repository.StaffMember{}, f.errCreate
	}
	m := repository.StaffMember{ID: uuid.New(), Name: name, Email: email, Role: role, CreatedAt: time.Now()}
	f.created = &m
	return m, nil
}
func (f *fakeConsoleRepo) ListAlerts(_ context.Context, _ int) ([]repository.Alert, error) {
	return f.alerts, f.errAlerts
}
func (f *fakeConsoleRepo) AppStatusCounts(context.Context) (repository.AppStatusCounts, error) {
	return f.statusCounts, f.errStatusCounts
}

func newConsoleSvc(repo *fakeConsoleRepo) PlatformConsoleService {
	return NewPlatformConsoleService(repo, authz.NewAllowAllAuthorizer())
}

func TestConsole_GetBillingSummary(t *testing.T) {
	repo := &fakeConsoleRepo{
		billing: repository.BillingConfig{
			PlanName:      "Pro",
			RenewsAt:      time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			PaymentStatus: "paid",
			AppsQuota:     10,
			SeatsQuota:    5,
		},
		appCount:   3,
		staffCount: 2,
	}
	got, err := newConsoleSvc(repo).GetBillingSummary(authedCtx())
	if err != nil {
		t.Fatal(err)
	}
	if got.PlanName != "Pro" || got.RenewsAt != "2026-08-01" || got.AppsUsed != 3 || got.SeatsUsed != 2 {
		t.Fatalf("unexpected summary: %+v", got)
	}
}

func TestConsole_GetBillingSummary_Unauthenticated(t *testing.T) {
	_, err := newConsoleSvc(&fakeConsoleRepo{}).GetBillingSummary(context.Background())
	if !errors.Is(err, apperrors.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

func TestConsole_GetBillingSummary_Forbidden(t *testing.T) {
	svc := NewPlatformConsoleService(&fakeConsoleRepo{}, denyAuthorizer{})
	_, err := svc.GetBillingSummary(authedCtx())
	if !errors.Is(err, apperrors.ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
}

func TestConsole_GetBillingSummary_RepoErrors(t *testing.T) {
	sentinel := errors.New("boom")
	cases := map[string]*fakeConsoleRepo{
		"billing":    {errBilling: sentinel},
		"appCount":   {errAppCount: sentinel},
		"staffCount": {errStaffCount: sentinel},
	}
	for name, repo := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := newConsoleSvc(repo).GetBillingSummary(authedCtx()); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestConsole_ListInvoices(t *testing.T) {
	repo := &fakeConsoleRepo{invoices: []repository.Invoice{
		{ID: "in_1", IssuedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Amount: "$10", Status: "paid"},
	}}
	got, err := newConsoleSvc(repo).ListInvoices(authedCtx(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "in_1" || got[0].IssuedAt != "2026-01-02" {
		t.Fatalf("unexpected invoices: %+v", got)
	}
}

func TestConsole_ListInvoices_RepoError(t *testing.T) {
	repo := &fakeConsoleRepo{errInvoices: errors.New("boom")}
	if _, err := newConsoleSvc(repo).ListInvoices(authedCtx(), 10); err == nil {
		t.Fatal("expected error")
	}
}

func TestConsole_ListStaff(t *testing.T) {
	repo := &fakeConsoleRepo{staff: []repository.StaffMember{
		{ID: uuid.New(), Name: "Ada", Email: "ada@example.com", Role: "admin"},
	}}
	got, err := newConsoleSvc(repo).ListStaff(authedCtx())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "Ada" {
		t.Fatalf("unexpected staff: %+v", got)
	}
}

func TestConsole_ListStaff_RepoError(t *testing.T) {
	repo := &fakeConsoleRepo{errStaff: errors.New("boom")}
	if _, err := newConsoleSvc(repo).ListStaff(authedCtx()); err == nil {
		t.Fatal("expected error")
	}
}

func TestConsole_CreateStaff(t *testing.T) {
	repo := &fakeConsoleRepo{}
	got, err := newConsoleSvc(repo).CreateStaff(authedCtx(), CreateStaffInput{
		Email: "New@Example.com", Role: "editor",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Email lower-cased and name derived from local-part when omitted.
	if got.Email != "new@example.com" || got.Name != "new" || got.Role != "editor" {
		t.Fatalf("unexpected staff: %+v", got)
	}
}

func TestConsole_CreateStaff_ExplicitName(t *testing.T) {
	repo := &fakeConsoleRepo{}
	got, err := newConsoleSvc(repo).CreateStaff(authedCtx(), CreateStaffInput{
		Email: "x@example.com", Role: "editor", Name: "Grace",
	})
	if err != nil || got.Name != "Grace" {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestConsole_CreateStaff_ValidationFails(t *testing.T) {
	// Missing role -> validation error before any authz/repo call.
	_, err := newConsoleSvc(&fakeConsoleRepo{}).CreateStaff(authedCtx(), CreateStaffInput{Email: "x@example.com"})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestConsole_CreateStaff_Conflict(t *testing.T) {
	repo := &fakeConsoleRepo{emailExists: true}
	_, err := newConsoleSvc(repo).CreateStaff(authedCtx(), CreateStaffInput{Email: "x@example.com", Role: "editor"})
	if err == nil {
		t.Fatal("expected conflict error")
	}
}

func TestConsole_CreateStaff_Forbidden(t *testing.T) {
	svc := NewPlatformConsoleService(&fakeConsoleRepo{}, denyAuthorizer{})
	_, err := svc.CreateStaff(authedCtx(), CreateStaffInput{Email: "x@example.com", Role: "editor"})
	if !errors.Is(err, apperrors.ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
}

func TestConsole_CreateStaff_RepoErrors(t *testing.T) {
	sentinel := errors.New("boom")
	t.Run("emailExists", func(t *testing.T) {
		repo := &fakeConsoleRepo{errEmailExists: sentinel}
		if _, err := newConsoleSvc(repo).CreateStaff(authedCtx(), CreateStaffInput{Email: "x@example.com", Role: "e"}); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("create", func(t *testing.T) {
		repo := &fakeConsoleRepo{errCreate: sentinel}
		if _, err := newConsoleSvc(repo).CreateStaff(authedCtx(), CreateStaffInput{Email: "x@example.com", Role: "e"}); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestConsole_ListAlerts(t *testing.T) {
	repo := &fakeConsoleRepo{alerts: []repository.Alert{
		{ID: uuid.New(), Title: "Disk full", AlertType: "warning", Read: false, CreatedAt: time.Now().UTC()},
	}}
	got, err := newConsoleSvc(repo).ListAlerts(authedCtx(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "Disk full" {
		t.Fatalf("unexpected alerts: %+v", got)
	}
}

func TestConsole_ListAlerts_RepoError(t *testing.T) {
	repo := &fakeConsoleRepo{errAlerts: errors.New("boom")}
	if _, err := newConsoleSvc(repo).ListAlerts(authedCtx(), 5); err == nil {
		t.Fatal("expected error")
	}
}

func TestConsole_GetReportsSummary(t *testing.T) {
	repo := &fakeConsoleRepo{statusCounts: repository.AppStatusCounts{Active: 4, Paused: 1}}
	got, err := newConsoleSvc(repo).GetReportsSummary(authedCtx())
	if err != nil {
		t.Fatal(err)
	}
	if got.ActiveApps != 4 || got.PausedApps != 1 || got.MRR == "" {
		t.Fatalf("unexpected reports: %+v", got)
	}
}

func TestConsole_GetReportsSummary_RepoError(t *testing.T) {
	repo := &fakeConsoleRepo{errStatusCounts: errors.New("boom")}
	if _, err := newConsoleSvc(repo).GetReportsSummary(authedCtx()); err == nil {
		t.Fatal("expected error")
	}
}
