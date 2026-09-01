package repository

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// schema_proposals against the real database (ADR-013 §3 step 8, 000037).
//
// Everything asserted below lives in SQL and nowhere else, which is why the
// service-level suite cannot stand in for it: the decision predicate is a WHERE
// clause, the audit-trail invariants are CHECK constraints, the isolation is an
// RLS policy. memRepo mirrors all three by hand — so it agrees with them
// whether or not they exist.
func TestSchemaProposals(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("proposals"),
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
	// Truncated to what timestamptz stores, at the SOURCE — see
	// revision_integration_test.go for why the bug this prevents is
	// structurally invisible on a Darwin machine.
	now := time.Now().UTC().Truncate(time.Microsecond)

	file := func(t *testing.T, tenant string, expires time.Time) *SchemaProposal {
		t.Helper()
		p := &SchemaProposal{
			TenantID:       tenant,
			Artifact:       []byte(`{"artifact_version":"v1","kind":"cms.schema/v1","types":[]}`),
			Prune:          false,
			Plan:           []byte(`{"steps":[],"applicable":0,"refused":0,"blocked":0}`),
			ProposedBy:     uuid.New(),
			ProposedByKind: "human",
			ExpiresAt:      expires,
		}
		if err := repo.CreateSchemaProposal(ctx, p); err != nil {
			t.Fatalf("create proposal: %v", err)
		}
		return p
	}

	t.Run("a proposal round-trips and starts pending", func(t *testing.T) {
		p := file(t, "t", now.Add(time.Hour))
		if p.ID == uuid.Nil {
			t.Fatal("the insert did not return an id")
		}
		got, err := repo.GetSchemaProposal(ctx, "t", p.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != ProposalPending {
			t.Fatalf("status=%q — a filed proposal is pending", got.Status)
		}
		if got.DecidedAt != nil || got.DecidedBy != nil {
			t.Fatalf("a pending proposal carries a decision: %+v", got)
		}
		// jsonb, so what comes back is the document and not the bytes. Parsing
		// it is the service's job; here it only has to be non-empty, because a
		// column typed text would still round-trip and a NULL would not.
		if len(got.Plan) == 0 || len(got.Artifact) == 0 {
			t.Fatalf("documents came back empty: %+v", got)
		}
	})

	t.Run("deciding is a one-way door, enforced by the WHERE clause", func(t *testing.T) {
		p := file(t, "t", now.Add(time.Hour))
		decider := uuid.New()
		if err := repo.DecideSchemaProposal(ctx, "t", p.ID, ProposalApproved, decider, now); err != nil {
			t.Fatalf("first decision: %v", err)
		}
		// The SECOND one is the claim. Two approvers pressing at once both read
		// a pending row; without `status = 'pending'` in the UPDATE both would
		// write, and the loser would be applying a schema change against a
		// proposal already spent.
		err := repo.DecideSchemaProposal(ctx, "t", p.ID, ProposalRejected, uuid.New(), now)
		if !errors.Is(err, ErrProposalNotPending) {
			t.Fatalf("second decision: %v — want ErrProposalNotPending", err)
		}
		got, err := repo.GetSchemaProposal(ctx, "t", p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != ProposalApproved {
			t.Fatalf("status=%q — the loser overwrote the winner", got.Status)
		}
		if got.DecidedBy == nil || *got.DecidedBy != decider {
			t.Fatalf("decided_by=%v — the audit trail names the wrong person", got.DecidedBy)
		}
	})

	t.Run("an expired proposal cannot be decided at all", func(t *testing.T) {
		p := file(t, "t", now.Add(-time.Minute))
		err := repo.DecideSchemaProposal(ctx, "t", p.ID, ProposalApproved, uuid.New(), now)
		if !errors.Is(err, ErrProposalNotPending) {
			t.Fatalf("deciding an expired proposal: %v — the deadline is in the UPDATE, not only in the service", err)
		}
		got, err := repo.GetSchemaProposal(ctx, "t", p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != ProposalPending {
			t.Fatalf("status=%q — an expired row must keep its stored status; 'expired' is derived", got.Status)
		}
		if !got.Expired(now) {
			t.Fatal("Expired() disagrees with the deadline the UPDATE enforced")
		}
	})

	t.Run("the audit trail cannot be made to lie", func(t *testing.T) {
		// A decision without a decider, and a decider without a decision. Both
		// are CHECK constraints; both are states the Go code has no path to and
		// a migration or a fixture could still write.
		if _, err := pool.Exec(ctx, `
			INSERT INTO schema_proposals (tenant_id, artifact, prune, plan, status,
				proposed_by, proposed_by_kind, expires_at, decided_at)
			VALUES ('t','{}'::jsonb,false,'{}'::jsonb,'approved',$1,'human',now(),now())`,
			uuid.New()); err == nil {
			t.Fatal("a decision with no decider was accepted")
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO schema_proposals (tenant_id, artifact, prune, plan, status,
				proposed_by, proposed_by_kind, expires_at)
			VALUES ('t','{}'::jsonb,false,'{}'::jsonb,'approved',$1,'human',now())`,
			uuid.New()); err == nil {
			t.Fatal("an approved proposal with no decision timestamp was accepted")
		}
		// 'expired' is deliberately not a storable status (000037): storing it
		// would need a sweeper for the column to ever become true.
		if _, err := pool.Exec(ctx, `
			INSERT INTO schema_proposals (tenant_id, artifact, prune, plan, status,
				proposed_by, proposed_by_kind, expires_at)
			VALUES ('t','{}'::jsonb,false,'{}'::jsonb,'expired',$1,'human',now())`,
			uuid.New()); err == nil {
			t.Fatal("'expired' was accepted as a stored status")
		}
		// An agent name without the agent kind, and vice versa.
		if _, err := pool.Exec(ctx, `
			INSERT INTO schema_proposals (tenant_id, artifact, prune, plan, status,
				proposed_by, proposed_by_kind, proposed_by_agent, expires_at)
			VALUES ('t','{}'::jsonb,false,'{}'::jsonb,'pending',$1,'human','writer-bot',now())`,
			uuid.New()); err == nil {
			t.Fatal("a human proposal carrying an agent name was accepted")
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO schema_proposals (tenant_id, artifact, prune, plan, status,
				proposed_by, proposed_by_kind, expires_at)
			VALUES ('t','{}'::jsonb,false,'{}'::jsonb,'pending',$1,'agent',now())`,
			uuid.New()); err == nil {
			t.Fatal("an agent proposal with no agent name was accepted")
		}
	})

	t.Run("another tenant's proposal is not reachable", func(t *testing.T) {
		mine := file(t, "t", now.Add(time.Hour))
		theirs := file(t, "other", now.Add(time.Hour))

		if _, err := repo.GetSchemaProposal(ctx, "t", theirs.ID); err == nil {
			t.Fatal("read across tenants")
		}
		// Deciding across tenants must fail as NOT PENDING/NOT FOUND rather than
		// silently affecting zero rows and returning success.
		if err := repo.DecideSchemaProposal(ctx, "t", theirs.ID, ProposalApproved, uuid.New(), now); err == nil {
			t.Fatal("decided another tenant's proposal")
		}

		list, err := repo.ListSchemaProposals(ctx, "t", 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range list {
			if p.ID == theirs.ID {
				t.Fatal("another tenant's proposal appeared in the queue")
			}
		}
		var sawMine bool
		for _, p := range list {
			if p.ID == mine.ID {
				sawMine = true
			}
		}
		// The control: without it, a list that returned nothing at all would
		// pass the isolation assertion above.
		if !sawMine {
			t.Fatal("the tenant's own proposal is missing from its queue")
		}
	})

	t.Run("the queue is capped, and the cap hides rather than deletes", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			file(t, "capped", now.Add(time.Hour))
		}
		list, err := repo.ListSchemaProposals(ctx, "capped", 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 2 {
			t.Fatalf("len=%d — the LIMIT is not applied", len(list))
		}
		var total int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_proposals WHERE tenant_id='capped'`).Scan(&total); err != nil {
			t.Fatal(err)
		}
		// Bypassing the method under test on purpose: this is the only assertion
		// that can tell "hidden by the LIMIT" from "purged", and a purge is the
		// change most likely to look like it is obeying the same rule.
		if total != 3 {
			t.Fatalf("rows=%d — the cap must hide, never delete", total)
		}
	})

	// The proposer's own read (000038). Everything below is a property of the
	// WHERE clause and of a nullable column, so memRepo agrees with it either
	// way — these are the assertions the service suite structurally cannot make.

	fileAs := func(t *testing.T, tenant string, by uuid.UUID, kind string, agent *string, proposerPlan []byte) *SchemaProposal {
		t.Helper()
		p := &SchemaProposal{
			TenantID:        tenant,
			Artifact:        []byte(`{"artifact_version":"v1","kind":"cms.schema/v1","types":[]}`),
			Plan:            []byte(`{"steps":[],"applicable":0,"refused":0,"blocked":0}`),
			PlanProposer:    proposerPlan,
			ProposedBy:      by,
			ProposedByKind:  kind,
			ProposedByAgent: agent,
			ExpiresAt:       now.Add(time.Hour),
		}
		if err := repo.CreateSchemaProposal(ctx, p); err != nil {
			t.Fatalf("create proposal: %v", err)
		}
		return p
	}

	t.Run("own read matches the CREDENTIAL, not the principal behind it", func(t *testing.T) {
		// One person, two agents. proposed_by is identical on both rows, which is
		// the whole difficulty: the principal cannot tell them apart.
		principal := uuid.New()
		postBot, invoiceBot := "post-bot", "invoice-bot"
		mine := fileAs(t, "own", principal, "agent", &postBot, []byte(`{"steps":[]}`))
		theirs := fileAs(t, "own", principal, "agent", &invoiceBot, []byte(`{"steps":[]}`))

		got, err := repo.GetOwnSchemaProposal(ctx, "own", mine.ID, principal, "agent", &postBot)
		if err != nil {
			t.Fatalf("the filer cannot read its own row: %v", err)
		}
		if got.ID != mine.ID {
			t.Fatalf("id=%s want %s", got.ID, mine.ID)
		}
		// The claim. Same tenant, same principal, same verb — only the agent name
		// differs, and that must be enough to refuse.
		if _, err := repo.GetOwnSchemaProposal(ctx, "own", theirs.ID, principal, "agent", &postBot); !errors.Is(err, apperrors.ErrNotFound) {
			t.Fatalf("a sibling agent's row was reachable: err=%v", err)
		}
	})

	t.Run("a human finds its own row through the NULL agent name", func(t *testing.T) {
		// `proposed_by_agent = $5` would return nothing here, because NULL = NULL
		// is NULL. IS NOT DISTINCT FROM is the difference between a person being
		// able to read the proposal they filed and not.
		person := uuid.New()
		p := fileAs(t, "own", person, "human", nil, []byte(`{"steps":[]}`))
		got, err := repo.GetOwnSchemaProposal(ctx, "own", p.ID, person, "human", nil)
		if err != nil {
			t.Fatalf("a human cannot read the proposal it filed: %v", err)
		}
		if got.ID != p.ID {
			t.Fatalf("id=%s want %s", got.ID, p.ID)
		}
		if _, err := repo.GetOwnSchemaProposal(ctx, "own", p.ID, uuid.New(), "human", nil); !errors.Is(err, apperrors.ErrNotFound) {
			t.Fatalf("another person reached it: err=%v", err)
		}
	})

	t.Run("plan_proposer round-trips, and stays NULL when nothing was written", func(t *testing.T) {
		person := uuid.New()
		withPlan := fileAs(t, "own", person, "human", nil, []byte(`{"steps":[],"applicable":7,"refused":0,"blocked":0}`))
		got, err := repo.GetOwnSchemaProposal(ctx, "own", withPlan.ID, person, "human", nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.PlanProposer) == 0 {
			t.Fatal("plan_proposer came back empty — the column is not being read")
		}
		// Position, not name: Scan assigns by ordinal, so a column appended to
		// the SELECT and not to the destinations misassigns every field after it
		// instead of failing. Asserting a field from the middle of the row proves
		// the two lists still line up.
		if got.ProposedByKind != "human" {
			t.Fatalf("kind=%q — the column list and the scan destinations have drifted", got.ProposedByKind)
		}

		// A row written the way 000037 wrote them: no proposer view at all. NULL
		// must survive as nil rather than arriving as an empty document, because
		// the service renders the two differently and only one of them is true.
		none := fileAs(t, "own", person, "human", nil, nil)
		got, err = repo.GetOwnSchemaProposal(ctx, "own", none.ID, person, "human", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got.PlanProposer != nil {
			t.Fatalf("plan_proposer=%q — an unwritten column must read back as nil", got.PlanProposer)
		}
	})

	t.Run("the own LIST matches on the same credential and carries plan_proposer", func(t *testing.T) {
		// Same shape as the single read's test, because the two WHERE clauses are
		// the same clause: an id the list shows must not 404 on the read, and a
		// row the read refuses must not appear in the list.
		principal := uuid.New()
		postBot, invoiceBot := "post-bot", "invoice-bot"
		first := fileAs(t, "ownlist", principal, "agent", &postBot, []byte(`{"steps":[],"applicable":1}`))
		second := fileAs(t, "ownlist", principal, "agent", &postBot, []byte(`{"steps":[],"applicable":2}`))
		sibling := fileAs(t, "ownlist", principal, "agent", &invoiceBot, []byte(`{"steps":[]}`))
		person := fileAs(t, "ownlist", principal, "human", nil, []byte(`{"steps":[]}`))

		list, err := repo.ListOwnSchemaProposals(ctx, "ownlist", principal, "agent", &postBot, 50)
		if err != nil {
			t.Fatalf("list own: %v", err)
		}
		seen := map[uuid.UUID]bool{}
		for _, p := range list {
			seen[p.ID] = true
			if len(p.PlanProposer) == 0 {
				t.Fatalf("row %s came back without plan_proposer — the only plan this surface may render", p.ID)
			}
		}
		if len(list) != 2 || !seen[first.ID] || !seen[second.ID] {
			t.Fatalf("list=%d rows, seen=%v — want exactly the two rows post-bot filed", len(list), seen)
		}
		if seen[sibling.ID] {
			t.Fatal("a sibling agent of the same principal appeared in the list")
		}
		if seen[person.ID] {
			t.Fatal("the person's own row appeared in an agent's list — IS NOT DISTINCT FROM matched NULL to a name")
		}
		// Newest first, the queue's ordering. Asserted as a property of the
		// sequence rather than as one expected id, so it holds when two rows
		// share a created_at and the id tiebreak decides.
		for i := 1; i < len(list); i++ {
			if list[i].CreatedAt.After(list[i-1].CreatedAt) {
				t.Fatalf("row %d is newer than row %d — the list is not newest-first", i, i-1)
			}
		}

		// The human's own list is theirs, which is what stops the refusals above
		// from also holding for a query that returns nothing.
		mine, err := repo.ListOwnSchemaProposals(ctx, "ownlist", principal, "human", nil, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(mine) != 1 || mine[0].ID != person.ID {
			t.Fatalf("the person's list did not return the row they filed: %d rows", len(mine))
		}

		// The cap is the queue's LIMIT precedent: hidden, never deleted.
		capped, err := repo.ListOwnSchemaProposals(ctx, "ownlist", principal, "agent", &postBot, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(capped) != 1 {
			t.Fatalf("limit was ignored: %d rows", len(capped))
		}
	})
}
