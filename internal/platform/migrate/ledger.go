package migrate

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// LedgerTable is the one table this package owns.
const LedgerTable = "schema_migrations"

// createLedger is idempotent so every verb can call it without ordering rules.
// applied_at has no default: the applier sets it inside the same transaction as
// the DDL, and a default would quietly paper over a caller that forgot.
const createLedger = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INT PRIMARY KEY,
    name       TEXT NOT NULL,
    checksum   TEXT NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL
)`

// Querier is the read/write surface shared by *pgxpool.Pool and pgx.Tx, so the
// same helpers work inside and outside a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Beginner is the subset of *pgxpool.Pool the applier needs.
type Beginner interface {
	Querier
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Applied is one row of the ledger.
type Applied struct {
	Version   int
	Name      string
	Checksum  string
	AppliedAt time.Time
}

// State is what the database says about itself, compared against the
// migrations on disk. It is pure description — deciding whether a state is
// permissible is Verify's job, so `migrate status` can print a state that
// Verify would refuse.
type State struct {
	HasLedger bool
	// Empty means no base tables at all besides the ledger. It is the
	// difference between "brand new database" and "database somebody has
	// been using" — the only two readings of a missing ledger, and they
	// need opposite responses.
	Empty      bool
	Applied    map[int]Applied
	Missing    []int // on disk, absent from the ledger
	Mismatched []int // in the ledger, but the file's checksum has changed
	Unknown    []int // in the ledger, absent from disk
}

// Inspect reads the ledger and diffs it against ms. It writes nothing — in
// particular it does not create the ledger table, because a server calling this
// at boot must not leave traces in a database it is about to refuse.
func Inspect(ctx context.Context, db Querier, ms []Migration) (State, error) {
	st := State{Applied: map[int]Applied{}}

	var hasLedger bool
	if err := db.QueryRow(ctx,
		`SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&hasLedger); err != nil {
		return State{}, fmt.Errorf("probing for the ledger: %w", err)
	}
	st.HasLedger = hasLedger

	// Count user tables other than the ledger itself. current_schema() rather
	// than a literal 'public' so a search_path-scoped deployment reports on the
	// schema it actually uses.
	var others int
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = current_schema()
		   AND table_type = 'BASE TABLE'
		   AND table_name <> $1`, LedgerTable).Scan(&others); err != nil {
		return State{}, fmt.Errorf("counting existing tables: %w", err)
	}
	st.Empty = others == 0

	if hasLedger {
		rows, err := db.Query(ctx,
			`SELECT version, name, checksum, applied_at FROM schema_migrations`)
		if err != nil {
			return State{}, fmt.Errorf("reading the ledger: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var a Applied
			if err := rows.Scan(&a.Version, &a.Name, &a.Checksum, &a.AppliedAt); err != nil {
				return State{}, fmt.Errorf("reading the ledger: %w", err)
			}
			st.Applied[a.Version] = a
		}
		if err := rows.Err(); err != nil {
			return State{}, fmt.Errorf("reading the ledger: %w", err)
		}
	}

	onDisk := map[int]bool{}
	for _, m := range ms {
		onDisk[m.Version] = true
		a, ok := st.Applied[m.Version]
		switch {
		case !ok:
			st.Missing = append(st.Missing, m.Version)
		case a.Checksum != m.Checksum:
			st.Mismatched = append(st.Mismatched, m.Version)
		}
	}
	for v := range st.Applied {
		if !onDisk[v] {
			st.Unknown = append(st.Unknown, v)
		}
	}
	sort.Ints(st.Missing)
	sort.Ints(st.Mismatched)
	sort.Ints(st.Unknown)
	return st, nil
}

// The refusal reasons are distinct types rather than one error with a string,
// so tests can assert that removing one check reddens that check's test alone
// (ADR-012 §驗證 6) instead of matching on message text that three of them
// could satisfy.

// ErrUnadopted is a non-empty database no ledger row accounts for: the one
// state that cannot be inferred from evidence. Assuming it is current
// reproduces the very bug this package exists to kill; re-running every
// migration explodes against the objects already there. So a human who knows
// the database declares it.
//
// An absent ledger and a ledger with no rows are the same fact here, and the
// wording has to hold for both — an earlier `up` that created the table before
// refusing leaves the second shape.
type ErrUnadopted struct{ Highest int }

func (e ErrUnadopted) Error() string {
	return fmt.Sprintf(
		"this database has tables that no %s row accounts for, so what it has been migrated to is unknown. "+
			"If it is up to date, claim it with `migrate adopt --to=%d`; if it is not, "+
			"adopt the number it actually reached and then run `migrate up`",
		LedgerTable, e.Highest)
}

// ErrUninitialised is an empty database. Distinct from ErrUnadopted because the
// answer differs: this one just needs `migrate up`, and adopting it would write
// a ledger claiming DDL that never ran.
type ErrUninitialised struct{ Pending int }

func (e ErrUninitialised) Error() string {
	return fmt.Sprintf(
		"this database is empty and has no %s ledger — run `migrate up` to apply all %d migrations",
		LedgerTable, e.Pending)
}

// ErrBehind names the numbers, because "the database is behind" without them
// sends someone to diff two directory listings by hand.
type ErrBehind struct{ Missing []int }

func (e ErrBehind) Error() string {
	return fmt.Sprintf(
		"this database is behind: migration(s) %s exist on disk but were never applied — run `migrate up`",
		formatVersions(e.Missing))
}

// ErrChecksumMismatch means an already-applied file was edited. The database
// cannot be reasoned about from the files any more, in either direction.
type ErrChecksumMismatch struct{ Versions []int }

func (e ErrChecksumMismatch) Error() string {
	return fmt.Sprintf(
		"migration(s) %s were applied but their files have changed since — "+
			"the database no longer matches the migrations that describe it",
		formatVersions(e.Versions))
}

func formatVersions(vs []int) string {
	parts := make([]string, 0, len(vs))
	for _, v := range vs {
		parts = append(parts, fmt.Sprintf("%06d", v))
	}
	return strings.Join(parts, ", ")
}

// Verify is the boot gate. It reports the first reason the database must not be
// served from, and nil when it may be.
//
// It never applies anything. Two reasons, the second being the real one: racing
// replicas would fight over the same DDL, and more fundamentally whether to
// migrate now is a decision a person makes, not a side effect of a process
// starting.
//
// A ledger entry with no file on disk (State.Unknown) is deliberately NOT a
// refusal. That is a binary older than its database — the normal middle of a
// rolling deploy — and refusing it would turn every deploy into an outage.
func Verify(ctx context.Context, db Querier, ms []Migration) error {
	st, err := Inspect(ctx, db, ms)
	if err != nil {
		return err
	}
	switch {
	case !st.HasLedger && st.Empty:
		return ErrUninitialised{Pending: len(ms)}
	case !st.HasLedger:
		return ErrUnadopted{Highest: ms[len(ms)-1].Version}
	case len(st.Missing) > 0:
		return ErrBehind{Missing: st.Missing}
	case len(st.Mismatched) > 0:
		return ErrChecksumMismatch{Versions: st.Mismatched}
	}
	return nil
}

// Up applies every migration the ledger does not have, oldest first, and
// returns the versions it applied.
//
// Each migration gets its own transaction containing BOTH the DDL and its
// ledger row. Postgres puts DDL under transaction control, so this is not a
// convention that has to be maintained: a migration that fails halfway leaves
// neither the objects nor the row.
func Up(ctx context.Context, db Beginner, ms []Migration) ([]int, error) {
	// Inspect BEFORE creating the ledger. Creating it first destroys the state
	// this has to recognise — no ledger, but tables already present — and that
	// state is not recoverable afterwards, because an empty ledger looks the
	// same whether the database is new or merely unaccounted for.
	st, err := Inspect(ctx, db, ms)
	if err != nil {
		return nil, err
	}
	// Tables that no ledger row accounts for mean this database predates the
	// ledger. Applying 000001 to it is wrong twice over: it explodes when the
	// objects collide, and it SUCCEEDS when they do not — leaving a ledger that
	// claims a history which never ran. Verify refuses this at boot (ADR-012
	// §4); the applier is the path `docker compose up` takes, so it must refuse
	// it too, and only adopt can resolve it.
	if !st.Empty && len(st.Applied) == 0 {
		return nil, ErrUnadopted{Highest: ms[len(ms)-1].Version}
	}
	if _, err := db.Exec(ctx, createLedger); err != nil {
		return nil, fmt.Errorf("creating the ledger: %w", err)
	}
	// Refuse to build on a mismatch rather than stacking new migrations on a
	// database whose history already disagrees with the files.
	if len(st.Mismatched) > 0 {
		return nil, ErrChecksumMismatch{Versions: st.Mismatched}
	}

	var applied []int
	for _, m := range ms {
		if _, done := st.Applied[m.Version]; done {
			continue
		}
		if err := applyOne(ctx, db, m); err != nil {
			return applied, err
		}
		applied = append(applied, m.Version)
	}
	return applied, nil
}

func applyOne(ctx context.Context, db Beginner, m Migration) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%06d %s: begin: %w", m.Version, m.Name, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Migration files hold several statements. pgx's default extended protocol
	// refuses more than one per Exec, so ask for the simple protocol; the SQL
	// is embedded from the repository and takes no parameters, which is the
	// condition under which that is safe.
	if _, err := tx.Exec(ctx, m.SQL, pgx.QueryExecModeSimpleProtocol); err != nil {
		return fmt.Errorf("%06d %s (%s): %w", m.Version, m.Name, m.Path, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name, checksum, applied_at)
		 VALUES ($1, $2, $3, now())`,
		m.Version, m.Name, m.Checksum); err != nil {
		return fmt.Errorf("%06d %s: recording in the ledger: %w", m.Version, m.Name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%06d %s: commit: %w", m.Version, m.Name, err)
	}
	return nil
}

// ErrNothingToAdopt guards the reflex of running adopt on a fresh database,
// which would record DDL that never ran and leave every table missing with the
// ledger insisting otherwise.
var ErrNothingToAdopt = errors.New(
	"this database is empty, so there is nothing to adopt — run `migrate up` instead")

// Adopt writes ledger rows for every migration up to and including `to`,
// executing none of them. It is the only way to bring an existing database —
// one that predates the ledger — under management.
//
// The caller asserts a fact the database cannot supply. Getting it wrong fails
// toward "refuses to start" (adopted too low, so `up` re-runs DDL that explodes
// on existing objects) rather than toward "serves on the wrong schema", which
// is the failure mode being eliminated.
func Adopt(ctx context.Context, db Beginner, ms []Migration, to int) error {
	if _, err := db.Exec(ctx, createLedger); err != nil {
		return fmt.Errorf("creating the ledger: %w", err)
	}
	st, err := Inspect(ctx, db, ms)
	if err != nil {
		return err
	}
	if st.Empty && len(st.Applied) == 0 {
		return ErrNothingToAdopt
	}
	var claim []Migration
	for _, m := range ms {
		if m.Version <= to {
			claim = append(claim, m)
		}
	}
	if len(claim) == 0 {
		return fmt.Errorf("no migration has a version <= %06d; the lowest on disk is %06d",
			to, ms[0].Version)
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, m := range claim {
		// ON CONFLICT DO NOTHING so re-adopting after a partial run is not an
		// error; an already-claimed row is already the state being asked for.
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, name, checksum, applied_at)
			 VALUES ($1, $2, $3, now()) ON CONFLICT (version) DO NOTHING`,
			m.Version, m.Name, m.Checksum); err != nil {
			return fmt.Errorf("adopting %06d: %w", m.Version, err)
		}
	}
	return tx.Commit(ctx)
}
