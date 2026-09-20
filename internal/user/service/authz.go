package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
)

func (s *userService) authorize(ctx context.Context, action string, userID uuid.UUID) error {
	return s.authz.Allow(ctx, authz.Input{
		Action: action,
		Resource: authz.Resource{
			Type: "user",
			ID:   userID.String(),
		},
	})
}
