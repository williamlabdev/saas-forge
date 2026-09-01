package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// Proposal statuses as stored. 'expired' is NOT one of them — see 000037: the
// clock decides expiry, and a stored value would need a sweeper to become true.
const (
	ProposalPending  = "pending"
	ProposalApproved = "approved"
	ProposalRejected = "rejected"
)

// ErrProposalNotPending says the row was already decided — approved, rejected,
// or decided by someone else between the read and the write.
//
// It exists as a distinct error because the losing side of that race is not a
// caller who did anything wrong, and the answer it deserves says which: a
// proposal approved twice must not apply twice, and one rejected after approval
// must not silently reopen.
var ErrProposalNotPending = errors.New("content: schema proposal is no longer pending")

// SchemaProposal is one filed schema change, as stored.
//
// Artifact and Plan are opaque JSON here on purpose. The repository has no
// business parsing either — PlanResult is a SERVICE type, and a repository that
// knew it would be the direction of dependency this package spent ADR-008
// avoiding. What the table stores is two documents and the facts about who
// filed them.
type SchemaProposal struct {
	ID       uuid.UUID
	TenantID string
	Artifact []byte
	Prune    bool
	// Plan is the plan in the APPROVER's scope, not the proposer's (000037 and
	// ADR-013 補裁 Q). Storing the proposer's narrowed view would make an agent's
	// proposal unapprovable in any tenant with a second content type.
	Plan []byte
	// PlanProposer is the same plan in the PROPOSER's scope (000038), stored
	// rather than derived: recomputing it on read would quietly repair a stale
	// proposal, and filtering the approver's steps would leave the counts
	// describing rows the caller cannot see.
	//
	// nil means NOT RECORDED — an agent row filed before 000038, whose narrowed
	// view is a function of the live schema at the time and unreconstructible.
	// It is never an empty plan: "no plan recorded" and "this would change
	// nothing" are different claims.
	PlanProposer []byte
	Status       string
	// Who asked. For an agent, ProposedBy is the PRINCIPAL — the person
	// answerable for it — and ProposedByAgent carries the agent's own name, the
	// same three-column shape as entry provenance (000030).
	ProposedBy      uuid.UUID
	ProposedByKind  string
	ProposedByAgent *string
	CreatedAt       time.Time
	ExpiresAt       time.Time
	DecidedAt       *time.Time
	DecidedBy       *uuid.UUID
}

// Expired reports the derived status 000037 refuses to store: pending, and past
// its deadline.
//
// It takes `now` rather than calling time.Now, because the answer must be the
// same one the SQL gave in the statement that loaded the row. A helper that
// consulted the clock itself could disagree with the WHERE clause that let the
// row through, which is precisely the window an approval must not slip into.
func (p *SchemaProposal) Expired(now time.Time) bool {
	return p.Status == ProposalPending && !p.ExpiresAt.After(now)
}

const proposalColumns = `id, tenant_id, artifact, prune, plan, status,
	proposed_by, proposed_by_kind, proposed_by_agent,
	created_at, expires_at, decided_at, decided_by`

// proposalOwnColumns adds the proposer's stored view, and it is deliberately not
// part of proposalColumns: the queue never renders it, and carrying a second
// plan document on every one of the 50 capped rows would double that response
// for nothing.
const proposalOwnColumns = proposalColumns + `, plan_proposer`

// proposalDests is the single copy of the column-to-field correspondence. Two
// hand-written lists — one in the SELECT, one in the Scan — drift silently and
// in the worst way: Scan matches by POSITION, so a column added to one and not
// the other misassigns every field after it rather than failing.
func proposalDests(p *SchemaProposal) []any {
	return []any{&p.ID, &p.TenantID, &p.Artifact, &p.Prune, &p.Plan, &p.Status,
		&p.ProposedBy, &p.ProposedByKind, &p.ProposedByAgent,
		&p.CreatedAt, &p.ExpiresAt, &p.DecidedAt, &p.DecidedBy}
}

func scanProposal(row pgx.Row) (*SchemaProposal, error) {
	var p SchemaProposal
	if err := row.Scan(proposalDests(&p)...); err != nil {
		return nil, err
	}
	return &p, nil
}

// scanProposalOwn scans proposalOwnColumns — the same row plus plan_proposer,
// appended in the same order the constant appends it.
func scanProposalOwn(row pgx.Row) (*SchemaProposal, error) {
	var p SchemaProposal
	if err := row.Scan(append(proposalDests(&p), &p.PlanProposer)...); err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *PostgresContentRepository) CreateSchemaProposal(ctx context.Context, p *SchemaProposal) error {
	return r.withTenant(ctx, p.TenantID, func(q querier) error {
		err := q.QueryRow(ctx, `
			INSERT INTO schema_proposals
				(tenant_id, artifact, prune, plan, plan_proposer, status, proposed_by, proposed_by_kind, proposed_by_agent, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING id, created_at`,
			p.TenantID, p.Artifact, p.Prune, p.Plan, p.PlanProposer, ProposalPending,
			p.ProposedBy, p.ProposedByKind, p.ProposedByAgent, p.ExpiresAt,
		).Scan(&p.ID, &p.CreatedAt)
		if err != nil {
			return fmt.Errorf("create schema proposal: %w", err)
		}
		p.Status = ProposalPending
		return nil
	})
}

func (r *PostgresContentRepository) GetSchemaProposal(ctx context.Context, tenantID string, id uuid.UUID) (*SchemaProposal, error) {
	var p *SchemaProposal
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		var err error
		p, err = scanProposal(q.QueryRow(ctx,
			`SELECT `+proposalColumns+` FROM schema_proposals WHERE tenant_id = $1 AND id = $2`,
			tenantID, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperrors.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get schema proposal: %w", err)
	}
	return p, nil
}

// GetOwnSchemaProposal loads one proposal only if the caller is the one who
// filed it, and returns ErrNotFound otherwise.
//
// THE AGENT NAME IS PART OF THE MATCH, and leaving it out is the bug this
// signature exists to prevent. `proposed_by` holds the PRINCIPAL, so every agent
// a person mints shares it with that person and with each other (000037's
// three-column provenance, following 000030). Matching on the principal alone
// would let an agent scoped to `post` read a proposal filed by a sibling agent
// scoped to `invoice` — and read it complete with the stored plan, which is the
// tenant's type list under another name. "Own" here means the same CREDENTIAL,
// not the same person.
//
// IS NOT DISTINCT FROM rather than =, because proposed_by_agent is NULL on human
// rows and `NULL = NULL` is NULL, which would match nothing: a person would be
// unable to read the proposal they filed themselves.
func (r *PostgresContentRepository) GetOwnSchemaProposal(ctx context.Context, tenantID string, id uuid.UUID,
	proposedBy uuid.UUID, kind string, agentID *string) (*SchemaProposal, error) {
	var p *SchemaProposal
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		var err error
		p, err = scanProposalOwn(q.QueryRow(ctx,
			`SELECT `+proposalOwnColumns+` FROM schema_proposals
			 WHERE tenant_id = $1 AND id = $2
			   AND proposed_by = $3 AND proposed_by_kind = $4
			   AND proposed_by_agent IS NOT DISTINCT FROM $5`,
			tenantID, id, proposedBy, kind, agentID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Not "forbidden": whether the id exists at all is not something a caller
		// who did not file it gets to learn from the difference.
		return nil, apperrors.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get own schema proposal: %w", err)
	}
	return p, nil
}

// ListOwnSchemaProposals returns the proposals THIS CREDENTIAL filed, newest
// first, capped at limit.
//
// The WHERE clause is GetOwnSchemaProposal's with the id dropped, and it is a
// copy on purpose rather than a looser predicate that "lists what this person
// filed": the two must agree about what "own" means, or an id the list shows
// would 404 on the single read — or worse, the list would show a sibling
// credential's row that the single read is written to refuse. Same three
// provenance columns, same IS NOT DISTINCT FROM for the NULL agent name on a
// human's row.
//
// It selects proposalOwnColumns because plan_proposer is the only plan this
// surface may render (補裁 Q-2). The queue's 50-row cost argument does not apply
// here: the approver's `plan` is the column this response must NOT carry, so
// the second document is not an extra — it is the whole answer.
func (r *PostgresContentRepository) ListOwnSchemaProposals(ctx context.Context, tenantID string,
	proposedBy uuid.UUID, kind string, agentID *string, limit int) ([]*SchemaProposal, error) {
	var out []*SchemaProposal
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		rows, err := q.Query(ctx,
			`SELECT `+proposalOwnColumns+` FROM schema_proposals
			 WHERE tenant_id = $1
			   AND proposed_by = $2 AND proposed_by_kind = $3
			   AND proposed_by_agent IS NOT DISTINCT FROM $4
			 ORDER BY created_at DESC, id DESC LIMIT $5`,
			tenantID, proposedBy, kind, agentID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanProposalOwn(rows)
			if err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list own schema proposals: %w", err)
	}
	return out, nil
}

// ListSchemaProposals returns the tenant's proposals newest first, capped at
// limit.
//
// The cap is a LIMIT rather than a purge, following 000034's precedent: what is
// past the cap is hidden from this list, not deleted — an expired proposal
// remains the record of something that was asked and never answered.
func (r *PostgresContentRepository) ListSchemaProposals(ctx context.Context, tenantID string, limit int) ([]*SchemaProposal, error) {
	var out []*SchemaProposal
	err := r.withTenant(ctx, tenantID, func(q querier) error {
		rows, err := q.Query(ctx,
			`SELECT `+proposalColumns+` FROM schema_proposals
			 WHERE tenant_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`,
			tenantID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanProposal(rows)
			if err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list schema proposals: %w", err)
	}
	return out, nil
}

// DecideSchemaProposal records the answer, and is the ONLY write that moves a
// proposal off pending.
//
// The `status = 'pending' AND expires_at > now` in the WHERE is not a repeat of
// the service's check — it is the one that holds under concurrency. Two
// approvers pressing at once both read a pending row; without this predicate
// both would write a decision, and the second would be applying a schema change
// against a proposal already spent. Zero rows affected means somebody else got
// there first, or the deadline passed while the approver was reading.
func (r *PostgresContentRepository) DecideSchemaProposal(ctx context.Context, tenantID string, id uuid.UUID,
	status string, decidedBy uuid.UUID, now time.Time) error {
	return r.withTenant(ctx, tenantID, func(q querier) error {
		tag, err := q.Exec(ctx, `
			UPDATE schema_proposals
			SET status = $1, decided_at = $2, decided_by = $3
			WHERE tenant_id = $4 AND id = $5 AND status = 'pending' AND expires_at > $2`,
			status, now, decidedBy, tenantID, id)
		if err != nil {
			return fmt.Errorf("decide schema proposal: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrProposalNotPending
		}
		return nil
	})
}
