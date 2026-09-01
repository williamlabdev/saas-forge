package authz

import "context"

// AllowAllAuthorizer permits every action (local dev only).
type AllowAllAuthorizer struct{}

func NewAllowAllAuthorizer() *AllowAllAuthorizer {
	return &AllowAllAuthorizer{}
}

func (a *AllowAllAuthorizer) Allow(context.Context, Input) error {
	return nil
}
