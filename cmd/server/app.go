package main

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/williamlabdev/saas-forge/internal/cms/content/mediatransform"
	contentrepo "github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	contentscheduler "github.com/williamlabdev/saas-forge/internal/cms/content/scheduler"
	contentservice "github.com/williamlabdev/saas-forge/internal/cms/content/service"
	iamservice "github.com/williamlabdev/saas-forge/internal/iam/service"
	"github.com/williamlabdev/saas-forge/internal/pkg/config"
	"github.com/williamlabdev/saas-forge/internal/pkg/outbox"
)

type app struct {
	Server *http.Server
	Pool   *pgxpool.Pool
	Worker *outbox.Worker
	// Scheduler executes due publish/unpublish schedules (ADR-017); main runs it
	// on the same shutdown context as the outbox worker.
	Scheduler *contentscheduler.Worker
	// MediaTransform renders image variants (ADR-019); nil when media is not
	// configured, and main skips it then.
	MediaTransform *mediatransform.Worker
	Runtime        config.Runtime
	IAM            iamservice.IAMService
	// DeliveryCounter buffers public delivery read volume; main runs its
	// flusher alongside the outbox worker. ContentRepo is the flush sink.
	DeliveryCounter *contentservice.DeliveryCounter
	ContentRepo     *contentrepo.PostgresContentRepository
}
