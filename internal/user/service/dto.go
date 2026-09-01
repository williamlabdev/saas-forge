package service

import (
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/user/domain"
)

type UserDTO struct {
	ID          uuid.UUID          `json:"id"`
	Username    string             `json:"username"`
	Email       string             `json:"email,omitempty"`
	DisplayName string             `json:"display_name,omitempty"`
	Phone       string             `json:"phone,omitempty"`
	Preferences domain.Preferences `json:"preferences"`
	Status      domain.Status      `json:"status"`
	CreatedAt   time.Time          `json:"created_at"`
	UpdatedAt   time.Time          `json:"updated_at"`
}

type RegisterInput struct {
	Username       string             `validate:"required,min=3,max=64"`
	Email          string             `validate:"required,email,max=254"`
	Password       string             `validate:"required,min=8,max=128"`
	IdempotencyKey string             `validate:"omitempty"`
	DisplayName    string             `validate:"omitempty,max=128"`
	Phone          string             `validate:"omitempty,max=32"`
	Preferences    domain.Preferences `validate:"omitempty"`
}

type UpdateProfileInput struct {
	DisplayName *string `validate:"omitempty,max=128"`
	Phone       *string `validate:"omitempty,max=32"`
}

type PreferencesInput struct {
	Merge       bool
	Preferences domain.Preferences `validate:"required"`
}

func toDTO(u *domain.User, exposePII bool) *UserDTO {
	dto := &UserDTO{
		ID:          u.ID,
		Username:    u.Username,
		Preferences: u.Preferences,
		Status:      u.Status,
		CreatedAt:   u.CreatedAt,
		UpdatedAt:   u.UpdatedAt,
	}
	if exposePII {
		dto.Email = u.Email
		dto.DisplayName = u.DisplayName
		dto.Phone = u.Phone
	}
	if dto.Preferences == nil {
		dto.Preferences = domain.Preferences{}
	}
	return dto
}
