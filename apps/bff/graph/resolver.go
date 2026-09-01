package graph

import "github.com/williamlabdev/saas-forge/apps/bff/internal/domainapi"

// Resolver implements gqlgen resolvers (thin BFF — delegates to domain REST).
type Resolver struct {
	Client *domainapi.Client
}
