package main

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	contentrepo "github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	contentservice "github.com/williamlabdev/saas-forge/internal/cms/content/service"
	iamservice "github.com/williamlabdev/saas-forge/internal/iam/service"
	"github.com/williamlabdev/saas-forge/internal/pkg/config"
	"github.com/williamlabdev/saas-forge/internal/pkg/outbox"
)

type app struct {
	Server  *http.Server
	Pool    *pgxpool.Pool
	Worker  *outbox.Worker
	Runtime config.Runtime
	IAM     iamservice.IAMService
	// DeliveryCounter buffers public delivery read volume; main runs its
	// flusher alongside the outbox worker. ContentRepo is the flush sink.
	DeliveryCounter *contentservice.DeliveryCounter
	ContentRepo     *contentrepo.PostgresContentRepository
}
