package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/platformops/domain"
)

func newAppSvc(repo *fakeRepo) PlatformAppService {
	return NewPlatformAppService(repo, authz.NewAllowAllAuthorizer())
}

func TestPlatformAppService_List_Unauthenticated(t *testing.T) {
	_, err := newAppSvc(&fakeRepo{}).List(context.Background(), ListInput{})
	if !errors.Is(err, apperrors.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

func TestPlatformAppService_List_Forbidden(t *testing.T) {
	svc := NewPlatformAppService(&fakeRepo{}, denyAuthorizer{})
	_, err := svc.List(authedCtx(), ListInput{})
	if !errors.Is(err, apperrors.ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
}

func TestPlatformAppService_List_LimitDefaulted(t *testing.T) {
	res, err := newAppSvc(&fakeRepo{}).List(authedCtx(), ListInput{Limit: 0})
	if err != nil {
		t.Fatal(err)
	}
	if res.Limit != 20 {
		t.Fatalf("want default limit 20, got %d", res.Limit)
	}
	res, err = newAppSvc(&fakeRepo{}).List(authedCtx(), ListInput{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if res.Limit != 20 {
		t.Fatalf("want clamped limit 20, got %d", res.Limit)
	}
}

func TestPlatformAppService_Create(t *testing.T) {
	repo := &fakeRepo{}
	got, err := newAppSvc(repo).Create(authedCtx(), CreateInput{Name: " App ", TenantID: " t1 "})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "App" || got.TenantID != "t1" || got.Status != domain.StatusActive {
		t.Fatalf("unexpected app: %+v", got)
	}
	if got.Owner == "" {
		t.Fatal("owner should default to subject id")
	}
	if len(repo.apps) != 1 {
		t.Fatalf("expected 1 persisted app, got %d", len(repo.apps))
	}
}

func TestPlatformAppService_Create_ExplicitOwner(t *testing.T) {
	got, err := newAppSvc(&fakeRepo{}).Create(authedCtx(), CreateInput{Name: "App", TenantID: "t1", Owner: "boss"})
	if err != nil || got.Owner != "boss" {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestPlatformAppService_Create_ValidationFails(t *testing.T) {
	_, err := newAppSvc(&fakeRepo{}).Create(authedCtx(), CreateInput{Name: "", TenantID: "t1"})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestPlatformAppService_Create_Forbidden(t *testing.T) {
	svc := NewPlatformAppService(&fakeRepo{}, denyAuthorizer{})
	_, err := svc.Create(authedCtx(), CreateInput{Name: "App", TenantID: "t1"})
	if !errors.Is(err, apperrors.ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
}

func TestPlatformAppService_UpdateStatus(t *testing.T) {
	id := uuid.New()
	repo := &fakeRepo{apps: []domain.PlatformApp{{ID: id, Name: "A", Status: domain.StatusActive}}}
	got, err := newAppSvc(repo).UpdateStatus(authedCtx(), UpdateStatusInput{ID: id, Status: domain.StatusPaused})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusPaused {
		t.Fatalf("want paused, got %s", got.Status)
	}
}

func TestPlatformAppService_UpdateStatus_InvalidStatus(t *testing.T) {
	repo := &fakeRepo{apps: []domain.PlatformApp{{ID: uuid.New()}}}
	_, err := newAppSvc(repo).UpdateStatus(authedCtx(), UpdateStatusInput{ID: uuid.New(), Status: "bogus"})
	if err == nil {
		t.Fatal("expected invalid-status error")
	}
}

func TestPlatformAppService_UpdateStatus_NotFound(t *testing.T) {
	repo := &fakeRepo{} // empty -> UpdateStatus returns "not found"
	_, err := newAppSvc(repo).UpdateStatus(authedCtx(), UpdateStatusInput{ID: uuid.New(), Status: domain.StatusActive})
	var appErr *apperrors.AppError
	if !errors.As(err, &appErr) || appErr.Code != "NOT_FOUND" {
		t.Fatalf("want NOT_FOUND, got %v", err)
	}
}

func TestPlatformAppService_UpdateStatus_Forbidden(t *testing.T) {
	svc := NewPlatformAppService(&fakeRepo{}, denyAuthorizer{})
	_, err := svc.UpdateStatus(authedCtx(), UpdateStatusInput{ID: uuid.New(), Status: domain.StatusActive})
	if !errors.Is(err, apperrors.ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
}

func TestPlatformAppService_List_FilterByStatus(t *testing.T) {
	repo := &fakeRepo{apps: []domain.PlatformApp{
		{ID: uuid.New(), Status: domain.StatusActive},
		{ID: uuid.New(), Status: domain.StatusPaused},
	}}
	res, err := newAppSvc(repo).List(authedCtx(), ListInput{Status: domain.StatusPaused, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].Status != domain.StatusPaused {
		t.Fatalf("unexpected filtered items: %+v", res.Items)
	}
}

// ensure authn import is used even if other tests change.
var _ = authn.Subject{}
