package repository

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// The half of field permission that a Go fake structurally cannot see: what the
// COLUMNS do. memRepo is written in Go, so it cannot fail a CHECK constraint,
// cannot lose a value on the way through pgx, and cannot reorder rows — which
// are the three things that go wrong here.

// A permission list survives the write and comes back. The failure this guards
// is the silent one: an unread read_roles scans as empty, empty means
// unrestricted, and the field is served to everyone it was just closed to.
func TestFieldPermission_RoundTripsThroughPostgres(t *testing.T) {
	pool, ctx := permPool(t, "fieldperm")
	repo := NewPostgresContentRepository(pool, nil)

	id := uuid.New()
	now := time.Now().UTC()
	ct := &domain.ContentType{
		ID: id, TenantID: "t1", Name: "employee", Label: "Employee",
		CreatedAt: now, UpdatedAt: now,
		Fields: []domain.Field{
			{
				ID: uuid.New(), ContentTypeID: id, Key: "salary", Type: domain.FieldTypeNumber,
				EnumValues: []string{},
				ReadRoles:  []string{"admin", "owner"}, WriteRoles: []string{"owner"},
				CreatedAt: now,
			},
			// A field with NO declaration, alongside one that has it — so a
			// pass cannot come from the columns being ignored wholesale.
			{
				ID: uuid.New(), ContentTypeID: id, Key: "name", Type: domain.FieldTypeString,
				EnumValues: []string{}, CreatedAt: now,
			},
		},
	}
	if err := repo.CreateContentType(ctx, ct); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.GetContentTypeByName(ctx, "t1", "employee")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	salary, ok := got.FieldByKey("salary")
	if !ok {
		t.Fatal("salary field missing")
	}
	if len(salary.ReadRoles) != 2 || salary.ReadRoles[0] != "admin" || salary.ReadRoles[1] != "owner" {
		t.Fatalf("read_roles came back as %v", salary.ReadRoles)
	}
	if len(salary.WriteRoles) != 1 || salary.WriteRoles[0] != "owner" {
		t.Fatalf("write_roles came back as %v", salary.WriteRoles)
	}
	name, _ := got.FieldByKey("name")
	if len(name.ReadRoles) != 0 || len(name.WriteRoles) != 0 {
		t.Fatalf("an undeclared field came back restricted: read=%v write=%v", name.ReadRoles, name.WriteRoles)
	}
}

// A nil list must reach the NOT NULL column as an empty array, not as NULL.
// pgx encodes a nil slice as SQL NULL — the same trap enum_values documents —
// and nil is the canonical empty everywhere above the repository, so the
// conversion has to happen at this boundary or every unrestricted field fails
// to insert.
func TestFieldPermission_NilListInsertsAsEmptyNotNull(t *testing.T) {
	pool, ctx := permPool(t, "fieldpermnil")
	repo := NewPostgresContentRepository(pool, nil)

	id := uuid.New()
	now := time.Now().UTC()
	if err := repo.CreateContentType(ctx, &domain.ContentType{
		ID: id, TenantID: "t1", Name: "post", CreatedAt: now, UpdatedAt: now,
		Fields: []domain.Field{{
			ID: uuid.New(), ContentTypeID: id, Key: "title", Type: domain.FieldTypeString,
			EnumValues: []string{}, ReadRoles: nil, WriteRoles: nil, CreatedAt: now,
		}},
	}); err != nil {
		t.Fatalf("a nil permission list must insert: %v", err)
	}
}

// The CHECK is the layer that survives someone writing to this table around the
// service. A role the application cannot recognise is a row no policy decision
// can be made from — so it must not be storable at all.
func TestFieldPermission_DatabaseRefusesAnUnknownRole(t *testing.T) {
	pool, ctx := permPool(t, "fieldpermcheck")

	typeID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO content_types (id, tenant_id, name, label) VALUES ($1,'t1','post','')`, typeID); err != nil {
		t.Fatalf("seed type: %v", err)
	}

	_, err := pool.Exec(ctx, `
		INSERT INTO content_type_fields (id, content_type_id, key, field_type, label, required, enum_values, read_roles)
		VALUES ($1, $2, 'title', 'string', '', false, '{}', ARRAY['superuser'])`,
		uuid.New(), typeID)
	if err == nil {
		t.Fatal("the database accepted a role the application does not know — the CHECK is not doing its job")
	}

	// And the legal set must actually be storable, or the constraint is merely
	// refusing everything.
	for _, role := range domain.AllowedFieldRoles() {
		if _, err := pool.Exec(ctx, `
			INSERT INTO content_type_fields (id, content_type_id, key, field_type, label, required, enum_values, read_roles)
			VALUES ($1, $2, $3, 'string', '', false, '{}', ARRAY[$4::TEXT])`,
			uuid.New(), typeID, "f_"+role, role); err != nil {
			t.Fatalf("the CHECK refuses a legal role %q: %v", role, err)
		}
	}
}
