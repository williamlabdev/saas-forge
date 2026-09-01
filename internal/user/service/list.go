package service

import (
	"context"
	"strings"

	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
	"github.com/williamlabdev/saas-forge/internal/pkg/pagination"
)

// ListInput is the admin user list query (cursor pagination).
type ListInput struct {
	Limit  int
	Cursor string
	Status string
}

// ListResult is a page of users with pagination meta.
type ListResult struct {
	Items []*UserDTO
	Page  pagination.PageMeta
}

func (s *userService) List(ctx context.Context, in ListInput) (*ListResult, error) {
	if err := s.authz.Allow(ctx, authz.Input{
		Action: authz.ActionUserList,
		Resource: authz.Resource{
			Type: "users",
			ID:   "collection",
		},
	}); err != nil {
		return nil, err
	}

	statusFilter, err := normalizeListStatus(in.Status)
	if err != nil {
		return nil, err
	}

	limit := pagination.ClampLimit(in.Limit)
	var cursor *pagination.UserCursor
	if raw := strings.TrimSpace(in.Cursor); raw != "" {
		c, err := pagination.DecodeUserCursor(raw)
		if err != nil {
			return nil, err
		}
		cursor = &c
	}

	users, err := s.repo.ListPage(ctx, statusFilter, cursor, limit+1)
	if err != nil {
		return nil, err
	}

	hasMore := len(users) > limit
	if hasMore {
		users = users[:limit]
	}

	items := make([]*UserDTO, len(users))
	for i, u := range users {
		items[i] = toDTO(u, true)
	}

	page := pagination.PageMeta{
		Limit:   limit,
		HasMore: hasMore,
	}
	if hasMore && len(users) > 0 {
		last := users[len(users)-1]
		token, err := pagination.EncodeUserCursor(pagination.UserCursor{
			CreatedAt: last.CreatedAt,
			ID:        last.ID,
		})
		if err != nil {
			return nil, err
		}
		page.NextCursor = token
	}

	return &ListResult{Items: items, Page: page}, nil
}

func normalizeListStatus(raw string) (string, error) {
	s := strings.TrimSpace(strings.ToLower(raw))
	if s == "" {
		return "active", nil
	}
	switch s {
	case "active", "suspended", "deleted", "all":
		return s, nil
	default:
		return "", apperrors.Wrap("INVALID_STATUS_FILTER", "status must be active, suspended, deleted, or all", 400, nil)
	}
}
