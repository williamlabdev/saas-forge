package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Non-test code allowed to build a pool directly. Everything else must go
// through OpenVerifiedPool, which is where the migration check lives.
//
//   - platform/db.go IS OpenVerifiedPool.
//   - cmd/migrate is the applier. It must reach a database that fails
//     verification — refusing to connect until the migrations are applied would
//     leave no way to apply them.
var mayOpenPoolDirectly = map[string]bool{
	"internal/platform/db.go": true,
	"cmd/migrate/main.go":     true,
}

// A bypass is silent in the worst way: the server comes up, serves from a
// schema nobody checked, and looks entirely healthy. Two composition roots
// already exist (BuildApp and Wire's graph in cmd/server); this is the tooth
// that makes a third one either use the choke point or turn this test red.
func TestNothingBypassesTheVerifiedPool(t *testing.T) {
	root := "../.."
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "node_modules", ".git", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if mayOpenPoolDirectly[rel] {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), "pgxpool.New(") {
			t.Errorf("%s calls pgxpool.New directly — use platform.OpenVerifiedPool so the "+
				"migration check cannot be skipped, or add it to mayOpenPoolDirectly with a reason", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}
}

// The allowlist is only meaningful if its entries exist; a renamed file would
// otherwise leave a permanent hole that reads as deliberate.
func TestPoolAllowlistEntriesExist(t *testing.T) {
	for rel := range mayOpenPoolDirectly {
		if _, err := os.Stat(filepath.Join("../..", rel)); err != nil {
			t.Errorf("mayOpenPoolDirectly names %s, which does not exist: %v", rel, err)
		}
	}
}
