package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	"github.com/williamlabdev/saas-forge/internal/platformops/domain"
	"github.com/williamlabdev/saas-forge/internal/platformops/repository"
)

type fakeRepo struct {
	apps []domain.PlatformApp
}

func (f *fakeRepo) List(_ context.Context, filter repository.ListFilter) (repository.ListResult, error) {
	var out []domain.PlatformApp
	for _, a := range f.apps {
		if filter.Status != "" && a.Status != filter.Status {
			continue
		}
		out = append(out, a)
	}
	return repository.ListResult{Items: out, Total: len(out)}, nil
}

func (f *fakeRepo) Create(_ context.Context, app *domain.PlatformApp) error {
	f.apps = append(f.apps, *app)
	return nil
}

func (f *fakeRepo) UpdateStatus(_ context.Context, id uuid.UUID, status string) (*domain.PlatformApp, error) {
	for i := range f.apps {
		if f.apps[i].ID == id {
			f.apps[i].Status = status
			return &f.apps[i], nil
		}
	}
	return nil, fmt.Errorf("platform app not found")
}

func TestPlatformAppService_List_AllowMode(t *testing.T) {
	repo := &fakeRepo{
		apps: []domain.PlatformApp{
			{ID: uuid.New(), Name: "A", TenantID: "t1", Status: domain.StatusActive},
		},
	}
	svc := NewPlatformAppService(repo, authz.NewAllowAllAuthorizer())
	ctx := authn.WithSubject(context.Background(), authn.Subject{UserID: uuid.New()})

	res, err := svc.List(ctx, ListInput{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("items: got %d", len(res.Items))
	}
}
