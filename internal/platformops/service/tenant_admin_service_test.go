package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	tenantdomain "github.com/williamlabdev/saas-forge/internal/tenant/domain"
)

type fakePlanWriter struct {
	err  error
	slug string
	plan string
}

func (f *fakePlanWriter) SetTenantPlan(_ context.Context, slug, plan string) error {
	f.slug, f.plan = slug, plan
	return f.err
}

func adminCtx() context.Context {
	return authn.WithSubject(context.Background(), authn.Subject{UserID: uuid.New(), Roles: []string{"admin"}})
}

func nonAdminCtx() context.Context {
	return authn.WithSubject(context.Background(), authn.Subject{UserID: uuid.New(), TenantID: "t_x", TenantRole: "owner"})
}

func TestSetPlan_AdminSucceeds(t *testing.T) {
	w := &fakePlanWriter{}
	svc := NewTenantAdminService(w, authz.NewRBACAuthorizer())
	dto, err := svc.SetPlan(adminCtx(), "t_acme", "pro")
	require.NoError(t, err)
	assert.Equal(t, TenantPlanDTO{TenantID: "t_acme", Plan: "pro"}, dto)
	assert.Equal(t, "t_acme", w.slug)
	assert.Equal(t, "pro", w.plan)
}

func TestSetPlan_NonAdminForbidden(t *testing.T) {
	w := &fakePlanWriter{}
	svc := NewTenantAdminService(w, authz.NewRBACAuthorizer())
	_, err := svc.SetPlan(nonAdminCtx(), "t_acme", "pro")
	assert.ErrorIs(t, err, apperrors.ErrForbidden)
	assert.Empty(t, w.slug, "must not touch the repo when forbidden")
}

func TestSetPlan_Unauthenticated(t *testing.T) {
	svc := NewTenantAdminService(&fakePlanWriter{}, authz.NewRBACAuthorizer())
	_, err := svc.SetPlan(context.Background(), "t_acme", "pro")
	assert.ErrorIs(t, err, apperrors.ErrUnauthorized)
}

func TestSetPlan_ValidationAndRepoErrorsPropagate(t *testing.T) {
	svc := NewTenantAdminService(&fakePlanWriter{}, authz.NewRBACAuthorizer())
	_, err := svc.SetPlan(adminCtx(), "t_acme", "")
	require.Error(t, err, "empty plan rejected")

	// Repo errors (unknown plan / tenant) surface unchanged.
	svc = NewTenantAdminService(&fakePlanWriter{err: tenantdomain.ErrPlanUnknown}, authz.NewRBACAuthorizer())
	_, err = svc.SetPlan(adminCtx(), "t_acme", "bogus")
	assert.ErrorIs(t, err, tenantdomain.ErrPlanUnknown)

	svc = NewTenantAdminService(&fakePlanWriter{err: apperrors.ErrNotFound}, authz.NewRBACAuthorizer())
	_, err = svc.SetPlan(adminCtx(), "t_missing", "pro")
	assert.True(t, errors.Is(err, apperrors.ErrNotFound))
}
