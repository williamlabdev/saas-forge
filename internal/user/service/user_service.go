package service

import (
	"context"
	"time"

	"github.com/google/uuid"
	authservice "github.com/williamlabdev/saas-forge/internal/auth/service"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	"github.com/williamlabdev/saas-forge/internal/pkg/crypto"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/idempotency"
	"github.com/williamlabdev/saas-forge/internal/pkg/validate"
	"github.com/williamlabdev/saas-forge/internal/user/domain"
	"github.com/williamlabdev/saas-forge/internal/user/repository"
)

// UserService exposes user use cases.
type UserService interface {
	Register(ctx context.Context, in RegisterInput) (*UserDTO, bool /*replayed*/, error)
	List(ctx context.Context, in ListInput) (*ListResult, error)
	CurrentUser(ctx context.Context) (*UserDTO, error)
	ByID(ctx context.Context, id uuid.UUID) (*UserDTO, error)
	UpdateProfile(ctx context.Context, id uuid.UUID, in UpdateProfileInput) (*UserDTO, error)
	UpdatePreferences(ctx context.Context, id uuid.UUID, in PreferencesInput) (*UserDTO, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

type userService struct {
	repo  repository.UserRepository
	idx   crypto.BlindIndexer
	authz authz.Authorizer
	auth  authservice.AuthService
	idem  idempotency.RegistrationStore
}

// NewUserService wires dependencies via constructor injection.
func NewUserService(repo repository.UserRepository, idx crypto.BlindIndexer, authz authz.Authorizer, auth authservice.AuthService, idem idempotency.RegistrationStore) UserService {
	return &userService{repo: repo, idx: idx, authz: authz, auth: auth, idem: idem}
}

func (s *userService) Register(ctx context.Context, in RegisterInput) (*UserDTO, bool, error) {
	if err := validate.Struct(in); err != nil {
		return nil, false, mapValidationErr(err)
	}

	idemKey, err := idempotency.NormalizeKey(in.IdempotencyKey)
	if err != nil {
		return nil, false, err
	}
	if idemKey != "" && s.idem != nil {
		if uid, ok, err := s.idem.UserIDByKey(ctx, idemKey); err != nil {
			return nil, false, err
		} else if ok {
			u, err := s.repo.ByID(ctx, uid)
			if err != nil {
				return nil, false, err
			}
			return toDTO(u, true), true, nil
		}
	}

	username, err := normalizeUsername(in.Username)
	if err != nil {
		return nil, false, err
	}
	email, err := normalizeEmail(in.Email)
	if err != nil {
		return nil, false, err
	}
	if err := validatePreferences(in.Preferences); err != nil {
		return nil, false, err
	}

	usernameHash, err := s.idx.Index(username)
	if err != nil {
		return nil, false, err
	}
	emailHash, err := s.idx.Index(email)
	if err != nil {
		return nil, false, err
	}

	if err := s.assertUnique(ctx, emailHash, usernameHash); err != nil {
		return nil, false, err
	}

	now := time.Now().UTC()
	prefs := in.Preferences
	if prefs == nil {
		prefs = domain.Preferences{}
	}

	u := &domain.User{
		ID:                 uuid.New(),
		Username:           username,
		Email:              email,
		DisplayName:        in.DisplayName,
		Phone:              in.Phone,
		Preferences:        prefs,
		Status:             domain.StatusActive,
		StatusVersion:      1,
		UsernameLookupHash: usernameHash,
		EmailLookupHash:    emailHash,
		CreatedAt:          now,
		UpdatedAt:          now,
	}

	passwordHash, err := s.auth.HashPassword(in.Password)
	if err != nil {
		return nil, false, err
	}
	if err := s.repo.Create(ctx, u, passwordHash, idemKey); err != nil {
		if idemKey != "" && idempotency.IsUniqueViolation(err) && s.idem != nil {
			if uid, ok, lookupErr := s.idem.UserIDByKey(ctx, idemKey); lookupErr == nil && ok {
				existing, byErr := s.repo.ByID(ctx, uid)
				if byErr != nil {
					return nil, false, byErr
				}
				return toDTO(existing, true), true, nil
			}
		}
		return nil, false, err
	}
	// D12: no global "member" role on registration — the platform plane keeps
	// only operator roles; in-tenant capability is the owner membership that
	// repo.Create provisioned atomically (D8).
	return toDTO(u, true), false, nil
}

func (s *userService) CurrentUser(ctx context.Context) (*UserDTO, error) {
	sub, ok := authn.SubjectFromContext(ctx)
	if !ok {
		return nil, apperrors.ErrUnauthorized
	}
	return s.ByID(ctx, sub.UserID)
}

func (s *userService) ByID(ctx context.Context, id uuid.UUID) (*UserDTO, error) {
	if err := s.authorize(ctx, authz.ActionUserRead, id); err != nil {
		return nil, err
	}
	u, err := s.repo.ByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if u.Status == domain.StatusDeleted || u.DeletedAt != nil {
		return nil, apperrors.ErrNotFound
	}
	return toDTO(u, true), nil
}

func (s *userService) UpdateProfile(ctx context.Context, id uuid.UUID, in UpdateProfileInput) (*UserDTO, error) {
	if err := validate.Struct(in); err != nil {
		return nil, mapValidationErr(err)
	}
	if err := s.authorize(ctx, authz.ActionUserUpdate, id); err != nil {
		return nil, err
	}

	u, err := s.repo.ByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.guardMutable(u); err != nil {
		return nil, err
	}

	if in.DisplayName != nil {
		u.DisplayName = *in.DisplayName
	}
	if in.Phone != nil {
		u.Phone = *in.Phone
	}

	if err := s.repo.Update(ctx, u); err != nil {
		return nil, err
	}
	if err := s.repo.PublishUserUpdated(ctx, u); err != nil {
		return nil, err
	}
	updated, err := s.repo.ByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return toDTO(updated, true), nil
}

func (s *userService) UpdatePreferences(ctx context.Context, id uuid.UUID, in PreferencesInput) (*UserDTO, error) {
	if err := validate.Struct(in); err != nil {
		return nil, mapValidationErr(err)
	}
	if err := s.authorize(ctx, authz.ActionUserPreferencesUpdate, id); err != nil {
		return nil, err
	}

	u, err := s.repo.ByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.guardMutable(u); err != nil {
		return nil, err
	}

	prefs := in.Preferences
	if err := validatePreferences(prefs); err != nil {
		return nil, err
	}

	if in.Merge {
		if err := s.repo.UpdatePreferences(ctx, id, prefs, true); err != nil {
			return nil, err
		}
	} else if err := s.repo.UpdatePreferences(ctx, id, prefs, false); err != nil {
		return nil, err
	}
	if err := s.repo.PublishUserUpdated(ctx, u); err != nil {
		return nil, err
	}

	updated, err := s.repo.ByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return toDTO(updated, true), nil
}

func (s *userService) Delete(ctx context.Context, id uuid.UUID) error {
	if err := s.authorize(ctx, authz.ActionUserDelete, id); err != nil {
		return err
	}
	u, err := s.repo.ByID(ctx, id)
	if err != nil {
		return err
	}
	if u.Status == domain.StatusDeleted {
		return apperrors.ErrNotFound
	}
	return s.repo.SoftDelete(ctx, id)
}

func (s *userService) assertUnique(ctx context.Context, emailHash, usernameHash []byte) error {
	existing, err := s.repo.ByEmailHash(ctx, emailHash)
	if err != nil && !isNotFound(err) {
		return err
	}
	if existing != nil && existing.Status != domain.StatusDeleted {
		return apperrors.ErrEmailTaken
	}

	existing, err = s.repo.ByUsernameHash(ctx, usernameHash)
	if err != nil && !isNotFound(err) {
		return err
	}
	if existing != nil && existing.Status != domain.StatusDeleted {
		return apperrors.ErrUsernameTaken
	}
	return nil
}

func isNotFound(err error) bool {
	ae, ok := apperrors.As(err)
	return ok && ae.Code == apperrors.ErrNotFound.Code
}

func (s *userService) guardMutable(u *domain.User) error {
	if u.Status == domain.StatusDeleted || u.DeletedAt != nil {
		return apperrors.ErrNotFound
	}
	if u.Status == domain.StatusSuspended {
		return apperrors.ErrSuspended
	}
	return nil
}

func mapValidationErr(err error) error {
	return apperrors.Wrap("VALIDATION_FAILED", err.Error(), 400, err)
}
