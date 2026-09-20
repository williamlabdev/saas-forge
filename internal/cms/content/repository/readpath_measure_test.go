package repository

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Measurement harness, not a gate. It answers "what does a read cost, and how
// does that cost move with the data" — the numbers a slice plan needs so the
// order of work follows the pain rather than a guess.
//
// Gated on MEASURE=1 because it seeds thousands of rows and reports rather than
// asserts: a test that cannot fail has no business spending CI's time. Run with
//
//	MEASURE=1 go test ./internal/cms/content/repository/ -run TestMeasure -v
//
// Read the numbers as SHAPE, not as absolutes — a container on a laptop is not
// production. The question each one answers is "does this grow with N", and
// that answer survives the move.

func measureEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("MEASURE") != "1" {
		t.Skip("measurement harness — set MEASURE=1 to run")
	}
}

// timeIt runs fn n times and returns the mean.
func timeIt(t *testing.T, n int, fn func()) time.Duration {
	t.Helper()
	fn() // warm the pool and the plan cache; the first call is not the steady state
	start := time.Now()
	for range n {
		fn()
	}
	return time.Since(start) / time.Duration(n)
}

// The schema read is on the hot path: every delivery request resolves the
// content type by name BEFORE it can read a single entry. This measures what
// that resolution costs and — more importantly — whether it grows with the
// number of fields on the type, since the fields come back through a second
// query.
func TestMeasure_ContentTypeResolution(t *testing.T) {
	measureEnabled(t)
	ctx, pool, _ := startContentDB(t, "measure_schema")
	repo := NewPostgresContentRepository(pool, nil)

	t.Log("GetContentTypeByName — the per-request schema resolution")
	for _, nFields := range []int{1, 10, 30, 60} {
		name := fmt.Sprintf("t%d", nFields)
		fields := make([][2]string, nFields)
		for i := range fields {
			fields[i] = [2]string{fmt.Sprintf("f%d", i), "text"}
		}
		mkType(t, ctx, repo, "perf", name, fields...)

		d := timeIt(t, 200, func() {
			if _, err := repo.GetContentTypeByName(ctx, "perf", name); err != nil {
				t.Fatal(err)
			}
		})
		t.Logf("  %3d fields: %8s / call", nFields, d.Round(time.Microsecond))
	}
}

// ListContentTypes reads every type's fields in one batched query (S3). This
// measures how that holds as the tenant's schema grows — the admin console's
// type list is the caller, and a tenant with a real content model is the case
// that matters. Before S3 it loaded fields one type at a time and the cost was
// linear (~145µs per type); the shape to look for now is flat.
func TestMeasure_ListContentTypesScaling(t *testing.T) {
	measureEnabled(t)
	ctx, pool, _ := startContentDB(t, "measure_typelist")
	repo := NewPostgresContentRepository(pool, nil)

	t.Log("ListContentTypes — one query for the types, one more for all their fields")
	made := 0
	for _, target := range []int{1, 5, 10, 25, 50} {
		for ; made < target; made++ {
			mkType(t, ctx, repo, "perf", fmt.Sprintf("t%d", made),
				[2]string{"title", "text"}, [2]string{"body", "text"}, [2]string{"slug", "text"})
		}
		d := timeIt(t, 50, func() {
			if _, err := repo.ListContentTypes(ctx, "perf"); err != nil {
				t.Fatal(err)
			}
		})
		t.Logf("  %3d types: %8s / call  (%s per type)",
			target, d.Round(time.Microsecond), (d / time.Duration(target)).Round(time.Microsecond))
	}
}

// The delivery read. Cursor mode skips COUNT(*) by design (ADR/OD2-024), so the
// question this answers is whether the page read itself stays flat as the
// collection grows — a page of 20 should cost the same at 100 entries and at
// 20,000 if the index is doing its job.
func TestMeasure_DeliveryListScaling(t *testing.T) {
	measureEnabled(t)
	ctx, pool, _ := startContentDB(t, "measure_entries")
	repo := NewPostgresContentRepository(pool, nil)

	ct := mkType(t, ctx, repo, "perf", "article",
		[2]string{"title", "text"}, [2]string{"body", "text"})

	page := func(cursorPaged bool) ListEntriesFilter {
		return ListEntriesFilter{
			TenantID: "perf", ContentTypeID: ct.ID,
			Status: "published", Limit: 20, CursorPaged: cursorPaged,
		}
	}

	seeded := 0
	for _, target := range []int{100, 1000, 5000, 20000} {
		batch := make([]entrySeed, 0, target-seeded)
		for ; seeded < target; seeded++ {
			body := fmt.Sprintf(`{"title":"post %d","body":"%s"}`, seeded, uuid.NewString())
			batch = append(batch, entrySeed{payload: body, version: 1, publishedPayload: body, publishedVersion: 1})
		}
		seedEntries(t, ctx, pool, "perf", ct.ID, batch...)
		// ANALYZE before timing, every round. Without it the planner is choosing
		// from statistics gathered when the table was a fraction of this size,
		// and the "cost" being measured is that staleness rather than anything
		// the product does. Autovacuum would get there eventually; a test that
		// seeds and immediately reads cannot wait for eventually.
		if _, err := pool.Exec(ctx, "ANALYZE entries"); err != nil {
			t.Fatal(err)
		}

		cursor := timeIt(t, 50, func() {
			if _, _, err := repo.ListEntries(ctx, page(true)); err != nil {
				t.Fatal(err)
			}
		})
		offset := timeIt(t, 50, func() {
			if _, _, err := repo.ListEntries(ctx, page(false)); err != nil {
				t.Fatal(err)
			}
		})
		t.Logf("  %6d entries: cursor(delivery) %9s | offset+COUNT(admin) %9s | COUNT costs %s",
			target, cursor.Round(time.Microsecond), offset.Round(time.Microsecond),
			(offset - cursor).Round(time.Microsecond))

		// A timing that grows is a symptom; the plan is the cause. Printed at the
		// largest size only, where a sequential scan has somewhere to hide.
		if target == 20000 {
			rows, err := pool.Query(ctx, `EXPLAIN (ANALYZE, BUFFERS)
				SELECT id FROM entries
				WHERE tenant_id = $1 AND content_type_id = $2 AND status = 'published'
				ORDER BY created_at DESC, id DESC LIMIT 20`, "perf", ct.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			t.Log("  plan for the delivery page at 20k:")
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					t.Fatal(err)
				}
				t.Logf("    %s", line)
			}
			rows.Close()

			// The candidate fix, tried here rather than argued: the existing index
			// stops at created_at, but every ordering this repository builds ends
			// in `id DESC` and the keyset predicate compares the pair
			// `(created_at, id)`. One trailing column short of serving either.
			if _, err := pool.Exec(ctx, `CREATE INDEX idx_probe_full_order
				ON entries (tenant_id, content_type_id, status, created_at DESC, id DESC)`); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, "ANALYZE entries"); err != nil {
				t.Fatal(err)
			}
			withIdx := timeIt(t, 50, func() {
				if _, _, err := repo.ListEntries(ctx, page(true)); err != nil {
					t.Fatal(err)
				}
			})
			t.Logf("  WITH the candidate index: cursor(delivery) %s  (was %s)",
				withIdx.Round(time.Microsecond), cursor.Round(time.Microsecond))

			probe, err := pool.Query(ctx, `EXPLAIN (ANALYZE, BUFFERS)
				SELECT id FROM entries
				WHERE tenant_id = $1 AND content_type_id = $2 AND status = 'published'
				ORDER BY created_at DESC, id DESC LIMIT 20`, "perf", ct.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer probe.Close()
			t.Log("  plan WITH the candidate index:")
			for probe.Next() {
				var line string
				if err := probe.Scan(&line); err != nil {
					t.Fatal(err)
				}
				t.Logf("    %s", line)
			}
		}
	}
}
