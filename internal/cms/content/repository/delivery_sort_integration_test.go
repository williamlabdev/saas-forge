package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// ADR-006 Amendment 5, proven against the real thing. The service tests can say
// "the request carried a Sort and MatchPublished"; whether the SQL then orders
// by published_payload, and whether its keyset window mirrors that ORDER BY
// term for term, are properties of strings built in this package. A fake would
// be a second implementation of those strings, and a keyset bug is invisible in
// a fake that shares the bug.
//
// Every row shares one created_at on purpose. created_at is not in the sorted
// ORDER BY at all, so a page order that came out right cannot have come out
// right by accident of insertion time.
//
// The working copy is IDENTICAL on every row — same title, same amount — so if
// any part of this read `payload` instead of `published_payload` the whole
// collection would collapse into one tie broken by id, with no null block at
// all. That is a different order from every expectation below, on every case.
func TestDeliverySort(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("deliverysort"),
		postgres.WithUsername("super"),
		postgres.WithPassword("super"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Skipf("postgres container: %v", err)
	}
	defer func() { _ = container.Terminate(ctx) }()

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, loadContentRLSMigrations(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	typeID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'t','post','')`, typeID); err != nil {
		t.Fatal(err)
	}
	repo := NewPostgresContentRepository(pool, nil)
	title := domain.Field{ID: uuid.New(), ContentTypeID: typeID, Key: "title", Type: domain.FieldTypeString, EnumValues: []string{}, CreatedAt: time.Now().UTC()}
	amount := domain.Field{ID: uuid.New(), ContentTypeID: typeID, Key: "amount", Type: domain.FieldTypeNumber, EnumValues: []string{}, CreatedAt: time.Now().UTC()}
	for _, f := range []*domain.Field{&title, &amount} {
		if err := repo.AddField(ctx, "t", f); err != nil {
			t.Fatal(err)
		}
	}

	// The seed set, chosen for the two cases a keyset over a payload key gets
	// wrong and gets wrong silently:
	//
	//   - TIES on the sort value ("a" three times; amount 10 twice, 2 twice), so
	//     the value alone cannot say where a page ended. Without `id` in BOTH the
	//     ORDER BY and the window, a tie straddling a page boundary skips or
	//     repeats rows.
	//   - MISSING values (`amount` absent on two rows), which are SQL NULL.
	//     NULL is not a value `<` or `>` can step past — the naive predicate is
	//     NULL, which is not true — so a window that ignores it stops dead at the
	//     first such row and reports the collection as finished.
	type seed struct {
		title  string
		amount string // "" = the key is absent from the snapshot
	}
	seeds := []seed{
		{"b", "10"},
		{"a", "2"},
		{"c", "10"},
		{"a", ""},
		{"d", "2"},
		{"a", ""},
		{"e", "9"},
	}
	shared := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	ids := make([]uuid.UUID, len(seeds))
	for i, s := range seeds {
		ids[i] = uuid.New()
		snapshot := fmt.Sprintf(`{"title":%q}`, s.title)
		if s.amount != "" {
			snapshot = fmt.Sprintf(`{"title":%q,"amount":%s}`, s.title, s.amount)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, status, locale, translation_group_id,
			                     published_payload, published_version, published_at, created_at, updated_at)
			VALUES ($1,'t',$2,'{"title":"zzz","amount":0}'::jsonb,'published','default',$3,$4::jsonb,1,$5,$5,$5)`,
			ids[i], typeID, uuid.New(), snapshot, shared); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// The expected order, computed from the seed table rather than read back
	// from the database: nulls last in BOTH directions, then the value in the
	// sort's direction, then id as the tiebreaker in the same direction.
	want := func(key string, desc bool) []uuid.UUID {
		idx := make([]int, len(seeds))
		for i := range idx {
			idx[i] = i
		}
		value := func(i int) (string, bool) {
			if key == "title" {
				return seeds[i].title, true
			}
			return seeds[i].amount, seeds[i].amount != ""
		}
		less := func(a, b string) bool {
			if key == "amount" {
				// ::numeric, not text — '10' < '9' as text and the cast is the
				// whole reason orderedExpr exists.
				var af, bf float64
				_, _ = fmt.Sscanf(a, "%g", &af)
				_, _ = fmt.Sscanf(b, "%g", &bf)
				return af < bf
			}
			return a < b
		}
		sort.SliceStable(idx, func(x, y int) bool {
			av, aok := value(idx[x])
			bv, bok := value(idx[y])
			if aok != bok {
				return aok
			}
			if aok {
				switch {
				case less(av, bv):
					return !desc
				case less(bv, av):
					return desc
				}
			}
			if desc {
				return ids[idx[x]].String() > ids[idx[y]].String()
			}
			return ids[idx[x]].String() < ids[idx[y]].String()
		})
		out := make([]uuid.UUID, len(idx))
		for i, j := range idx {
			out[i] = ids[j]
		}
		return out
	}

	for _, tc := range []struct {
		field domain.Field
		key   string
		desc  bool
	}{
		{title, "title", false},
		{title, "title", true},
		{amount, "amount", false},
		{amount, "amount", true},
	} {
		dir := "asc"
		if tc.desc {
			dir = "desc"
		}
		t.Run(tc.key+":"+dir, func(t *testing.T) {
			base := ListEntriesFilter{
				TenantID:       "t",
				ContentTypeID:  typeID,
				Status:         domain.StatusPublished,
				MatchPublished: true,
				CursorPaged:    true,
				Sort:           &SortSpec{Field: tc.field, Desc: tc.desc},
			}
			expected := want(tc.key, tc.desc)

			// One unpaged request first: this isolates the ORDER BY from the
			// keyset, so a failure below says which of the two is wrong.
			whole := base
			whole.Limit = 50
			rows, total, err := repo.ListEntries(ctx, whole)
			if err != nil {
				t.Fatalf("unpaged: %v", err)
			}
			if total != 0 {
				t.Fatalf("cursor mode must not run COUNT(*); got total=%d", total)
			}
			if len(rows) != len(expected) {
				t.Fatalf("unpaged returned %d rows, want %d", len(rows), len(expected))
			}
			for i, r := range rows {
				if r.ID != expected[i] {
					t.Fatalf("unpaged position %d: got %s want %s — the ORDER BY is wrong (or it read the working copy)", i, r.ID, expected[i])
				}
			}

			// Then the same order, two rows at a time, through the cursor.
			var seen []uuid.UUID
			var after *EntryCursor
			for page := 0; ; page++ {
				if page > len(seeds)+2 {
					t.Fatal("did not terminate — the keyset predicate is not advancing")
				}
				f := base
				f.Limit = 2
				f.After = after
				got, _, err := repo.ListEntries(ctx, f)
				if err != nil {
					t.Fatalf("page %d: %v", page, err)
				}
				if len(got) == 0 {
					break
				}
				for _, r := range got {
					seen = append(seen, r.ID)
				}
				last := got[len(got)-1]
				after = &EntryCursor{ID: last.ID, SortValue: snapshotValue(t, last, tc.key)}
			}

			if len(seen) != len(expected) {
				t.Fatalf("paged over %d rows, want %d — the window skipped or repeated", len(seen), len(expected))
			}
			uniq := map[uuid.UUID]bool{}
			for i, id := range seen {
				if uniq[id] {
					t.Fatalf("row %s came back on two pages", id)
				}
				uniq[id] = true
				if id != expected[i] {
					t.Fatalf("paged position %d: got %s want %s", i, id, expected[i])
				}
			}
		})
	}
}

// snapshotValue reads one key off the published snapshot the way the service's
// encodeCursor does — nil where the key is absent, which is the position inside
// the trailing null block rather than the absence of a position.
//
// Deliberately hand-rolled here rather than imported: the service package
// depends on this one, so calling its encoder would be a cycle, and a test that
// reused the code under test as its own oracle would agree with any bug.
func snapshotValue(t *testing.T, e *domain.Entry, key string) *string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(e.PublishedPayload, &doc); err != nil {
		t.Fatalf("snapshot payload: %v", err)
	}
	v, ok := doc[key]
	if !ok || v == nil {
		return nil
	}
	var s string
	switch tv := v.(type) {
	case string:
		s = tv
	case float64:
		s = fmt.Sprintf("%g", tv)
	default:
		t.Fatalf("unexpected %T for %q", v, key)
	}
	return &s
}
