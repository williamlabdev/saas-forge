package repository

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// The acceptance bar for the artifact format, run against Postgres: export a
// schema, rebuild it somewhere else, export again, and get the SAME BYTES.
//
// Bytes rather than structs. A struct comparison would pass while the file
// differed in key order, indentation or escaping — and the file is the thing
// that goes into git, gets reviewed, and gets diffed. It is also why this lives
// against the real database: field order comes out of a SQL query, and a Go
// fake would be asserting its own insertion order.
func TestArtifactRoundTripIsByteIdentical(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("artifact"),
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
	repo := NewPostgresContentRepository(pool, nil)

	// Two types written in an order that is NOT their sorted order, with a
	// field order that is not alphabetical and an enum that is not sorted —
	// otherwise the canonical rules would hold by accident.
	now := time.Now().UTC()
	seedType := func(tenant, name, label string, fields ...domain.Field) {
		id := uuid.New()
		for i := range fields {
			fields[i].ID, fields[i].ContentTypeID, fields[i].CreatedAt = uuid.New(), id, now
			if fields[i].EnumValues == nil {
				fields[i].EnumValues = []string{}
			}
		}
		ct := &domain.ContentType{
			ID: id, TenantID: tenant, Name: name, Label: label,
			Fields: fields, CreatedAt: now, UpdatedAt: now,
		}
		if err := repo.CreateContentType(ctx, ct); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	seedType("src", "zebra", "Zebra & Co",
		domain.Field{Key: "name", Type: domain.FieldTypeString, Required: true})
	seedType("src", "article", "Article",
		domain.Field{Key: "title", Type: domain.FieldTypeString, Required: true},
		domain.Field{Key: "body", Type: domain.FieldTypeText},
		domain.Field{Key: "state", Type: domain.FieldTypeEnum, EnumValues: []string{"draft", "review", "live"}},
		domain.Field{Key: "tags", Type: domain.FieldTypeString, Multiple: true},
		domain.Field{Key: "author", Type: domain.FieldTypeRelation, RelationEntity: "zebra"},
	)

	export := func(tenant string) []byte {
		t.Helper()
		cts, err := repo.ListContentTypes(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		b, err := domain.MarshalArtifact(domain.NewArtifact(cts))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	first := export("src")

	// Rebuild into a DIFFERENT tenant from the artifact alone — which is the
	// real claim. Same-tenant re-export would prove only that the query is
	// stable.
	var art domain.Artifact
	if err := json.Unmarshal(first, &art); err != nil {
		t.Fatal(err)
	}
	for _, at := range art.Types {
		fields := make([]domain.Field, 0, len(at.Fields))
		for _, af := range at.Fields {
			f := domain.Field{
				Key: af.Key, Type: af.Type, Label: af.Label,
				Required: af.Required, Multiple: af.Multiple,
				RelationEntity: af.RelationEntity, EnumValues: af.EnumValues,
			}
			if f.EnumValues == nil {
				f.EnumValues = []string{}
			}
			fields = append(fields, f)
		}
		seedType("dst", at.Name, at.Label, fields...)
	}

	second := export("dst")
	if string(first) != string(second) {
		t.Fatalf("round trip changed the bytes:\n--- exported\n%s\n--- re-exported\n%s", first, second)
	}

	t.Run("an ampersand survives unescaped", func(t *testing.T) {
		// Go's default encoder would render this &, which is valid JSON
		// and not what anyone typed. A schema file people hand-edit must come
		// back the way it went in.
		if !strings.Contains(string(first), "Zebra & Co") {
			t.Fatalf("label was escaped:\n%s", first)
		}
	})

	t.Run("the artifact is sorted by type even though rows are not", func(t *testing.T) {
		if art.Types[0].Name != "article" || art.Types[1].Name != "zebra" {
			t.Fatalf("types not canonically ordered: %s, %s", art.Types[0].Name, art.Types[1].Name)
		}
		// And the field order is the DEFINITION order, not alphabetical: it is
		// what the admin form renders.
		got := art.Types[0].Fields
		for i, want := range []string{"title", "body", "state", "tags", "author"} {
			if got[i].Key != want {
				t.Fatalf("field %d is %q, want %q — the query is not preserving definition order", i, got[i].Key, want)
			}
		}
	})
}
