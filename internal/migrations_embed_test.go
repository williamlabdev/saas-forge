package internal

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// diskMigrations walks the whole of internal/ looking for directories named
// `migrations`, rather than reusing the embed's two glob shapes.
//
// Reusing the globs would make this test agree with the embed by construction
// and catch nothing. The failure worth catching is a module nested at a depth
// the globs do not reach — the globs go one and two levels deep because that is
// what exists today, and nothing stops the tenth module from sitting deeper.
func diskMigrations(t *testing.T) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Base(filepath.Dir(p)) != "migrations" {
			return nil
		}
		if strings.HasSuffix(p, ".sql") {
			found[filepath.ToSlash(p)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking internal/: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("no migrations found on disk — this test is not testing anything")
	}
	return found
}

func embeddedMigrations(t *testing.T) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	err := fs.WalkDir(MigrationsFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			found[p] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the embedded FS: %v", err)
	}
	return found
}

func TestEmbeddedTreeMatchesDisk(t *testing.T) {
	disk, embedded := diskMigrations(t), embeddedMigrations(t)

	for p := range disk {
		if !embedded[p] {
			t.Errorf("%s is on disk but not embedded — the go:embed patterns in "+
				"migrations_embed.go do not reach it, so every applier would skip it silently", p)
		}
	}
	for p := range embedded {
		if !disk[p] {
			t.Errorf("%s is embedded but not on disk", p)
		}
	}
}

func TestEveryMigrationHasADownFile(t *testing.T) {
	// Nothing applies down files today, so their absence has no symptom until
	// the day a rollback is needed and there is nothing to roll back to.
	// 000005_auth_audit sat without one long enough to become a backlog ticket.
	// ADR-012 makes down files a precondition of ever introducing a rollback
	// verb; this keeps the set complete in the meantime.
	for p := range embeddedMigrations(t) {
		if !strings.HasSuffix(p, ".up.sql") {
			continue
		}
		down := strings.TrimSuffix(p, ".up.sql") + ".down.sql"
		if _, err := os.Stat(down); err != nil {
			t.Errorf("%s has no matching %s — every migration must be reversible",
				p, filepath.Base(down))
		}
	}
}
