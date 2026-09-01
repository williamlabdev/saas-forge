// Command migrate is the only thing in this repository that executes DDL.
//
// Servers check and refuse; this applies. Keeping those in separate binaries is
// the point of ADR-012 §4 — "should this database change shape right now" stays
// a decision someone makes, rather than a side effect of a process starting,
// and N replicas booting at once cannot race over the same DDL.
//
//	migrate status           what the ledger says vs. what is on disk
//	migrate up               apply everything not yet applied
//	migrate adopt --to=NNN   claim an existing database up to NNN, running no DDL
//
// DATABASE_URL is read from the environment, the same variable the servers use.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/williamlabdev/saas-forge/internal"
	"github.com/williamlabdev/saas-forge/internal/platform/migrate"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	adopt := flag.NewFlagSet("adopt", flag.ExitOnError)
	adoptTo := adopt.Int("to", 0, "highest migration number this database already has")

	flag.Usage = usage
	flag.Parse()
	if flag.NArg() == 0 {
		usage()
		return errors.New("no command given")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ms, err := migrate.Discover(internal.MigrationsFS)
	if err != nil {
		return err
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	switch cmd := flag.Arg(0); cmd {
	case "status":
		return status(ctx, pool, ms)
	case "up":
		return up(ctx, pool, ms)
	case "adopt":
		if err := adopt.Parse(flag.Args()[1:]); err != nil {
			return err
		}
		if *adoptTo == 0 {
			return errors.New("adopt needs --to=NNN: the highest migration this database already has. " +
				"Nobody can infer it, which is why the flag is required")
		}
		if err := migrate.Adopt(ctx, pool, ms, *adoptTo); err != nil {
			return err
		}
		fmt.Printf("adopted up to %06d — no DDL was executed\n", *adoptTo)
		return status(ctx, pool, ms)
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func status(ctx context.Context, pool *pgxpool.Pool, ms []migrate.Migration) error {
	st, err := migrate.Inspect(ctx, pool, ms)
	if err != nil {
		return err
	}
	fmt.Printf("migrations on disk: %d (up to %06d)\n", len(ms), ms[len(ms)-1].Version)
	if !st.HasLedger {
		if st.Empty {
			fmt.Println("ledger:             absent, and the database is empty — run `migrate up`")
		} else {
			fmt.Printf("ledger:             absent, but the database has tables — "+
				"run `migrate adopt --to=NNN` once you know what it holds "+
				"(highest on disk is %06d)\n", ms[len(ms)-1].Version)
		}
		return nil
	}
	fmt.Printf("applied:            %d\n", len(st.Applied))
	report("not applied", st.Missing)
	report("checksum changed since applied", st.Mismatched)
	// Not a refusal — see migrate.Verify. Worth printing, because it is also
	// what a binary older than its database looks like.
	report("in the ledger but not on disk", st.Unknown)
	if len(st.Missing) == 0 && len(st.Mismatched) == 0 {
		fmt.Println("up to date")
	}
	return nil
}

func report(label string, vs []int) {
	if len(vs) == 0 {
		return
	}
	fmt.Printf("%s: ", label)
	for i, v := range vs {
		if i > 0 {
			fmt.Print(", ")
		}
		fmt.Printf("%06d", v)
	}
	fmt.Println()
}

func up(ctx context.Context, pool *pgxpool.Pool, ms []migrate.Migration) error {
	applied, err := migrate.Up(ctx, pool, ms)
	// Report what landed even on failure: each migration commits on its own, so
	// a run that dies in the middle leaves real work behind, and saying nothing
	// about it is how someone ends up re-running by hand.
	for _, v := range applied {
		fmt.Printf("applied %06d\n", v)
	}
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		fmt.Println("nothing to apply — already up to date")
	}
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: migrate <command>

  status           compare the ledger against the migrations on disk
  up               apply every migration not yet in the ledger
  adopt --to=NNN   record migrations up to NNN as applied WITHOUT running them,
                   for a database that predates the ledger

DATABASE_URL must be set.
`)
}
