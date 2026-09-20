package repository

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// mustDSN re-derives the connection string startContentDB used, so this test
// can open a second pool onto the same database with a tracer attached.
func mustDSN(t *testing.T, ctx context.Context, c *postgres.PostgresContainer) string {
	t.Helper()
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	return dsn
}

// countingTracer records the SQL of every statement the pool executes. It is
// the only way to state the property this slice is actually about: not "the
// list is fast" (a machine-speed claim that would pass on a fast enough box
// however many round trips it takes) but "the list reads the fields in one
// query, whatever the tenant's schema looks like".
type countingTracer struct {
	mu   sync.Mutex
	sqls []string
}

func (c *countingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sqls = append(c.sqls, data.SQL)
	return ctx
}

func (c *countingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *countingTracer) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sqls = nil
}

// matching counts statements mentioning table. It matches on the table name and
// not on any string the repository builds, so a rewrite of the query keeps the
// assertion honest: what is being counted is round trips to that table.
func (c *countingTracer) matching(table string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, s := range c.sqls {
		if strings.Contains(s, table) {
			n++
		}
	}
	return n
}

// ListContentTypes loaded fields one type at a time, which put the admin
// console's type list on a curve that followed the tenant's content model
// (~145µs per type, 8ms at 50 — docs/readpath-optimisation-slices.md §1.3).
//
// Two things have to hold at once, and the second is why the first is safe:
// one query for all the fields, AND every type still holding exactly its own
// fields in their defined order. Batching is only correct if the grouping is,
// and the fixture below is built to make a broken grouping visible — the four
// types have overlapping field keys and their created_at offsets interleave,
// so rows come back from the single query in an order that does not follow
// type boundaries. A grouping that leaned on arrival order would split them
// wrong; a grouping that dropped the last type would still pass a count check.
func TestListContentTypes_ReadsFieldsInOneQuery(t *testing.T) {
	ctx, pool, container := startContentDB(t, "typelist_fanout")

	setup := NewPostgresContentRepository(pool, nil)
	mkType(t, ctx, setup, "t1", "alpha",
		[2]string{"title", "text"}, [2]string{"body", "text"}, [2]string{"slug", "text"})
	mkType(t, ctx, setup, "t1", "beta", [2]string{"title", "text"})
	mkType(t, ctx, setup, "t1", "gamma",
		[2]string{"hero", "file"}, [2]string{"body", "text"})
	mkType(t, ctx, setup, "t1", "delta")
	// A second tenant, so the batch is also asked not to reach outside the
	// types the tenant-scoped query returned.
	mkType(t, ctx, setup, "t2", "intruder", [2]string{"secret", "text"})

	tracer := &countingTracer{}
	cfg, err := pgxpool.ParseConfig(mustDSN(t, ctx, container))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = tracer
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)

	repo := NewPostgresContentRepository(traced, nil)
	tracer.reset()

	got, err := repo.ListContentTypes(ctx, "t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if n := tracer.matching("content_type_fields"); n != 1 {
		t.Errorf("content_type_fields was queried %d times, want 1 (N+1 is back)", n)
	}

	want := map[string][]string{
		"alpha": {"title", "body", "slug"},
		"beta":  {"title"},
		"gamma": {"hero", "body"},
		"delta": nil,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d types, want %d", len(got), len(want))
	}
	for _, ct := range got {
		keys, ok := want[ct.Name]
		if !ok {
			t.Errorf("unexpected type %q (tenant leak?)", ct.Name)
			continue
		}
		var have []string
		for _, f := range ct.Fields {
			have = append(have, f.Key)
		}
		if strings.Join(have, ",") != strings.Join(keys, ",") {
			t.Errorf("type %s: fields %v, want %v", ct.Name, have, keys)
		}
		for _, f := range ct.Fields {
			if f.ContentTypeID != ct.ID {
				t.Errorf("type %s: field %s belongs to %s", ct.Name, f.Key, f.ContentTypeID)
			}
		}
	}
}
