// Package migrate owns the answer to "what has this database been migrated to".
//
// See ADR-012. The short version: applying migrations used to be expressed as
// something that can only happen once — compose's initdb mounts run on an empty
// data directory and never again — so a database that fell behind had no way to
// say so. The symptom was a 500 from whichever handler first touched the new
// column, days later, on someone else's machine.
//
// Everything here follows from wanting that failure to be loud and early:
//
//   - one ledger table, written in the SAME transaction as the DDL it records,
//     so "applied but not recorded" and "recorded but not applied" are states
//     that cannot be expressed rather than states to be careful about;
//   - servers CHECK but never APPLY, so whether to migrate stays a decision
//     someone makes rather than a side effect of a process starting;
//   - a non-empty database with no ledger is refused, not guessed at.
package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
)

// filename matches `000012_tenants_memberships.up.sql`, capturing 000012 and
// tenants_memberships.
var filename = regexp.MustCompile(`^(\d{6})_(.+)\.up\.sql$`)

// Migration is one up-migration on disk. Down files are deliberately absent
// from this type: nothing applies them today, and modelling a rollback the
// applier cannot perform would be a claim we do not honour (ADR-012 Trigger
// conditions covers when that changes).
type Migration struct {
	Version  int    // the six-digit prefix; globally unique across modules
	Name     string // the slug after the prefix
	Path     string // path within the source FS, for error messages
	SQL      string
	Checksum string // sha256 of SQL, hex — what the ledger stores
}

// Discover returns every up-migration in fsys ordered by version.
//
// Ordering is by the number, never by path: modules live in separate
// directories (internal/user, internal/cms/content) that interleave, so sorting
// paths would run migrations in an order nobody chose.
func Discover(fsys fs.FS) ([]Migration, error) {
	var out []Migration
	seen := map[int]string{}

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		m := filename.FindStringSubmatch(path.Base(p))
		if m == nil {
			return nil // down files and anything else are not our business here
		}
		version, err := strconv.Atoi(m[1])
		if err != nil {
			return fmt.Errorf("%s: unreadable version: %w", p, err)
		}
		// Two migrations sharing a number is unrecoverable rather than
		// merely wrong: the ledger's primary key is the version, so the
		// second one would look already-applied forever.
		if prev, dup := seen[version]; dup {
			return fmt.Errorf("migration %06d is claimed by both %s and %s", version, prev, p)
		}
		seen[version] = p

		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{
			Version:  version,
			Name:     m[2],
			Path:     p,
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no migrations found — did the layout move out from under the embed patterns?")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}
