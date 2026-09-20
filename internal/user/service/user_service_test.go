package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authservice "github.com/williamlabdev/saas-forge/internal/auth/service"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	"github.com/williamlabdev/saas-forge/internal/pkg/crypto"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/pagination"
	"github.com/williamlabdev/saas-forge/internal/user/domain"
	"github.com/williamlabdev/saas-forge/internal/user/repository"
)

type mockUserRepository struct {
	users map[uuid.UUID]*domain.User

	createFn             func(context.Context, *domain.User, string, string) error
	byIDFn               func(context.Context, uuid.UUID) (*domain.User, error)
	byEmailHashFn        func(context.Context, []byte) (*domain.User, error)
	byUsernameHashFn     func(context.Context, []byte) (*domain.User, error)
	updateFn             func(context.Context, *domain.User) error
	updatePreferencesFn  func(context.Context, uuid.UUID, domain.Preferences, bool) error
	softDeleteFn         func(context.Context, uuid.UUID) error
	publishUserUpdatedFn func(context.Context, *domain.User) error
	publishUserDeletedFn func(context.Context, *domain.User) error
	listPageFn           func(context.Context, string, *pagination.UserCursor, int) ([]*domain.User, error)
}

func newMockRepo() *mockUserRepository {
	return &mockUserRepository{users: make(map[uuid.UUID]*domain.User)}
}

type stubAuthService struct{}

func (stubAuthService) HashPassword(string) (string, error) { return "argon2id$test", nil }
func (stubAuthService) Login(context.Context, authservice.LoginInput) (*authservice.TokenResponse, error) {
	return nil, nil
}
func (stubAuthService) Refresh(context.Context, string) (*authservice.TokenResponse, error) {
	return nil, nil
}
func (stubAuthService) SwitchTenant(context.Context, uuid.UUID, string) (*authservice.TokenResponse, error) {
	return nil, nil
}
func (stubAuthService) Logout(context.Context, string) error { return nil }

func (m *mockUserRepository) Create(ctx context.Context, u *domain.User, _, _ string) error {
	if m.createFn != nil {
		return m.createFn(ctx, u, "", "")
	}
	cp := *u
	m.users[u.ID] = &cp
	return nil
}

func (m *mockUserRepository) ByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	if m.byIDFn != nil {
		return m.byIDFn(ctx, id)
	}
	u, ok := m.users[id]
	if !ok {
		return nil, apperrors.ErrNotFound
	}
	cp := *u
	return &cp, nil
}

func (m *mockUserRepository) ByEmailHash(ctx context.Context, hash []byte) (*domain.User, error) {
	if m.byEmailHashFn != nil {
		return m.byEmailHashFn(ctx, hash)
	}
	for _, u := range m.users {
		if string(u.EmailLookupHash) == string(hash) {
			cp := *u
			return &cp, nil
		}
	}
	return nil, apperrors.ErrNotFound
}

func (m *mockUserRepository) ByUsernameHash(ctx context.Context, hash []byte) (*domain.User, error) {
	if m.byUsernameHashFn != nil {
		return m.byUsernameHashFn(ctx, hash)
	}
	for _, u := range m.users {
		if string(u.UsernameLookupHash) == string(hash) {
			cp := *u
			return &cp, nil
		}
	}
	return nil, apperrors.ErrNotFound
}

func (m *mockUserRepository) Update(ctx context.Context, u *domain.User) error {
	if m.updateFn != nil {
		return m.updateFn(ctx, u)
	}
	if _, ok := m.users[u.ID]; !ok {
		return apperrors.ErrNotFound
	}
	cp := *u
	m.users[u.ID] = &cp
	return nil
}

func (m *mockUserRepository) UpdatePreferences(ctx context.Context, id uuid.UUID, prefs domain.Preferences, merge bool) error {
	if m.updatePreferencesFn != nil {
		return m.updatePreferencesFn(ctx, id, prefs, merge)
	}
	u, ok := m.users[id]
	if !ok {
		return apperrors.ErrNotFound
	}
	if merge {
		for k, v := range prefs {
			u.Preferences[k] = v
		}
	} else {
		u.Preferences = prefs
	}
	return nil
}

func (m *mockUserRepository) PublishUserUpdated(ctx context.Context, u *domain.User) error {
	if m.publishUserUpdatedFn != nil {
		return m.publishUserUpdatedFn(ctx, u)
	}
	return nil
}

func (m *mockUserRepository) ListPage(ctx context.Context, statusFilter string, cursor *pagination.UserCursor, limit int) ([]*domain.User, error) {
	if m.listPageFn != nil {
		return m.listPageFn(ctx, statusFilter, cursor, limit)
	}
	return nil, nil
}

func (m *mockUserRepository) PublishUserDeleted(ctx context.Context, u *domain.User) error {
	if m.publishUserDeletedFn != nil {
		return m.publishUserDeletedFn(ctx, u)
	}
	return nil
}

func (m *mockUserRepository) SoftDelete(ctx context.Context, id uuid.UUID) error {
	if m.softDeleteFn != nil {
		return m.softDeleteFn(ctx, id)
	}
	u, ok := m.users[id]
	if !ok {
		return apperrors.ErrNotFound
	}
	now := time.Now().UTC()
	u.Status = domain.StatusDeleted
	u.DeletedAt = &now
	return nil
}

func testIndexer(t *testing.T) crypto.BlindIndexer {
	t.Helper()
	idx, err := crypto.NewHMACBlindIndexer(bytesRepeat(32, 0xab))
	require.NoError(t, err)
	return idx
}

func bytesRepeat(n int, b byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestUserService_List_AdminOnly(t *testing.T) {
	repo := newMockRepo()
	svc := NewUserService(repo, testIndexer(t), authz.NewRBACAuthorizer(), stubAuthService{}, nil)

	_, err := svc.List(context.Background(), ListInput{})
	ae, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, apperrors.ErrUnauthorized.Code, ae.Code)

	ctx := authn.WithSubject(context.Background(), authn.Subject{UserID: uuid.New(), Roles: []string{"admin"}})
	repo.listPageFn = func(context.Context, string, *pagination.UserCursor, int) ([]*domain.User, error) {
		return []*domain.User{}, nil
	}
	result, err := svc.List(ctx, ListInput{Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, 10, result.Page.Limit)
}

func TestUserService_Register(t *testing.T) {
	repo := newMockRepo()
	svc := NewUserService(repo, testIndexer(t), authz.NewAllowAllAuthorizer(), stubAuthService{}, nil)

	dto, _, err := svc.Register(context.Background(), RegisterInput{
		Username:    "Jane_Doe",
		Email:       "jane@example.com",
		Password:    "password12",
		Preferences: domain.Preferences{"locale": "en-US", "theme": "dark"},
	})
	require.NoError(t, err)
	assert.Equal(t, "jane_doe", dto.Username)
	assert.Equal(t, "jane@example.com", dto.Email)
}

func TestUserService_Register_EmailTaken(t *testing.T) {
	tests := []struct {
		name        string
		firstUser   string
		secondUser  string
		secondEmail string
	}{
		{name: "duplicate email", firstUser: "alice", secondUser: "bob", secondEmail: "alice@example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newMockRepo()
			svc := NewUserService(repo, testIndexer(t), authz.NewAllowAllAuthorizer(), stubAuthService{}, nil)

			_, _, err := svc.Register(context.Background(), RegisterInput{
				Username: tt.firstUser,
				Email:    tt.firstUser + "@example.com",
				Password: "password12",
			})
			require.NoError(t, err)

			_, _, err = svc.Register(context.Background(), RegisterInput{
				Username: tt.secondUser,
				Email:    tt.secondEmail,
				Password: "password12",
			})
			ae, ok := apperrors.As(err)
			require.True(t, ok)
			assert.Equal(t, apperrors.ErrEmailTaken.Code, ae.Code)
		})
	}
}

func TestUserService_ByID_DeletedReturnsNotFound(t *testing.T) {
	repo := newMockRepo()
	svc := NewUserService(repo, testIndexer(t), authz.NewAllowAllAuthorizer(), stubAuthService{}, nil)

	dto, _, err := svc.Register(context.Background(), RegisterInput{
		Username: "carol",
		Email:    "carol@example.com",
		Password: "password12",
	})
	require.NoError(t, err)
	require.NoError(t, svc.Delete(context.Background(), dto.ID))

	_, err = svc.ByID(context.Background(), dto.ID)
	ae, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, apperrors.ErrNotFound.Code, ae.Code)
}

func TestUserService_UpdatePreferences_Merge(t *testing.T) {
	repo := newMockRepo()
	svc := NewUserService(repo, testIndexer(t), authz.NewAllowAllAuthorizer(), stubAuthService{}, nil)

	dto, _, err := svc.Register(context.Background(), RegisterInput{
		Username:    "dave",
		Email:       "dave@example.com",
		Password:    "password12",
		Preferences: domain.Preferences{"locale": "en-US", "theme": "dark"},
	})
	require.NoError(t, err)

	updated, err := svc.UpdatePreferences(context.Background(), dto.ID, PreferencesInput{
		Merge:       true,
		Preferences: domain.Preferences{"theme": "light"},
	})
	require.NoError(t, err)
	assert.Equal(t, "en-US", updated.Preferences["locale"])
	assert.Equal(t, "light", updated.Preferences["theme"])
}

func TestUserService_ByID_ForbiddenWithoutSubject(t *testing.T) {
	repo := newMockRepo()
	svc := NewUserService(repo, testIndexer(t), authz.NewRBACAuthorizer(), stubAuthService{}, nil)

	dto, _, err := svc.Register(context.Background(), RegisterInput{
		Username: "frank",
		Email:    "frank@example.com",
		Password: "password12",
	})
	require.NoError(t, err)

	_, err = svc.ByID(context.Background(), dto.ID)
	ae, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, apperrors.ErrUnauthorized.Code, ae.Code)
}

func TestUserService_ByID_SelfReadWithRBAC(t *testing.T) {
	repo := newMockRepo()
	svc := NewUserService(repo, testIndexer(t), authz.NewRBACAuthorizer(), stubAuthService{}, nil)

	dto, _, err := svc.Register(context.Background(), RegisterInput{
		Username: "grace",
		Email:    "grace@example.com",
		Password: "password12",
	})
	require.NoError(t, err)

	ctx := authn.WithSubject(context.Background(), authn.Subject{UserID: dto.ID, Roles: []string{"member"}})
	got, err := svc.ByID(ctx, dto.ID)
	require.NoError(t, err)
	assert.Equal(t, dto.ID, got.ID)
}

func TestUserService_GuardSuspended(t *testing.T) {
	repo := newMockRepo()
	svc := NewUserService(repo, testIndexer(t), authz.NewAllowAllAuthorizer(), stubAuthService{}, nil)

	dto, _, err := svc.Register(context.Background(), RegisterInput{
		Username: "erin",
		Email:    "erin@example.com",
		Password: "password12",
	})
	require.NoError(t, err)

	u, _ := repo.ByID(context.Background(), dto.ID)
	u.Status = domain.StatusSuspended
	require.NoError(t, repo.Update(context.Background(), u))

	_, err = svc.UpdateProfile(context.Background(), dto.ID, UpdateProfileInput{
		DisplayName: ptr("New Name"),
	})
	ae, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, apperrors.ErrSuspended.Code, ae.Code)
}

func ptr(s string) *string { return &s }

var _ repository.UserRepository = (*mockUserRepository)(nil)
