package platform

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/williamlabdev/saas-forge/internal"
	"github.com/williamlabdev/saas-forge/internal/platform/migrate"
)

// OpenVerifiedPool is the only way a server gets a database handle.
//
// There are two composition roots in this repository (BuildApp here, and Wire's
// graph in cmd/server), which is exactly the shape where a check gets added to
// one and forgotten in the other. Rather than remembering to call Verify twice,
// the check lives inside the thing both of them must call to get a pool at all,
// and db_guard_test.go fails if anything else constructs one.
//
// It verifies; it never applies. cmd/migrate is the only binary that runs DDL —
// see ADR-012 §4 for why booting must not be a way to change the schema.
func OpenVerifiedPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}

	ms, err := migrate.Discover(internal.MigrationsFS)
	if err != nil {
		pool.Close()
		return nil, err
	}
	if err := migrate.Verify(ctx, pool, ms); err != nil {
		pool.Close()
		// Refusing to start IS the feature: the alternative this replaces was
		// serving happily on a schema that was missing things, and finding out
		// from a 500 days later.
		return nil, fmt.Errorf("refusing to start: %w", err)
	}
	return pool, nil
}
