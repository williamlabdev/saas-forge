package repository

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// The half of DATA-level permission that a Go fake structurally cannot see.
//
// memRepo mirrors the confinement predicate, and that mirror is worth having —
// but it is Go, so it cannot fail a CHECK, cannot lose a list on the way through
// pgx, and above all cannot get the COUNT wrong in the way that matters here:
// the page and the total come from two different SQL statements that share one
// WHERE, and a predicate added to the first and forgotten in the second is a leak
// the fake reproduces perfectly (it has one loop).

func typeWithRoles(t *testing.T, read, write, ownOnly []string) *domain.ContentType {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC()
	return &domain.ContentType{
		ID: id, TenantID: "t1", Name: "article", Label: "Article",
		ReadRoles: read, WriteRoles: write, OwnOnlyRoles: ownOnly,
		CreatedAt: now, UpdatedAt: now,
		Fields: []domain.Field{{
			ID: uuid.New(), ContentTypeID: id, Key: "title", Type: domain.FieldTypeString,
			EnumValues: []string{}, CreatedAt: now,
		}},
	}
}

// The three lists survive the write and come back. The failure this guards is
// the silent one, and it is worse than the field-level version: an unread
// read_roles scans as empty, empty means unrestricted, and the whole collection
// is served to everyone it was just closed to.
func TestTypePermission_RoundTripsThroughPostgres(t *testing.T) {
	pool, ctx := permPool(t, "typeperm")
	repo := NewPostgresContentRepository(pool, nil)

	ct := typeWithRoles(t, []string{"admin", "owner"}, []string{"owner"}, []string{"editor"})
	if err := repo.CreateContentType(ctx, ct); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetContentTypeByName(ctx, "t1", "article")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got.ReadRoles) != 2 || got.ReadRoles[0] != "admin" || got.ReadRoles[1] != "owner" {
		t.Fatalf("read_roles came back as %v", got.ReadRoles)
	}
	if len(got.WriteRoles) != 1 || got.WriteRoles[0] != "owner" {
		t.Fatalf("write_roles came back as %v", got.WriteRoles)
	}
	if len(got.OwnOnlyRoles) != 1 || got.OwnOnlyRoles[0] != "editor" {
		t.Fatalf("own_only_roles came back as %v", got.OwnOnlyRoles)
	}

	// ListContentTypes reads through a SECOND column list. When the two drifted
	// on the field table the symptom was that one path enforced the permission
	// and the other did not — so both are asserted, from one write.
	all, err := repo.ListContentTypes(ctx, "t1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected one type, got %d", len(all))
	}
	if len(all[0].ReadRoles) != 2 || len(all[0].OwnOnlyRoles) != 1 {
		t.Fatalf("the list path lost a permission list: read=%v own_only=%v",
			all[0].ReadRoles, all[0].OwnOnlyRoles)
	}
}

// A nil list must reach the NOT NULL column as an empty array, not as NULL.
// pgx encodes a nil slice as SQL NULL, and nil is the canonical empty
// everywhere above the repository — so without orEmptyRoles, creating any
// ordinary type is a constraint violation.
func TestTypePermission_NilListsInsertAsEmptyArrays(t *testing.T) {
	pool, ctx := permPool(t, "typepermnil")
	repo := NewPostgresContentRepository(pool, nil)

	if err := repo.CreateContentType(ctx, typeWithRoles(t, nil, nil, nil)); err != nil {
		t.Fatalf("an undeclared type must insert cleanly: %v", err)
	}
	got, err := repo.GetContentTypeByName(ctx, "t1", "article")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got.ReadRoles) != 0 || len(got.WriteRoles) != 0 || len(got.OwnOnlyRoles) != 0 {
		t.Fatalf("an undeclared type came back restricted: %v %v %v",
			got.ReadRoles, got.WriteRoles, got.OwnOnlyRoles)
	}
}

// The CHECK is the backstop for a role that reaches the table around the
// service — a row no policy decision can be made from.
func TestTypePermission_CheckRefusesAnUnknownRole(t *testing.T) {
	pool, ctx := permPool(t, "typepermcheck")
	repo := NewPostgresContentRepository(pool, nil)

	err := repo.CreateContentType(ctx, typeWithRoles(t, []string{"editors"}, nil, nil))
	if err == nil {
		t.Fatal("the database accepted a role that does not exist")
	}
}

// UpdateContentTypeDefinition writes all three lists UNCONDITIONALLY. The bug
// this pins is a partial UPDATE that touched a list only when it "looked set":
// clearing a restriction would then be a no-op, so a revoke-by-omission would
// leave the collection exactly as open as it was while reporting success.
func TestTypePermission_UpdateWritesEveryListIncludingTheEmptyOnes(t *testing.T) {
	pool, ctx := permPool(t, "typepermupd")
	repo := NewPostgresContentRepository(pool, nil)

	ct := typeWithRoles(t, []string{"owner"}, []string{"owner"}, []string{"editor"})
	if err := repo.CreateContentType(ctx, ct); err != nil {
		t.Fatalf("create: %v", err)
	}

	next := *ct
	next.Label = "Articles"
	next.ReadRoles, next.WriteRoles, next.OwnOnlyRoles = nil, nil, nil
	if err := repo.UpdateContentTypeDefinition(ctx, "t1", &next, time.Now().UTC()); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := repo.GetContentTypeByName(ctx, "t1", "article")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got.ReadRoles) != 0 || len(got.WriteRoles) != 0 || len(got.OwnOnlyRoles) != 0 {
		t.Fatalf("a clearing update left restrictions behind: %v %v %v",
			got.ReadRoles, got.WriteRoles, got.OwnOnlyRoles)
	}
	if got.Label != "Articles" {
		t.Fatalf("label not written: %q", got.Label)
	}
}

// Confinement in SQL: the page AND the total. The two come from separate
// statements sharing one WHERE, so this is the assertion the Go fake cannot
// make — there, both come from the same slice.
func TestConfinement_PredicateAppliesToThePageAndTheCount(t *testing.T) {
	pool, ctx := permPool(t, "confinecount")
	repo := NewPostgresContentRepository(pool, nil)

	ct := typeWithRoles(t, nil, nil, []string{"editor"})
	if err := repo.CreateContentType(ctx, ct); err != nil {
		t.Fatalf("create: %v", err)
	}

	alice, bob := uuid.New(), uuid.New()
	now := time.Now().UTC()
	seed := func(author *uuid.UUID, title string) {
		t.Helper()
		e := &domain.Entry{
			ID: uuid.New(), TenantID: "t1", ContentTypeID: ct.ID,
			Payload: []byte(`{"title":"` + title + `"}`), Version: 1,
			Status: domain.StatusDraft, Locale: domain.DefaultLocale,
			TranslationGroupID: uuid.New(), CreatedBy: author,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := repo.CreateEntry(ctx, e); err != nil {
			t.Fatalf("seed %s: %v", title, err)
		}
	}
	seed(&alice, "a1")
	seed(&alice, "a2")
	seed(&bob, "b1")
	// An UNATTRIBUTED row — what every entry predating migration 000021 looks
	// like. It must match nobody, which is the fail-closed direction and the
	// reason enabling confinement is guarded against these.
	seed(nil, "legacy")

	items, total, err := repo.ListEntries(ctx, ListEntriesFilter{
		TenantID: "t1", ContentTypeID: ct.ID, CreatedBy: &alice, Limit: 100,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 of Alice's rows, got %d", len(items))
	}
	if total != 2 {
		t.Fatalf("total is %d, not 2 — the COUNT does not share the confinement predicate, "+
			"so the hidden row count is recoverable by subtraction", total)
	}

	// Unconfined: all four, including the unattributed one.
	items, total, err = repo.ListEntries(ctx, ListEntriesFilter{
		TenantID: "t1", ContentTypeID: ct.ID, Limit: 100,
	})
	if err != nil {
		t.Fatalf("list unconfined: %v", err)
	}
	if len(items) != 4 || total != 4 {
		t.Fatalf("unconfined list is confined: items=%d total=%d", len(items), total)
	}
}

// The count that guards enabling confinement. It must see the unattributed rows
// and only those.
func TestConfinement_CountEntriesWithoutAuthor(t *testing.T) {
	pool, ctx := permPool(t, "confineauthor")
	repo := NewPostgresContentRepository(pool, nil)

	ct := typeWithRoles(t, nil, nil, nil)
	if err := repo.CreateContentType(ctx, ct); err != nil {
		t.Fatalf("create: %v", err)
	}
	author := uuid.New()
	now := time.Now().UTC()
	for i, by := range []*uuid.UUID{&author, nil, nil} {
		e := &domain.Entry{
			ID: uuid.New(), TenantID: "t1", ContentTypeID: ct.ID,
			Payload: []byte(`{"title":"t"}`), Version: 1,
			Status: domain.StatusDraft, Locale: domain.DefaultLocale,
			TranslationGroupID: uuid.New(), CreatedBy: by,
			CreatedAt: now.Add(time.Duration(i) * time.Second), UpdatedAt: now,
		}
		if err := repo.CreateEntry(ctx, e); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	n, err := repo.CountEntriesWithoutAuthor(ctx, "t1", ct.ID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 unattributed rows, got %d", n)
	}
}
