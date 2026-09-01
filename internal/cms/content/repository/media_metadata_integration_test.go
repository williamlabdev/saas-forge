package repository

import (
	"context"
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

// Client-declared media metadata against the real database.
//
// Everything asserted here is something the service-level fake structurally
// cannot answer. It cannot enforce a CHECK constraint, so a Go validator that
// silently disagreed with the SQL would pass every test in the service package
// and fail in production. And it cannot tell you whether the columns were
// actually written: CreateMediaAsset, GetMediaAsset and
// UpdateMediaAssetMetadata each build their own statement, and a column present
// in one and missing from another returns a zero value that reads as data rather
// than as an error.
//
// The constraints come FIRST, before any happy path, because they are the guard
// that survives a future writer who never reads this file.
func TestMediaAssetMetadata(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("mediameta"),
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

	// insert goes around the repository on purpose: these cases are about what the
	// DATABASE refuses, and routing them through Go would only re-test the Go
	// validator that already has its own tests.
	insert := func(t *testing.T, filename, altText any, w, h any) error {
		t.Helper()
		_, err := pool.Exec(ctx, `
			INSERT INTO media_assets (id, tenant_id, storage_key, content_type, size_bytes, filename, alt_text, width_px, height_px)
			VALUES ($1,'t',$2,'image/png',1, $3, $4, $5, $6)`,
			uuid.New(), "t/"+uuid.NewString(), filename, altText, w, h)
		return err
	}

	t.Run("constraints", func(t *testing.T) {
		// The control: all four NULL is the shape of every row that predates
		// migration 000022, and must remain legal forever. Without this the
		// rejections below could all be passing for the wrong reason.
		if err := insert(t, nil, nil, nil, nil); err != nil {
			t.Fatalf("all-NULL metadata must stay legal (every pre-000022 row is this shape): %v", err)
		}
		if err := insert(t, "duck.png", "a duck", 800, 600); err != nil {
			t.Fatalf("a fully populated row must be legal: %v", err)
		}
		// '' alt text is the "decorative" answer and is NOT the same as NULL.
		if err := insert(t, "duck.png", "", nil, nil); err != nil {
			t.Fatalf("empty alt_text must be legal — it is how an editor says decorative: %v", err)
		}

		reject := []struct {
			name     string
			filename any
			altText  any
			w, h     any
		}{
			{"a lone width cannot reserve layout space", nil, nil, 800, nil},
			{"a lone height, symmetrically", nil, nil, nil, 600},
			{"zero is not a picture", nil, nil, 0, 600},
			{"a dimension past the ceiling", nil, nil, 800, domain.MaxImageDimension + 1},
			{"a negative dimension", nil, nil, -1, -1},
			{"alt_text one character over the limit", nil, strings.Repeat("a", domain.MaxAltTextLen+1), nil, nil},
			{"a zero-length filename is not a filename", "", nil, nil, nil},
			{"a filename one character over the limit", strings.Repeat("a", domain.MaxFilenameLen+1), nil, nil, nil},
			{"a forward slash makes the name read as a path", "a/b.png", nil, nil, nil},
			{"a backslash is the same hazard", `a\b.png`, nil, nil, nil},
			{"a newline is header injection once this reaches Content-Disposition", "a\nb.png", nil, nil, nil},
			{"a tab is a control character too, not just CR/LF", "a\tb.png", nil, nil, nil},
		}
		for _, tc := range reject {
			t.Run(tc.name, func(t *testing.T) {
				if err := insert(t, tc.filename, tc.altText, tc.w, tc.h); err == nil {
					t.Fatal("the CHECK constraint did not fire")
				}
			})
		}

		// The boundaries themselves are legal — a constraint that is one off in the
		// tightening direction would pass every rejection above.
		if err := insert(t, strings.Repeat("a", domain.MaxFilenameLen),
			strings.Repeat("a", domain.MaxAltTextLen), 1, domain.MaxImageDimension); err != nil {
			t.Fatalf("the limits are inclusive: %v", err)
		}
	})

	// Declared metadata is written at RESERVATION, so it has to survive the insert
	// path and come back out of the read path. Both statements are involved, which
	// is the point.
	assetID := uuid.New()
	t.Run("reservation stores declared metadata and the read path returns it", func(t *testing.T) {
		name, alt := "duck.png", "a duck on a pond"
		w, h := 1920, 1080
		if err := repo.CreateMediaAsset(ctx, &domain.MediaAsset{
			ID: assetID, TenantID: "t", StorageKey: "t/" + assetID.String(),
			ContentType: "image/png", CreatedAt: time.Now().UTC(),
			Filename: &name, AltText: &alt, WidthPx: &w, HeightPx: &h,
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
		got := mustGetAsset(t, ctx, repo, assetID)
		requireStr(t, "filename", got.Filename, "duck.png")
		requireStr(t, "alt_text", got.AltText, "a duck on a pond")
		requireInt(t, "width_px", got.WidthPx, 1920)
		requireInt(t, "height_px", got.HeightPx, 1080)
	})

	t.Run("a patch touches only the fields it names", func(t *testing.T) {
		alt := "a mallard"
		got, err := repo.UpdateMediaAssetMetadata(ctx, "t", assetID, MediaAssetPatch{
			SetAltText: true, AltText: &alt,
		})
		if err != nil {
			t.Fatalf("patch: %v", err)
		}
		requireStr(t, "alt_text", got.AltText, "a mallard")
		// The whole reason MediaAssetPatch carries Set flags: a caller correcting
		// alt text must not blank a filename it never sent. A SET list that assigned
		// all four columns would null these three and this is where it shows.
		requireStr(t, "filename", got.Filename, "duck.png")
		requireInt(t, "width_px", got.WidthPx, 1920)
		requireInt(t, "height_px", got.HeightPx, 1080)

		// RETURNING and a re-read must agree, or the DTO is reporting an intention
		// rather than a stored fact.
		reread := mustGetAsset(t, ctx, repo, assetID)
		requireStr(t, "alt_text after re-read", reread.AltText, "a mallard")
		requireStr(t, "filename after re-read", reread.Filename, "duck.png")
	})

	t.Run("empty alt text is stored as a value, not as NULL", func(t *testing.T) {
		empty := ""
		got, err := repo.UpdateMediaAssetMetadata(ctx, "t", assetID, MediaAssetPatch{
			SetAltText: true, AltText: &empty,
		})
		if err != nil {
			t.Fatalf("patch: %v", err)
		}
		if got.AltText == nil {
			t.Fatal(`"" collapsed to NULL: "an editor said this is decorative" became "nobody has described it"`)
		}
		if *got.AltText != "" {
			t.Fatalf("alt_text = %q, want empty", *got.AltText)
		}
		// And the DB agrees it is not NULL — the distinction the whole tri-state
		// design rests on, asserted where it actually lives.
		var isNull bool
		if err := pool.QueryRow(ctx,
			`SELECT alt_text IS NULL FROM media_assets WHERE id = $1`, assetID).Scan(&isNull); err != nil {
			t.Fatal(err)
		}
		if isNull {
			t.Fatal("alt_text is SQL NULL, but the editor sent an empty string")
		}
	})

	t.Run("an explicit null clears the column", func(t *testing.T) {
		got, err := repo.UpdateMediaAssetMetadata(ctx, "t", assetID, MediaAssetPatch{
			SetAltText: true, AltText: nil,
		})
		if err != nil {
			t.Fatalf("patch: %v", err)
		}
		if got.AltText != nil {
			t.Fatalf("alt_text = %q, want nil — an explicit null must reset to not-recorded", *got.AltText)
		}
		// Guard against the reset being indistinguishable from "did nothing": the
		// previous subtest proved the column held a value first.
		requireStr(t, "filename", got.Filename, "duck.png")
	})

	t.Run("dimensions clear as a pair", func(t *testing.T) {
		got, err := repo.UpdateMediaAssetMetadata(ctx, "t", assetID, MediaAssetPatch{
			SetDimensions: true, WidthPx: nil, HeightPx: nil,
		})
		if err != nil {
			t.Fatalf("patch: %v", err)
		}
		if got.WidthPx != nil || got.HeightPx != nil {
			t.Fatal("both dimensions must clear together")
		}
	})

	// The Go struct makes "set width, leave height" unrepresentable, so this is
	// what happens if a future edit removes that protection: the DB still refuses.
	// Belt and braces stated as such — the constraint is the one that has to hold.
	t.Run("the database still refuses a half-written pair", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`UPDATE media_assets SET width_px = 800, height_px = NULL WHERE id = $1`, assetID); err == nil {
			t.Fatal("media_assets_dimensions_check did not fire on a direct UPDATE")
		}
	})

	t.Run("an empty patch is a no-op read, not an error", func(t *testing.T) {
		got, err := repo.UpdateMediaAssetMetadata(ctx, "t", assetID, MediaAssetPatch{})
		if err != nil {
			t.Fatalf("an empty patch must not error: %v", err)
		}
		requireStr(t, "filename", got.Filename, "duck.png")
	})

	t.Run("a patch cannot reach another tenant's asset", func(t *testing.T) {
		name := "stolen.png"
		_, err := repo.UpdateMediaAssetMetadata(ctx, "other-tenant", assetID, MediaAssetPatch{
			SetFilename: true, Filename: &name,
		})
		if err == nil {
			t.Fatal("a patch scoped to another tenant must not find the row")
		}
		requireStr(t, "filename", mustGetAsset(t, ctx, repo, assetID).Filename, "duck.png")
	})
}

func mustGetAsset(t *testing.T, ctx context.Context, repo *PostgresContentRepository, id uuid.UUID) *domain.MediaAsset {
	t.Helper()
	a, err := repo.GetMediaAsset(ctx, "t", id)
	if err != nil {
		t.Fatalf("get media asset: %v", err)
	}
	return a
}

func requireStr(t *testing.T, what string, got *string, want string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s is nil, want %q", what, want)
	}
	if *got != want {
		t.Fatalf("%s = %q, want %q", what, *got, want)
	}
}

func requireInt(t *testing.T, what string, got *int, want int) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s is nil, want %d", what, want)
	}
	if *got != want {
		t.Fatalf("%s = %d, want %d", what, *got, want)
	}
}
