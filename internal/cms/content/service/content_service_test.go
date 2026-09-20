package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/cms/content/repository"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// --- in-memory repository ---------------------------------------------------

type memRepo struct {
	types   []*domain.ContentType
	entries []*domain.Entry
	// components mirrors content_components (000044) with their sub-field
	// rows; the methods live in component_memrepo_test.go.
	components []*domain.Component
	// lastList is the most recent ListEntries request, verbatim — see the note
	// on memRepo.ListEntries for what a test may and may not read from it.
	lastList repository.ListEntriesFilter
	// byIDsCalls counts GetEntriesByIDs, and lastByIDs is the id set of the most
	// recent one. Together they are how a test pins "one query per PAGE" —
	// populate's whole reason for existing as a batch (ADR-006 Amendment 6). A
	// fake that only returned the right rows would agree with an N+1
	// implementation on every assertion about the response.
	byIDsCalls    int
	lastByIDs     []uuid.UUID
	deliveryReads map[string]int64
	assets        []*domain.MediaAsset
	links         map[uuid.UUID][]uuid.UUID // entryID -> assetIDs (WORKING copy)
	// publishedLinks mirrors entry_media_published: the asset refs of the
	// published snapshot. Kept separate so the delivery gate under test is the
	// real one — a single map would make the draft-removes-image case pass
	// vacuously.
	publishedLinks map[uuid.UUID][]uuid.UUID
	// relations mirrors entry_relations (000050, ADR-024): entryID -> the
	// (fieldPath, targetID) pairs its WORKING payload names. publishedRelations
	// is its published-snapshot twin, kept separate for the same reason
	// publishedLinks is: a single map would make the draft-removes-reference
	// case pass vacuously.
	relations          map[uuid.UUID][]repository.EntryRelationRef
	publishedRelations map[uuid.UUID][]repository.EntryRelationRef
	webhooks           []*domain.Webhook
	// activity is the append-only stream (ADR-014 §3), oldest first.
	activity []*domain.Activity
	// idem mirrors entry_idempotency, keyed the way the table's PRIMARY KEY is:
	// (tenant, actor, key). A map keyed by the key alone would make the
	// per-issuer scoping ruled on 2026-08-06 pass vacuously — the fake would
	// agree with a table that did not have the column.
	idem map[idemKey]repository.EntryIdempotency
	// proposals mirrors schema_proposals (000037), newest last.
	proposals []*repository.SchemaProposal
	// schedules mirrors content_entry_schedules (000041), oldest first. The
	// partial unique index is mirrored in CreateEntrySchedule, and the two
	// transitions the table makes from OTHER writes — stale on update,
	// superseded on a manual publish — are mirrored in UpdateEntry and
	// SetEntryPublishState below, because in Postgres they are statements
	// inside those same transactions and a fake that only moved them from the
	// scheduling path would agree with a database that had no such rule.
	schedules []*domain.EntrySchedule
	// reviewDecisions mirrors entry_review_decisions (000051, ADR-014
	// Amendment: review decisions). The methods live in
	// review_decision_memrepo_test.go.
	reviewDecisions []*domain.EntryReviewDecision
	// variants mirrors media_variants (000043). The FK cascade is mirrored in
	// DeleteMediaAsset; the (asset, preset) primary key in EnqueueMediaVariants.
	// SKIP LOCKED is NOT mirrored — that claim lives in the repository's
	// integration test, where there are real row locks to claim.
	variants []*domain.MediaVariant
	// publishRevisions mirrors entry_publish_revisions (000042), keyed by entry,
	// oldest first. It is written from SetEntryPublishState — not from a method
	// of its own — because in Postgres that is where the INSERT lives, inside
	// the publish's transaction. A fake that let a test append revisions
	// directly would agree with an implementation that recorded them from the
	// service, which is precisely the arrangement ADR-018 rejected: the
	// scheduler worker never passes through the service.
	//
	// The retention purge is mirrored here too, for the same reason: a fake with
	// unbounded history would make "the twenty-first publish drops the first"
	// pass vacuously.
	publishRevisions map[uuid.UUID][]domain.EntryPublishRevision
	// self is what WithTx hands to its callback, when a test has WRAPPED this
	// fake to intervene in one method.
	//
	// Without it WithTx passes `m`, so a decorator is silently bypassed for the
	// whole transaction — every override a test installed stops applying at
	// exactly the point the interesting failures happen. That is a fake-only
	// trap with no counterpart in Postgres, whose WithTx hands back a repository
	// bound to the transaction, and it cost this file one test that passed while
	// testing nothing.
	self repository.ContentRepository
}

type idemKey struct{ tenant, actor, key string }

// setUnpublishedChanges mirrors unpublishedChangesExpr in the Postgres
// repository. The comparison has to be SEMANTIC, not bytes.Equal: jsonb does
// not consider content written two ways a difference, so a byte comparison
// would let the fake disagree with the database on exactly the case this flag
// exists to get right. TestMemRepo_MirrorsJSONBSemantics pins that.
func setUnpublishedChanges(e *domain.Entry) {
	e.HasUnpublishedChanges = e.Status == domain.StatusPublished &&
		!sameJSON(e.Payload, e.PublishedPayload)
}

// sameJSON approximates jsonb equality by normalising both sides through Go's
// decoder. Known limit: integers beyond float64's 53-bit mantissa collapse
// together here while Postgres (numeric) keeps them apart. That cannot make the
// fake disagree with the database about anything the service can produce —
// validateAndNormalize puts every stored payload through the same float64
// round-trip — but do not reach for this outside the fake.
func sameJSON(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	an, err := json.Marshal(av)
	if err != nil {
		return false
	}
	bn, err := json.Marshal(bv)
	if err != nil {
		return false
	}
	return string(an) == string(bn)
}

// WithTx rolls back on error, at the granularity a Go fake can honestly offer:
// it snapshots the COLLECTIONS, so rows appended (or removed) inside a failed
// callback are discarded.
//
// WHAT IT DOES NOT ROLL BACK is fields mutated in place on an existing
// *domain.Entry — the fake hands out pointers where Postgres hands out copies.
// That limit is stated rather than papered over because the create path's
// rollback IS load-bearing: when RecordEntryIdempotency loses the race, the
// entry built moments earlier must not survive. This fake proves the append is
// discarded; the Postgres integration test is what proves the real transaction
// does the same, and it is the only thing that can (see memrepo memory note).
func (m *memRepo) WithTx(_ context.Context, _ string, fn func(repository.ContentRepository) error) error {
	entries := append([]*domain.Entry(nil), m.entries...)
	// Deep-copied, unlike entries: 缺口計畫 5.1's partial approval made this
	// load-bearing the same way proposals became load-bearing at step 7 —
	// AddField (and its siblings below) mutate an existing *domain.ContentType's
	// Fields IN PLACE, so a shallow pointer-slice snapshot would leave a field
	// added by one selected step surviving the rollback triggered by another
	// selected step in the same approval that failed. Cloning each type AND its
	// Fields slice is what makes a rolled-back field addition actually
	// disappear from this fake, the same way it disappears from Postgres.
	types := make([]*domain.ContentType, len(m.types))
	for i, t := range m.types {
		cp := *t
		cp.Fields = append([]domain.Field(nil), t.Fields...)
		types[i] = &cp
	}
	activity := append([]*domain.Activity(nil), m.activity...)
	assets := append([]*domain.MediaAsset(nil), m.assets...)
	idem := make(map[idemKey]repository.EntryIdempotency, len(m.idem))
	for k, v := range m.idem {
		idem[k] = v
	}
	links := cloneLinks(m.links)
	published := cloneLinks(m.publishedLinks)
	// Deep-copied, unlike entries: approving DECIDES a proposal in place, so a
	// slice snapshot alone would leave a rolled-back approval still marked
	// approved — the fake agreeing with an audit trail Postgres would not have
	// written.
	proposals := make([]*repository.SchemaProposal, len(m.proposals))
	for i, p := range m.proposals {
		cp := *p
		proposals[i] = &cp
	}
	// Deep-copied for the same reason as proposals: a schedule is DECIDED in
	// place (pending → done/stale/…), so a slice snapshot would leave a
	// rolled-back execution still marked done.
	schedules := make([]*domain.EntrySchedule, len(m.schedules))
	for i, sc := range m.schedules {
		cp := *sc
		schedules[i] = &cp
	}
	// Deep-copied like schedules: a variant row is finished in place.
	variants := make([]*domain.MediaVariant, len(m.variants))
	for i, v := range m.variants {
		cp := *v
		variants[i] = &cp
	}
	var target repository.ContentRepository = m
	if m.self != nil {
		target = m.self
	}
	if err := fn(target); err != nil {
		m.entries, m.types, m.activity, m.assets = entries, types, activity, assets
		m.idem, m.links, m.publishedLinks = idem, links, published
		m.proposals, m.schedules, m.variants = proposals, schedules, variants
		return err
	}
	return nil
}

func cloneLinks(src map[uuid.UUID][]uuid.UUID) map[uuid.UUID][]uuid.UUID {
	if src == nil {
		return nil
	}
	out := make(map[uuid.UUID][]uuid.UUID, len(src))
	for k, v := range src {
		out[k] = append([]uuid.UUID(nil), v...)
	}
	return out
}

func (m *memRepo) FindEntryIdempotency(_ context.Context, tenantID, actorKey, key string) (*repository.EntryIdempotency, error) {
	rec, ok := m.idem[idemKey{tenantID, actorKey, key}]
	if !ok {
		return nil, nil
	}
	return &rec, nil
}

func (m *memRepo) RecordEntryIdempotency(_ context.Context, rec repository.EntryIdempotency) error {
	k := idemKey{rec.TenantID, rec.ActorKey, rec.Key}
	// Mirror the PRIMARY KEY rather than overwriting, or the lost-race path would
	// never be reachable in a service-level test.
	if _, taken := m.idem[k]; taken {
		return repository.ErrIdempotencyKeyTaken
	}
	if m.idem == nil {
		m.idem = map[idemKey]repository.EntryIdempotency{}
	}
	m.idem[k] = rec
	return nil
}

func (m *memRepo) CreateSchemaProposal(_ context.Context, p *repository.SchemaProposal) error {
	cp := *p
	cp.ID = uuid.New()
	cp.CreatedAt = time.Now()
	cp.Status = repository.ProposalPending
	m.proposals = append(m.proposals, &cp)
	*p = cp
	return nil
}

func (m *memRepo) GetSchemaProposal(_ context.Context, tenantID string, id uuid.UUID) (*repository.SchemaProposal, error) {
	for _, p := range m.proposals {
		if p.TenantID == tenantID && p.ID == id {
			cp := *p
			return &cp, nil
		}
	}
	return nil, apperrors.ErrNotFound
}

// GetOwnSchemaProposal mirrors the SQL, not the intention. The WHERE clause
// matches all three provenance columns, so this does too — including the
// IS NOT DISTINCT FROM on the agent name, which is what makes a human row (NULL
// agent) findable by its human author and an agent row unreachable by a sibling
// agent sharing the same principal. A fake that matched on proposed_by alone
// would agree with every test that only ever uses one credential, and disagree
// with Postgres on exactly the case the column list exists for.
func (m *memRepo) GetOwnSchemaProposal(_ context.Context, tenantID string, id uuid.UUID,
	proposedBy uuid.UUID, kind string, agentID *string) (*repository.SchemaProposal, error) {
	sameAgent := func(a, b *string) bool {
		if a == nil || b == nil {
			return a == b
		}
		return *a == *b
	}
	for _, p := range m.proposals {
		if p.TenantID == tenantID && p.ID == id &&
			p.ProposedBy == proposedBy && p.ProposedByKind == kind &&
			sameAgent(p.ProposedByAgent, agentID) {
			cp := *p
			return &cp, nil
		}
	}
	return nil, apperrors.ErrNotFound
}

// ListOwnSchemaProposals mirrors the SQL the same way, and newest-first is part
// of the mirror rather than incidental: m.proposals is append-ordered, so
// walking it backwards is this fake's ORDER BY created_at DESC. A fake that
// returned insertion order would let an ordering assertion pass against a query
// with no ORDER BY at all.
func (m *memRepo) ListOwnSchemaProposals(_ context.Context, tenantID string,
	proposedBy uuid.UUID, kind string, agentID *string, limit int) ([]*repository.SchemaProposal, error) {
	sameAgent := func(a, b *string) bool {
		if a == nil || b == nil {
			return a == b
		}
		return *a == *b
	}
	var out []*repository.SchemaProposal
	for i := len(m.proposals) - 1; i >= 0 && len(out) < limit; i-- {
		p := m.proposals[i]
		if p.TenantID == tenantID && p.ProposedBy == proposedBy && p.ProposedByKind == kind &&
			sameAgent(p.ProposedByAgent, agentID) {
			cp := *p
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (m *memRepo) ListSchemaProposals(_ context.Context, tenantID string, limit int) ([]*repository.SchemaProposal, error) {
	var out []*repository.SchemaProposal
	for i := len(m.proposals) - 1; i >= 0 && len(out) < limit; i-- {
		if m.proposals[i].TenantID == tenantID {
			cp := *m.proposals[i]
			out = append(out, &cp)
		}
	}
	return out, nil
}

// DecideSchemaProposal mirrors the real UPDATE's WHERE clause, including the
// two predicates that decide the answer under a race: still pending, and not
// past its deadline. A fake that wrote the decision unconditionally would make
// the double-approval test pass while the SQL that prevents it was missing.
func (m *memRepo) DecideSchemaProposal(_ context.Context, tenantID string, id uuid.UUID,
	status string, decidedBy uuid.UUID, now time.Time, appliedSteps []byte) error {
	for _, p := range m.proposals {
		if p.TenantID != tenantID || p.ID != id {
			continue
		}
		if p.Status != repository.ProposalPending || !p.ExpiresAt.After(now) {
			return repository.ErrProposalNotPending
		}
		p.Status = status
		decidedAt, by := now, decidedBy
		p.DecidedAt, p.DecidedBy = &decidedAt, &by
		p.AppliedSteps = appliedSteps
		return nil
	}
	return apperrors.ErrNotFound
}

func (m *memRepo) CreateContentType(_ context.Context, ct *domain.ContentType) error {
	for _, t := range m.types {
		if t.TenantID == ct.TenantID && t.Name == ct.Name {
			return apperrors.New("CONTENT_TYPE_EXISTS", "exists", 409)
		}
	}
	cp := *ct
	cp.Fields = append([]domain.Field(nil), ct.Fields...)
	m.types = append(m.types, &cp)
	return nil
}

func (m *memRepo) AddField(_ context.Context, _ string, f *domain.Field) error {
	for _, t := range m.types {
		if t.ID == f.ContentTypeID {
			t.Fields = append(t.Fields, *f)
			t.UpdatedAt = f.CreatedAt
			return nil
		}
	}
	return apperrors.ErrNotFound
}

// --- schema mutation ---------------------------------------------------------
//
// These exist so the service's REFUSALS can be tested — every guard fires before
// the corresponding write, so those tests do not depend on what follows being
// faithful. What these do NOT mirror, and what therefore has no meaningful
// assertion at this layer:
//
//   - JSONB operator semantics (`-`, `||`, jsonb_build_object) and the CASE
//     guards around them;
//   - ON DELETE CASCADE, which is a database behaviour with no Go analogue;
//   - the tenant join on the relation_entity cascade — the very thing whose
//     absence would corrupt another tenant.
//
// Those live in repository/schema_mutation_integration_test.go against real
// Postgres. The counters below ARE mirrored carefully, because a wrong count
// here would make a refusal test pass for the wrong reason.

// entryPayloads yields both copies of every entry of a type, which is what the
// SQL guards examine: a value edited out of the working copy but still live in
// the snapshot still constrains the schema.
func (m *memRepo) entryPayloads(tenantID string, ctID uuid.UUID) [][]map[string]any {
	var out [][]map[string]any
	for _, e := range m.entries {
		if e.TenantID != tenantID || e.ContentTypeID != ctID {
			continue
		}
		var copies []map[string]any
		for _, raw := range []json.RawMessage{e.Payload, e.PublishedPayload} {
			if len(raw) == 0 {
				continue
			}
			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err == nil {
				copies = append(copies, doc)
			}
		}
		out = append(out, copies)
	}
	return out
}

func (m *memRepo) CountEntriesForType(_ context.Context, tenantID string, ctID uuid.UUID) (int, error) {
	return len(m.entryPayloads(tenantID, ctID)), nil
}

func (m *memRepo) CountEntriesWithField(_ context.Context, tenantID string, ctID uuid.UUID, key string) (int, error) {
	n := 0
	for _, copies := range m.entryPayloads(tenantID, ctID) {
		for _, doc := range copies {
			if _, ok := doc[key]; ok {
				n++
				break
			}
		}
	}
	return n, nil
}

func (m *memRepo) CountEntriesMissingField(_ context.Context, tenantID string, ctID uuid.UUID, key string) (int, error) {
	n := 0
	for _, copies := range m.entryPayloads(tenantID, ctID) {
		if len(copies) == 0 {
			continue
		}
		// Working copy only, and "missing" means absent OR explicitly null —
		// exactly validatePayload's test (`!present || v == nil`). A bare
		// presence check would disagree with the validator on the rows that make
		// tightening `required` unsafe, which is the whole point of the count.
		v, present := copies[0][key]
		if !present || v == nil {
			n++
		}
	}
	return n, nil
}

func (m *memRepo) CountEntriesWithValuesOutside(_ context.Context, tenantID string, ctID uuid.UUID, f domain.Field, allowed []string) (int, error) {
	ok := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		ok[a] = struct{}{}
	}
	outside := func(v any) bool {
		s, isStr := v.(string)
		if !isStr {
			return false
		}
		_, allowed := ok[s]
		return !allowed
	}
	n := 0
	for _, copies := range m.entryPayloads(tenantID, ctID) {
		hit := false
		for _, doc := range copies {
			v, present := doc[f.Key]
			if !present || v == nil {
				continue
			}
			if f.Multiple {
				xs, isArr := v.([]any)
				if !isArr {
					continue
				}
				for _, x := range xs {
					if outside(x) {
						hit = true
					}
				}
				continue
			}
			if outside(v) {
				hit = true
			}
		}
		if hit {
			n++
		}
	}
	return n, nil
}

func (m *memRepo) ListRelationReferrers(_ context.Context, tenantID, typeName string) ([]repository.RelationRef, error) {
	var out []repository.RelationRef
	for _, t := range m.types {
		if t.TenantID != tenantID {
			continue
		}
		for _, f := range t.Fields {
			if f.Type == domain.FieldTypeRelation && f.RelationEntity == typeName {
				out = append(out, repository.RelationRef{TypeName: t.Name, FieldKey: f.Key})
			}
		}
	}
	return out, nil
}

func (m *memRepo) findType(id uuid.UUID) *domain.ContentType {
	for _, t := range m.types {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (m *memRepo) UpdateFieldDefinition(_ context.Context, _ string, ct *domain.ContentType, f domain.Field, now time.Time) error {
	t := m.findType(ct.ID)
	if t == nil {
		return apperrors.ErrNotFound
	}
	for i := range t.Fields {
		if t.Fields[i].Key == f.Key {
			t.Fields[i] = f
			t.UpdatedAt = now
			return nil
		}
	}
	return apperrors.ErrNotFound
}

func (m *memRepo) DeleteField(_ context.Context, tenantID string, ct *domain.ContentType, f domain.Field, actor domain.WriteActor, now time.Time) error {
	t := m.findType(ct.ID)
	if t == nil {
		return apperrors.ErrNotFound
	}
	m.rewriteEntryDocs(tenantID, ct.ID, func(doc map[string]any) bool {
		if _, ok := doc[f.Key]; !ok {
			return false
		}
		delete(doc, f.Key)
		return true
	}, actor, now)
	kept := t.Fields[:0]
	for _, x := range t.Fields {
		if x.Key != f.Key {
			kept = append(kept, x)
		}
	}
	t.Fields, t.UpdatedAt = kept, now
	return nil
}

func (m *memRepo) RenameField(_ context.Context, tenantID string, ct *domain.ContentType, oldKey, newKey string, actor domain.WriteActor, now time.Time) error {
	t := m.findType(ct.ID)
	if t == nil {
		return apperrors.ErrNotFound
	}
	m.rewriteEntryDocs(tenantID, ct.ID, func(doc map[string]any) bool {
		v, ok := doc[oldKey]
		if !ok {
			// Mirrors the SQL's CASE guard: a row that lacks the key must not
			// acquire {"new": null}.
			return false
		}
		delete(doc, oldKey)
		doc[newKey] = v
		return true
	}, actor, now)
	for i := range t.Fields {
		if t.Fields[i].Key == oldKey {
			t.Fields[i].Key = newKey
		}
	}
	t.UpdatedAt = now
	return nil
}

// rewriteEntryDocs applies mutate to both copies, bumping the matching version
// only for the copies that actually changed — the same per-copy CASE the SQL
// uses, because a snapshot that moves while published_version stands still is
// the lie ADR-006 Amendment 1a closed.
// rewriteEntryDocs mirrors the bulk schema statements, INCLUDING which rows get
// the last-write trio. The SQL guards those three columns on the working copy
// changing, not on the row being touched, because updated_by means "who last
// wrote the working copy" — a row whose published copy alone held the key has
// no new working-copy write to attribute. Stamping unconditionally here would
// make the fake agree with a version of the repository that answers a
// documented question wrongly, and service-level tests would ratify it.
func (m *memRepo) rewriteEntryDocs(tenantID string, ctID uuid.UUID, mutate func(map[string]any) bool, actor domain.WriteActor, now time.Time) {
	for _, e := range m.entries {
		if e.TenantID != tenantID || e.ContentTypeID != ctID {
			continue
		}
		if len(e.Payload) > 0 {
			var doc map[string]any
			if json.Unmarshal(e.Payload, &doc) == nil && mutate(doc) {
				if b, err := json.Marshal(doc); err == nil {
					e.Payload = b
					e.Version++
					e.UpdatedAt = now
					kind := actor.Kind
					e.UpdatedBy, e.UpdatedByKind, e.UpdatedByAgent = actor.UserID, &kind, actor.AgentID
				}
			}
		}
		if len(e.PublishedPayload) > 0 {
			var doc map[string]any
			if json.Unmarshal(e.PublishedPayload, &doc) == nil && mutate(doc) {
				if b, err := json.Marshal(doc); err == nil {
					e.PublishedPayload = b
					e.PublishedVersion++
				}
			}
		}
		setUnpublishedChanges(e)
	}
}

// UpdateContentTypeDefinition mirrors the Postgres implementation's contract:
// it writes all three permission lists UNCONDITIONALLY, from the type it is
// handed. A fake that only wrote the non-empty ones would make the service's
// "copy the stored lists forward" step untestable — the bug it guards against
// (an omitted list silently opening a collection) would pass here and fail in
// Postgres.
func (m *memRepo) UpdateContentTypeDefinition(_ context.Context, _ string, in *domain.ContentType, now time.Time) error {
	t := m.findType(in.ID)
	if t == nil {
		return apperrors.ErrNotFound
	}
	t.Label = in.Label
	t.ReadRoles, t.WriteRoles, t.OwnOnlyRoles = in.ReadRoles, in.WriteRoles, in.OwnOnlyRoles
	t.UpdatedAt = now
	return nil
}

func (m *memRepo) ListDuplicateFieldValues(_ context.Context, tenantID string, ctID uuid.UUID, f domain.Field) ([]repository.DuplicateValue, error) {
	// Same reservation rule as reservationsSQL: the working copy's value and,
	// while published, the snapshot's; never empty. Counted per entry, so an
	// entry holding the value in both copies is one holder.
	holders := map[[2]string]map[uuid.UUID]struct{}{}
	for _, e := range m.entries {
		if e.TenantID != tenantID || e.ContentTypeID != ctID {
			continue
		}
		for _, v := range m.reservedValues(e, f) {
			k := [2]string{e.Locale, v}
			if holders[k] == nil {
				holders[k] = map[uuid.UUID]struct{}{}
			}
			holders[k][e.ID] = struct{}{}
		}
	}
	var out []repository.DuplicateValue
	for k, ids := range holders {
		if len(ids) > 1 {
			out = append(out, repository.DuplicateValue{Locale: k[0], Value: k[1], Entries: len(ids)})
		}
	}
	return out, nil
}

func (m *memRepo) reservedValues(e *domain.Entry, f domain.Field) []string {
	seen := map[string]struct{}{}
	var out []string
	copies := []json.RawMessage{e.Payload}
	if e.Status == domain.StatusPublished {
		copies = append(copies, e.PublishedPayload)
	}
	for _, raw := range copies {
		var doc map[string]any
		if len(raw) == 0 || json.Unmarshal(raw, &doc) != nil {
			continue
		}
		v := domain.UniqueValue(f.Type, doc[f.Key])
		if _, dup := seen[v]; v == "" || dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func (m *memRepo) ScanFieldValues(_ context.Context, tenantID string, ctID uuid.UUID, key string, fn func(entryID uuid.UUID, working, live any) error) error {
	for _, e := range m.entries {
		if e.TenantID != tenantID || e.ContentTypeID != ctID {
			continue
		}
		pick := func(raw json.RawMessage) any {
			var doc map[string]any
			if len(raw) == 0 || json.Unmarshal(raw, &doc) != nil {
				return nil
			}
			return doc[key]
		}
		var live any
		if e.Status == domain.StatusPublished {
			live = pick(e.PublishedPayload)
		}
		if err := fn(e.ID, pick(e.Payload), live); err != nil {
			return err
		}
	}
	return nil
}

func (m *memRepo) CountEntriesWithoutAuthor(_ context.Context, tenantID string, ctID uuid.UUID) (int, error) {
	n := 0
	for _, e := range m.entries {
		if e.TenantID == tenantID && e.ContentTypeID == ctID && e.CreatedBy == nil {
			n++
		}
	}
	return n, nil
}

func (m *memRepo) RenameContentType(_ context.Context, tenantID string, id uuid.UUID, oldName, newName string, now time.Time) error {
	t := m.findType(id)
	if t == nil {
		return apperrors.ErrNotFound
	}
	for _, other := range m.types {
		if other.TenantID == tenantID && other.Name == newName {
			return apperrors.New("CONTENT_TYPE_EXISTS", "content type already exists", 409).
				WithDetails(map[string]any{"name": newName})
		}
	}
	t.Name, t.UpdatedAt = newName, now
	// Scoped to the tenant, mirroring the SQL's join. The real risk this stands
	// in for — rewriting another tenant's fields — is only assertable against
	// Postgres, where content_type_fields has no RLS of its own.
	for _, other := range m.types {
		if other.TenantID != tenantID {
			continue
		}
		for i := range other.Fields {
			if other.Fields[i].Type == domain.FieldTypeRelation && other.Fields[i].RelationEntity == oldName {
				other.Fields[i].RelationEntity = newName
				other.UpdatedAt = now
			}
		}
	}
	return nil
}

func (m *memRepo) DeleteContentType(_ context.Context, tenantID string, id uuid.UUID) error {
	for i, t := range m.types {
		if t.ID == id && t.TenantID == tenantID {
			m.types = append(m.types[:i], m.types[i+1:]...)
			// Stands in for ON DELETE CASCADE; the real behaviour, including
			// whether it respects RLS, is only observable in Postgres.
			kept := m.entries[:0]
			for _, e := range m.entries {
				if e.ContentTypeID != id {
					kept = append(kept, e)
				}
			}
			m.entries = kept
			return nil
		}
	}
	return apperrors.ErrNotFound
}

func (m *memRepo) GetContentTypeByName(_ context.Context, tenantID, name string) (*domain.ContentType, error) {
	for _, t := range m.types {
		if t.TenantID == tenantID && t.Name == name {
			cp := *t
			cp.Fields = append([]domain.Field(nil), t.Fields...)
			return &cp, nil
		}
	}
	return nil, apperrors.ErrNotFound
}

func (m *memRepo) GetContentTypeByID(_ context.Context, tenantID string, id uuid.UUID) (*domain.ContentType, error) {
	for _, t := range m.types {
		if t.TenantID == tenantID && t.ID == id {
			cp := *t
			cp.Fields = append([]domain.Field(nil), t.Fields...)
			return &cp, nil
		}
	}
	return nil, apperrors.ErrNotFound
}

func (m *memRepo) ListContentTypes(_ context.Context, tenantID string) ([]*domain.ContentType, error) {
	var out []*domain.ContentType
	for _, t := range m.types {
		if t.TenantID == tenantID {
			out = append(out, t)
		}
	}
	return out, nil
}

func (m *memRepo) CountContentTypes(_ context.Context, tenantID string) (int, error) {
	n := 0
	for _, t := range m.types {
		if t.TenantID == tenantID {
			n++
		}
	}
	return n, nil
}

func (m *memRepo) CountEntriesForTenant(_ context.Context, tenantID string) (int, error) {
	n := 0
	for _, e := range m.entries {
		if e.TenantID == tenantID {
			n++
		}
	}
	return n, nil
}

func (m *memRepo) CreateEntry(_ context.Context, e *domain.Entry) error {
	// Mirror the (tenant, translation_group_id, locale) unique index — without
	// it the duplicate-translation test would pass vacuously.
	for _, it := range m.entries {
		if it.TenantID == e.TenantID && it.TranslationGroupID == e.TranslationGroupID && it.Locale == e.Locale {
			return repository.ErrTranslationExists
		}
	}
	cp := *e
	// The Postgres INSERT writes created_by AND updated_by from the same value:
	// a new entry's last editor is its author.
	cp.UpdatedBy = clonePtr(e.CreatedBy)
	cp.CreatedBy = clonePtr(e.CreatedBy)
	// Mirror created_by_kind's column default (migration 000030). A fake that
	// stored "" where Postgres stores "human" would let a provenance assertion
	// pass here and fail against the database, or the reverse.
	if cp.CreatedByKind == "" {
		cp.CreatedByKind = domain.ActorKindHuman
	}
	// And the updated pair starts as a copy of the created one, mirroring the
	// INSERT's repeated parameters (migration 000031). Same hazard as the line
	// above: leaving it nil here would have the fake report "not recorded" for a
	// row it just authored, so a projector that dropped the pair would look right.
	kindCopy := cp.CreatedByKind
	cp.UpdatedByKind = &kindCopy
	cp.UpdatedByAgent = cloneStr(e.CreatedByAgent)
	cp.CreatedByAgent = cloneStr(e.CreatedByAgent)
	setUnpublishedChanges(&cp)
	m.entries = append(m.entries, &cp)
	e.UpdatedBy = clonePtr(cp.UpdatedBy)
	e.CreatedByKind = cp.CreatedByKind
	e.UpdatedByKind, e.UpdatedByAgent = cloneStr(cp.UpdatedByKind), cloneStr(cp.UpdatedByAgent)
	return nil
}

// clonePtr copies the POINTEE, not the pointer. `cp := *e` is a value copy, so
// the authorship pointers would otherwise be shared between the fake's stored
// row and the caller's struct — a mutation through one would appear in the
// other, which no real repository does.
func clonePtr(p *uuid.UUID) *uuid.UUID {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// cloneStr is clonePtr for the provenance kind/agent pair, which the schema
// stores as nullable text.
func cloneStr(p *string) *string {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func (m *memRepo) GetEntry(_ context.Context, tenantID string, contentTypeID, id uuid.UUID) (*domain.Entry, error) {
	for _, e := range m.entries {
		if e.TenantID == tenantID && e.ContentTypeID == contentTypeID && e.ID == id {
			cp := *e
			cp.CreatedBy, cp.UpdatedBy, cp.PublishedBy = clonePtr(e.CreatedBy), clonePtr(e.UpdatedBy), clonePtr(e.PublishedBy)
			cp.CreatedByAgent, cp.UpdatedByKind, cp.UpdatedByAgent = cloneStr(e.CreatedByAgent), cloneStr(e.UpdatedByKind), cloneStr(e.UpdatedByAgent)
			setUnpublishedChanges(&cp)
			return &cp, nil
		}
	}
	return nil, apperrors.ErrNotFound
}

// GetEntriesByIDs mirrors the batched read: same tenant, id set membership, and
// under matchPublished the SAME pair of conditions the SQL spells — published
// status AND a snapshot present. Both halves matter here: a retracted entry
// keeps its published_payload, so a fake that checked only the snapshot would
// agree with a query that served retracted content.
func (m *memRepo) GetEntriesByIDs(_ context.Context, tenantID string, ids []uuid.UUID, matchPublished bool) ([]*domain.Entry, error) {
	m.byIDsCalls++
	m.lastByIDs = append([]uuid.UUID(nil), ids...)
	want := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var out []*domain.Entry
	for _, e := range m.entries {
		if e.TenantID != tenantID || !want[e.ID] {
			continue
		}
		if matchPublished && (e.Status != domain.StatusPublished || len(e.PublishedPayload) == 0) {
			continue
		}
		cp := *e
		setUnpublishedChanges(&cp)
		out = append(out, &cp)
	}
	return out, nil
}

func (m *memRepo) UpdateEntry(_ context.Context, e *domain.Entry) error {
	for _, it := range m.entries {
		if it.TenantID == e.TenantID && it.ContentTypeID == e.ContentTypeID && it.ID == e.ID {
			// Mirror the Postgres optimistic lock: only apply when the stored
			// version still matches the version the caller read.
			if it.Version != e.Version {
				return repository.ErrVersionConflict
			}
			it.Payload = e.Payload
			// The Postgres UPDATE writes updated_at = $5 from the caller. Without
			// this the fake froze it, which would silently make any assertion
			// about updated_at leaking through the delivery DTO pass for free.
			it.UpdatedAt = e.UpdatedAt
			// Same reason as updated_at: the real UPDATE writes updated_by from
			// the caller, and a fake that froze it would make every "the editor
			// was recorded" assertion pass without the column being written.
			it.UpdatedBy = clonePtr(e.UpdatedBy)
			// The kind/agent pair rides the same UPDATE and for the same reason —
			// a fake that froze it would let "the agent's edit was recorded" pass
			// while the column still held whoever wrote the row before.
			it.UpdatedByKind, it.UpdatedByAgent = cloneStr(e.UpdatedByKind), cloneStr(e.UpdatedByAgent)
			// Mirrors the Postgres UPDATE, which writes the payload ONLY: saving
			// a draft must not be able to move editorial state or the published
			// snapshot. That is SetEntryPublishState's job.
			it.Version++
			// Mirrors RETURNING: the flag comes back recomputed from the row the
			// write just produced, so the caller never holds a stale one.
			setUnpublishedChanges(it)
			// Mirrors markSchedulesStale, which runs in the same transaction as
			// this UPDATE: the working copy moved past what was approved, so a
			// pending schedule no longer describes anything anyone signed off.
			m.markSchedulesStale(it)
			e.Version = it.Version
			e.HasUnpublishedChanges = it.HasUnpublishedChanges
			return nil
		}
	}
	return apperrors.ErrNotFound
}

// SetEntryPublishState mirrors the Postgres statement: flip status, move the
// snapshot, bump version — and write published_version as the POST-bump value,
// which is what keeps HasUnpublishedChanges false right after a publish.
func (m *memRepo) SetEntryPublishState(_ context.Context, e *domain.Entry, status string, publishedAt *time.Time, opts ...domain.PublishOrigin) error {
	for _, it := range m.entries {
		if it.TenantID != e.TenantID || it.ContentTypeID != e.ContentTypeID || it.ID != e.ID {
			continue
		}
		if it.Version != e.Version {
			return repository.ErrVersionConflict
		}
		// Mirrors markSchedulesSuperseded. Somebody did by hand what the
		// schedule was waiting to do; leaving the row pending would fire it a
		// second time against whatever the entry looks like then.
		m.markSchedulesSuperseded(it.TenantID, it.ID)
		it.Version++
		it.Status = status
		it.PublishedAt = publishedAt
		it.UpdatedAt = e.UpdatedAt
		it.UpdatedBy = clonePtr(e.UpdatedBy)
		// Publish and unpublish are writes: the pair follows updated_by here as it
		// does in UpdateEntry, or a person publishing over a bot's draft would
		// leave the row still attributing the last change to the bot.
		it.UpdatedByKind, it.UpdatedByAgent = cloneStr(e.UpdatedByKind), cloneStr(e.UpdatedByAgent)
		// Mirrors the three CASE arms in SetEntryPublishState: on a publish the
		// snapshot columns take the working copy, and on a RETRACT they keep the
		// values they already hold. ADR-014 §5.1 made that survival the rule —
		// before it, all three were nulled here, and this fake went on nulling
		// them for two steps after the SQL stopped. What that hid: every
		// service-layer test involving a retracted entry was asserting against
		// pre-§5.1 semantics, so "has a snapshot" and "is live" stayed the same
		// condition in Go while they had already come apart in the database.
		if status == domain.StatusPublished {
			it.PublishedPayload = it.Payload
			it.PublishedVersion = it.Version
			it.PublishedBy = clonePtr(e.PublishedBy)
			if m.publishedLinks == nil {
				m.publishedLinks = map[uuid.UUID][]uuid.UUID{}
			}
			m.publishedLinks[it.ID] = append([]uuid.UUID(nil), m.links[it.ID]...)
			if m.publishedRelations == nil {
				m.publishedRelations = map[uuid.UUID][]repository.EntryRelationRef{}
			}
			m.publishedRelations[it.ID] = append([]repository.EntryRelationRef(nil), m.relations[it.ID]...)
		} else {
			// The entry_media links are the one thing a retract DOES drop: the
			// snapshot survives, but nothing offline may keep a signed asset
			// reachable. Migration 000020's DELETE is unconditional for the same
			// reason.
			delete(m.publishedLinks, it.ID)
			delete(m.publishedRelations, it.ID)
		}
		setUnpublishedChanges(it)
		// The publish revision, written HERE and only on a publish, mirroring
		// recordPublishRevision's position inside the same transaction. Every
		// value is read off the STORED row rather than off the caller's entry,
		// as the SQL's INSERT ... SELECT does — copying from `e` would let a
		// test's hand-built argument disagree with what a publish actually
		// lands, which is the drift the real statement is shaped to prevent.
		if status == domain.StatusPublished {
			var origin domain.PublishOrigin
			if len(opts) > 0 {
				origin = opts[0]
			}
			if m.publishRevisions == nil {
				m.publishRevisions = map[uuid.UUID][]domain.EntryPublishRevision{}
			}
			next := 1
			if prev := m.publishRevisions[it.ID]; len(prev) > 0 {
				next = prev[len(prev)-1].RevisionNo + 1
			}
			m.publishRevisions[it.ID] = append(m.publishRevisions[it.ID], domain.EntryPublishRevision{
				ID:            uuid.New(),
				TenantID:      it.TenantID,
				EntryID:       it.ID,
				RevisionNo:    next,
				Payload:       append(json.RawMessage(nil), it.PublishedPayload...),
				Version:       it.PublishedVersion,
				PublishedAt:   it.UpdatedAt,
				PublishedBy:   clonePtr(it.PublishedBy),
				Via:           origin.Via,
				ViaScheduleID: clonePtr(origin.ScheduleID),
				CreatedAt:     it.UpdatedAt,
			})
			// Retention, trimming the OLDEST, exactly as the DELETE does.
			if over := len(m.publishRevisions[it.ID]) - domain.MaxPublishRevisionsPerEntry; over > 0 {
				m.publishRevisions[it.ID] = m.publishRevisions[it.ID][over:]
			}
		}
		e.Version = it.Version
		e.Status = it.Status
		e.PublishedAt = it.PublishedAt
		e.PublishedPayload = it.PublishedPayload
		e.PublishedVersion = it.PublishedVersion
		e.PublishedBy = clonePtr(it.PublishedBy)
		e.HasUnpublishedChanges = it.HasUnpublishedChanges
		return nil
	}
	return apperrors.ErrNotFound
}

func (m *memRepo) ListEntryPublishRevisions(_ context.Context, tenantID string, entryID uuid.UUID) ([]domain.EntryPublishRevision, error) {
	// Newest first, and tenant-scoped in Go because the real query is
	// tenant-scoped twice (RLS plus the WHERE). A fake that ignored tenantID
	// would let a cross-tenant read pass here and fail only in the integration
	// suite.
	var out []domain.EntryPublishRevision
	rows := m.publishRevisions[entryID]
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].TenantID != tenantID {
			continue
		}
		out = append(out, rows[i])
	}
	return out, nil
}

func (m *memRepo) GetEntryPublishRevision(_ context.Context, tenantID string, entryID uuid.UUID, revisionNo int) (*domain.EntryPublishRevision, error) {
	for _, r := range m.publishRevisions[entryID] {
		if r.TenantID == tenantID && r.RevisionNo == revisionNo {
			rev := r
			return &rev, nil
		}
	}
	return nil, apperrors.ErrNotFound
}

func (m *memRepo) DeleteEntry(_ context.Context, tenantID string, contentTypeID, id uuid.UUID) error {
	for i, e := range m.entries {
		if e.TenantID == tenantID && e.ContentTypeID == contentTypeID && e.ID == id {
			m.entries = append(m.entries[:i], m.entries[i+1:]...)
			return nil
		}
	}
	return apperrors.ErrNotFound
}

// memSortValue mirrors Postgres `->>` over the copy the ORDER BY reads: the
// published snapshot under MatchPublished, the working copy otherwise. The
// second return is "the row has a value here" — false for an absent key and for
// a JSON null, both of which are SQL NULL and sort last.
func memSortValue(e *domain.Entry, f repository.ListEntriesFilter) (string, bool) {
	payload := e.Payload
	if f.MatchPublished {
		payload = e.PublishedPayload
	}
	v := sortValueText(payload, f.Sort.Field.Key)
	if v == nil {
		return "", false
	}
	return *v, true
}

// memSortLess mirrors the comparison the SQL performs AFTER orderedExpr's cast:
// numerically for number, by instant for datetime, by text otherwise. Comparing
// everything as text is the bug this exists to avoid — '10' < '9' as text, and
// two spellings of one instant compare unequal.
func memSortLess(a, b, fieldType string) bool {
	switch fieldType {
	case domain.FieldTypeNumber:
		af, aerr := strconv.ParseFloat(a, 64)
		bf, berr := strconv.ParseFloat(b, 64)
		if aerr == nil && berr == nil {
			return af < bf
		}
	case domain.FieldTypeDateTime:
		at, aerr := time.Parse(time.RFC3339, a)
		bt, berr := time.Parse(time.RFC3339, b)
		if aerr == nil && berr == nil {
			return at.Before(bt)
		}
	}
	return a < b
}

// memIDBefore is the id tiebreaker: does a sort BEFORE b in this direction?
// Note the asymmetry it exists to keep straight — "earlier in the page" is a
// LARGER id under desc, so the ordering comparator and the cursor window read
// it with their arguments the other way round.
//
// String comparison mirrors Postgres's uuid ordering because uuid.String() is
// fixed-width lowercase hex with the hyphens at fixed positions, so
// lexicographic order over the text equals byte order over the 16-byte value.
// That equivalence is why the fake and the database agree; it is not a general
// property of "compare uuids as strings".
func memIDBefore(a, b uuid.UUID, desc bool) bool {
	if desc {
		return a.String() > b.String()
	}
	return a.String() < b.String()
}

// memPastCursor mirrors the keyset predicate: is this row STRICTLY past the
// cursor in the order the filter declares?
func memPastCursor(e *domain.Entry, f repository.ListEntriesFilter) bool {
	if f.Sort == nil {
		if e.CreatedAt.After(f.After.CreatedAt) {
			return false
		}
		return !e.CreatedAt.Equal(f.After.CreatedAt) || memIDBefore(f.After.ID, e.ID, true)
	}
	v, ok := memSortValue(e, f)
	if f.After.SortValue == nil {
		// Already inside the trailing null block: only null rows further along
		// by id remain.
		return !ok && memIDBefore(f.After.ID, e.ID, f.Sort.Desc)
	}
	if !ok {
		// The whole null block sorts after every valued row, so it is entirely
		// ahead of a cursor that still carries a value.
		return true
	}
	cv := *f.After.SortValue
	ft := f.Sort.Field.Type
	switch {
	case memSortLess(v, cv, ft):
		return f.Sort.Desc
	case memSortLess(cv, v, ft):
		return !f.Sort.Desc
	default:
		return memIDBefore(f.After.ID, e.ID, f.Sort.Desc)
	}
}

func (m *memRepo) ListEntries(_ context.Context, f repository.ListEntriesFilter) ([]*domain.Entry, int, error) {
	// Recorded so a test can assert WHAT the service asked the repository for.
	// memRepo does not implement Filters/Sort, so "the rows came back filtered"
	// is not a claim this fake can support — but "the service bound the
	// predicate to the snapshot" is a property of the request, and that one it
	// can pin (ADR-006 Amendment 4).
	m.lastList = f
	var out []*domain.Entry
	for _, e := range m.entries {
		if e.TenantID != f.TenantID || e.ContentTypeID != f.ContentTypeID {
			continue
		}
		// Mirror the status column predicate; empty Status means all states.
		if f.Status != "" && e.Status != f.Status {
			continue
		}
		if f.Locale != "" && e.Locale != f.Locale {
			continue
		}
		if f.TranslationGroupID != uuid.Nil && e.TranslationGroupID != f.TranslationGroupID {
			continue
		}
		// Confinement, mirroring the `created_by = $n` column predicate — and
		// applied BEFORE `total` is computed below, which is the property the fake
		// exists to pin. A fake that filtered after counting would agree with the
		// service on which rows come back while disagreeing on the number, and the
		// number is exactly what leaks the hidden count.
		//
		// A NULL author matches nothing, like the SQL: `created_by = $n` is never
		// true of NULL.
		if f.CreatedBy != nil && (e.CreatedBy == nil || *e.CreatedBy != *f.CreatedBy) {
			continue
		}
		setUnpublishedChanges(e)
		out = append(out, e)
	}
	// Mirror the Postgres ORDER BY. The id tiebreaker is not decoration —
	// keyset pagination is only correct if the fake and the database agree on a
	// TOTAL order, and neither created_at nor a payload key is unique. Filters
	// remain unimplemented on purpose (see the comment on the cursor tests);
	// every caller that reaches here with them set has already been checked by
	// the service, which is the property those tests pin.
	//
	// Sort IS implemented, because ADR-006 Amendment 5 makes the ORDER BY part
	// of what the cursor has to reproduce: a fake that ignored it would page
	// through created_at order while the service believed it was paging through
	// a sort key, and "every row exactly once" would pass without testing the
	// thing it names.
	if f.Sort != nil {
		// `(value IS NULL), value <dir>, id <dir>` — missing values last in
		// BOTH directions, which is the ordering the repository spells with a
		// leading boolean rather than relying on Postgres's direction-dependent
		// NULLS default.
		sort.SliceStable(out, func(i, j int) bool {
			av, aok := memSortValue(out[i], f)
			bv, bok := memSortValue(out[j], f)
			if aok != bok {
				return aok
			}
			if aok {
				switch {
				case memSortLess(av, bv, f.Sort.Field.Type):
					return !f.Sort.Desc
				case memSortLess(bv, av, f.Sort.Field.Type):
					return f.Sort.Desc
				}
			}
			return memIDBefore(out[i].ID, out[j].ID, f.Sort.Desc)
		})
	} else {
		sort.SliceStable(out, func(i, j int) bool {
			if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
				return out[i].CreatedAt.After(out[j].CreatedAt)
			}
			return out[i].ID.String() > out[j].ID.String()
		})
	}
	total := len(out)

	// Keyset window: strictly past the cursor in the order above.
	if f.CursorPaged && f.After != nil {
		kept := out[:0:0]
		for _, e := range out {
			if memPastCursor(e, f) {
				kept = append(kept, e)
			}
		}
		out = kept
	}
	if f.CursorPaged {
		// Postgres issues no COUNT(*) in this mode; a fake that still returned a
		// real total would let a test read a number the real repo never supplies.
		total = 0
	} else if f.Offset > 0 {
		if f.Offset >= len(out) {
			out = nil
		} else {
			out = out[f.Offset:]
		}
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, total, nil
}

func (m *memRepo) EntryExists(_ context.Context, tenantID string, contentTypeID, id uuid.UUID) (bool, error) {
	for _, e := range m.entries {
		if e.TenantID == tenantID && e.ContentTypeID == contentTypeID && e.ID == id {
			return true, nil
		}
	}
	return false, nil
}

func (m *memRepo) AddDeliveryReads(_ context.Context, tenantID string, day time.Time, n int64) error {
	if m.deliveryReads == nil {
		m.deliveryReads = map[string]int64{}
	}
	m.deliveryReads[tenantID+"|"+day.UTC().Format("2006-01-02")] += n
	return nil
}

func (m *memRepo) DeliveryReadsForDay(_ context.Context, tenantID string, day time.Time) (int64, error) {
	return m.deliveryReads[tenantID+"|"+day.UTC().Format("2006-01-02")], nil
}

func (m *memRepo) CreateMediaAsset(_ context.Context, a *domain.MediaAsset) error {
	cp := *a
	m.assets = append(m.assets, &cp)
	return nil
}

func (m *memRepo) GetMediaAsset(_ context.Context, tenantID string, id uuid.UUID) (*domain.MediaAsset, error) {
	for _, a := range m.assets {
		if a.TenantID == tenantID && a.ID == id {
			cp := *a
			return &cp, nil
		}
	}
	return nil, apperrors.ErrNotFound
}

// ListMediaAssets mirrors the Postgres WHERE and ORDER BY exactly: only
// uploaded assets, the same two optional filters, newest-first with id as the
// tiebreaker, then the same clamp rule as the repository applies for a caller
// that reaches the fake directly (a unit test) rather than through the
// service (which already clamped).
func (m *memRepo) ListMediaAssets(_ context.Context, tenantID string, f repository.MediaListFilter) ([]*domain.MediaAsset, int, error) {
	var matched []*domain.MediaAsset
	q := strings.ToLower(strings.TrimSpace(f.Query))
	for _, a := range m.assets {
		if a.TenantID != tenantID {
			continue
		}
		// Mirrors Postgres ListMediaAssets: the library lists uploaded assets
		// only, EXCEPT under Orphan=true, where reservations are sweep
		// candidates too (an uncompleted reservation names no link table, so
		// it satisfies the NOT EXISTS definition trivially). Fresh ones are
		// excluded by the sweep's age filter, not by this list.
		if a.UploadedAt == nil && !f.Orphan {
			continue
		}
		if q != "" && (a.Filename == nil || !strings.Contains(strings.ToLower(*a.Filename), q)) {
			continue
		}
		if f.ContentTypePrefix != "" && !strings.HasPrefix(a.ContentType, f.ContentTypePrefix) {
			continue
		}
		if f.Orphan && m.assetReferenced(a.ID) {
			continue
		}
		cp := *a
		matched = append(matched, &cp)
	}
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.After(matched[j].CreatedAt)
		}
		return matched[i].ID.String() > matched[j].ID.String()
	})
	total := len(matched)

	limit := f.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	if offset >= len(matched) {
		return []*domain.MediaAsset{}, total, nil
	}
	end := offset + limit
	if end > len(matched) {
		end = len(matched)
	}
	return matched[offset:end], total, nil
}

func (m *memRepo) MarkMediaUploaded(_ context.Context, tenantID string, id uuid.UUID, size int64, ct string) error {
	for _, a := range m.assets {
		if a.TenantID == tenantID && a.ID == id {
			now := time.Now().UTC()
			a.UploadedAt, a.SizeBytes, a.ContentType = &now, size, ct
			return nil
		}
	}
	return apperrors.ErrNotFound
}

// UpdateMediaAssetMetadata mirrors the per-column SET list, INCLUDING the fact
// that an unset flag leaves the column alone. Assigning all four unconditionally
// would make this fake disagree with the SQL about the one property
// MediaAssetPatch exists for.
//
// What it CANNOT do is enforce a CHECK constraint, which is why every limit
// assertion for these columns lives in
// repository/media_metadata_integration_test.go against a real Postgres and not
// here. See the header of that file.
func (m *memRepo) UpdateMediaAssetMetadata(_ context.Context, tenantID string, id uuid.UUID, p repository.MediaAssetPatch) (*domain.MediaAsset, error) {
	for _, a := range m.assets {
		if a.TenantID != tenantID || a.ID != id {
			continue
		}
		if p.SetFilename {
			a.Filename = p.Filename
		}
		if p.SetAltText {
			a.AltText = p.AltText
		}
		if p.SetDimensions {
			a.WidthPx, a.HeightPx = p.WidthPx, p.HeightPx
		}
		cp := *a
		return &cp, nil
	}
	return nil, apperrors.ErrNotFound
}

func (m *memRepo) DeleteMediaAsset(_ context.Context, tenantID string, id uuid.UUID) ([]string, error) {
	for i, a := range m.assets {
		if a.TenantID == tenantID && a.ID == id {
			m.assets = append(m.assets[:i], m.assets[i+1:]...)
			// FK cascade, and the keys the service must sweep — never the
			// original's, which is the asset's own key.
			var keys []string
			kept := m.variants[:0]
			for _, v := range m.variants {
				if v.AssetID != id {
					kept = append(kept, v)
					continue
				}
				if v.Preset != domain.MediaPresetOriginal && v.StorageKey != "" {
					keys = append(keys, v.StorageKey)
				}
			}
			m.variants = kept
			return keys, nil
		}
	}
	return nil, apperrors.ErrNotFound
}

// --- media variants (000043) -------------------------------------------------

func (m *memRepo) EnqueueMediaVariants(_ context.Context, tenantID string, assetID uuid.UUID, storageKey string) error {
	presets := append([]string{domain.MediaPresetOriginal}, domain.MediaPresetNames()...)
	now := time.Now().UTC()
	for _, p := range presets {
		var row *domain.MediaVariant
		for _, v := range m.variants {
			if v.AssetID == assetID && v.Preset == p {
				row = v
				break
			}
		}
		if row == nil {
			row = &domain.MediaVariant{AssetID: assetID, TenantID: tenantID, Preset: p, CreatedAt: now}
			if p == domain.MediaPresetOriginal {
				row.StorageKey = storageKey
			}
			m.variants = append(m.variants, row)
		}
		// The upsert's SET list: state back to pending, counters cleared,
		// storage_key kept so a regenerated object overwrites its predecessor.
		row.State = domain.MediaVariantPending
		row.Attempts, row.NextAttemptAt = 0, time.Time{}
		row.Error, row.SkipReason = "", ""
		row.UpdatedAt = now
	}
	return nil
}

func (m *memRepo) ListMediaVariants(_ context.Context, tenantID string, assetID uuid.UUID) ([]*domain.MediaVariant, error) {
	var out []*domain.MediaVariant
	for _, p := range append([]string{domain.MediaPresetOriginal}, domain.MediaPresetNames()...) {
		for _, v := range m.variants {
			if v.TenantID == tenantID && v.AssetID == assetID && v.Preset == p {
				cp := *v
				out = append(out, &cp)
			}
		}
	}
	return out, nil
}

func (m *memRepo) ListPendingMediaVariantAssets(_ context.Context, now time.Time, limit int) ([]repository.MediaVariantAssetRef, error) {
	seen := map[uuid.UUID]bool{}
	var out []repository.MediaVariantAssetRef
	for _, v := range m.variants {
		if v.State != domain.MediaVariantPending || v.NextAttemptAt.After(now) || seen[v.AssetID] {
			continue
		}
		seen[v.AssetID] = true
		out = append(out, repository.MediaVariantAssetRef{AssetID: v.AssetID, TenantID: v.TenantID})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memRepo) ClaimPendingMediaVariants(ctx context.Context, tenantID string, assetID uuid.UUID, now time.Time) ([]*domain.MediaVariant, error) {
	all, _ := m.ListMediaVariants(ctx, tenantID, assetID)
	var out []*domain.MediaVariant
	for _, v := range all {
		if v.State == domain.MediaVariantPending && !v.NextAttemptAt.After(now) {
			out = append(out, v)
		}
	}
	return out, nil
}

func (m *memRepo) FinishMediaVariant(_ context.Context, tenantID string, assetID uuid.UUID, preset string, o repository.MediaVariantOutcome) (bool, error) {
	for _, v := range m.variants {
		if v.TenantID != tenantID || v.AssetID != assetID || v.Preset != preset {
			continue
		}
		v.State = o.State
		v.UpdatedAt = time.Now().UTC()
		switch o.State {
		case domain.MediaVariantDone:
			v.StorageKey, v.ContentType, v.SizeBytes = o.StorageKey, o.ContentType, o.SizeBytes
			v.WidthPx, v.HeightPx = o.WidthPx, o.HeightPx
			v.Error, v.SkipReason, v.NextAttemptAt = "", "", time.Time{}
		case domain.MediaVariantSkipped:
			v.SkipReason, v.Error, v.NextAttemptAt = o.SkipReason, "", time.Time{}
		case domain.MediaVariantPending, domain.MediaVariantFailed:
			v.Attempts++
			v.Error = o.Error
			v.NextAttemptAt = time.Time{}
			if o.NextAttemptAt != nil {
				v.NextAttemptAt = *o.NextAttemptAt
			}
		default:
			return false, fmt.Errorf("unknown state %q", o.State)
		}
		return true, nil
	}
	return false, nil
}

func (m *memRepo) CreateWebhook(_ context.Context, w *domain.Webhook) error {
	m.webhooks = append(m.webhooks, w)
	return nil
}

func (m *memRepo) ListWebhooks(_ context.Context, tenantID string) ([]*domain.Webhook, error) {
	var out []*domain.Webhook
	for _, w := range m.webhooks {
		if w.TenantID == tenantID {
			out = append(out, w)
		}
	}
	return out, nil
}

func (m *memRepo) DeleteWebhook(_ context.Context, tenantID string, id uuid.UUID) error {
	for i, w := range m.webhooks {
		if w.TenantID == tenantID && w.ID == id {
			m.webhooks = append(m.webhooks[:i], m.webhooks[i+1:]...)
			return nil
		}
	}
	return apperrors.ErrNotFound
}

// --- activity record (ADR-014 §3) --------------------------------------------
//
// The fake keeps rows in insertion order and REVERSES on read, because the
// Postgres query is `ORDER BY occurred_at DESC`. Several rows of one request
// share a wall-clock timestamp here — the fake stamps them from the same
// time.Now() the service does — so sorting by OccurredAt in the fake would give
// an order that depends on map/slice luck rather than on arrival, and tests
// asserting "the newest row is X" would pass or fail by accident.
//
// What this fake CANNOT check, and where those assertions therefore live: every
// CHECK constraint in migration 000032 (a half-written actor, a denied row
// claiming changed keys, a success carrying an error code), the append-only
// policies, and RLS. Those are the Postgres suite's — see the memRepo header.

func (m *memRepo) RecordActivity(_ context.Context, a *domain.Activity) error {
	cp := *a
	cp.ChangedKeys = append([]string(nil), a.ChangedKeys...)
	m.activity = append(m.activity, &cp)
	return nil
}

// activityVisible mirrors the stream's half of ADR-009's data layer, which is
// spelled differently from the queue's and has to be mirrored separately:
// content_activity holds no content_type_id, so the type is matched by NAME, and
// confinement resolves against the ACTOR rather than the entry's author (the
// entry may be deleted — see ListActivity).
//
// A NAME THAT MATCHES NO TYPE IS VISIBLE, which covers both the ” rows (schema
// verbs, collection reads) and rows naming a type that has since been renamed.
// That is the SQL's behaviour and it is a deliberate trade, not an omission;
// mirroring it here keeps a service test from quietly asserting the opposite.
func (m *memRepo) activityVisible(a *domain.Activity, role string, viewer uuid.UUID) bool {
	var ct *domain.ContentType
	for _, t := range m.types {
		if t.TenantID == a.TenantID && t.Name == a.TargetType {
			ct = t
			break
		}
	}
	if ct == nil {
		return true
	}
	if !ct.EntriesReadableBy(role) {
		return false
	}
	if !ct.ConfinesToOwn(role) {
		return true
	}
	return a.ActorUserID != nil && *a.ActorUserID == viewer
}

func (m *memRepo) ListActivity(_ context.Context, f repository.ActivityFilter) ([]*domain.Activity, error) {
	var out []*domain.Activity
	for i := len(m.activity) - 1; i >= 0; i-- {
		a := m.activity[i]
		if a.TenantID != f.TenantID {
			continue
		}
		if f.EntryID != nil && (a.TargetEntryID == nil || *a.TargetEntryID != *f.EntryID) {
			continue
		}
		if f.Since != nil && !a.OccurredAt.After(*f.Since) {
			continue
		}
		if !m.activityVisible(a, f.ViewerRole, f.ViewerUserID) {
			continue
		}
		out = append(out, a)
		if f.Limit > 0 && len(out) == f.Limit {
			break
		}
	}
	return out, nil
}

// viewerMaySee mirrors pendingReviewVisibleExpr — ADR-009's data layer as the
// cross-type queries apply it: the type's read list, then own_only confinement
// against the row's author.
//
// IT IS HERE SO THAT A SERVICE TEST CANNOT ASSERT THE OLD BEHAVIOUR AND PASS. A
// fake that ignored ViewerRole would happily return a confined caller their
// colleagues' drafts, and a test written against that fake would then pin the
// leak in place with a green check beside it. What this MIRROR cannot do is show
// that the SQL says the same thing — that half is
// TestCrossTypeReadsApplyDataPermission, against a real database, and it is the
// authority whenever the two disagree.
func (m *memRepo) viewerMaySee(e *domain.Entry, role string, viewer uuid.UUID) bool {
	var ct *domain.ContentType
	for _, t := range m.types {
		if t.ID == e.ContentTypeID {
			ct = t
			break
		}
	}
	if ct == nil {
		return false
	}
	if !ct.EntriesReadableBy(role) {
		return false
	}
	if !ct.ConfinesToOwn(role) {
		return true
	}
	return e.CreatedBy != nil && *e.CreatedBy == viewer
}

// ListPendingReview mirrors pendingReviewExpr: not live, OR live and edited.
//
// It is a REWRITE of that predicate, not an execution of it, so what this can
// prove is limited in the way memRepo always is — it pins the service's use of
// the queue (authorization, titles, DTO shape), and it cannot see whether the
// SQL says the same thing. TestPendingReviewSpansTypesAndStates in the
// repository's integration suite is the half that runs the real predicate.
//
// Agent-first ordering is mirrored too, because the service hands this order
// straight to the console and a fake that returned insertion order would let a
// sort bug through to a test that reads position.
func (m *memRepo) ListPendingReview(_ context.Context, f repository.PendingReviewFilter) ([]*domain.Entry, error) {
	var out []*domain.Entry
	for _, e := range m.entries {
		if e.TenantID != f.TenantID {
			continue
		}
		setUnpublishedChanges(e)
		if e.Status == domain.StatusPublished && !e.HasUnpublishedChanges {
			continue
		}
		// Mirrors reviewDecisionNotSentBackExpr (postgres_repository.go): an
		// entry whose latest decision still names the entry's current version
		// is sent back and stays out of the queue until the next save moves
		// the version on.
		if latest := m.latestReviewDecision(e.TenantID, e.ID); latest != nil && latest.EntryVersion == e.Version {
			continue
		}
		if !m.viewerMaySee(e, f.ViewerRole, f.ViewerUserID) {
			continue
		}
		out = append(out, e)
	}
	byAgent := func(e *domain.Entry) bool {
		return e.UpdatedByKind != nil && *e.UpdatedByKind == domain.ActorKindAgent
	}
	sort.SliceStable(out, func(i, j int) bool {
		if byAgent(out[i]) != byAgent(out[j]) {
			return byAgent(out[i])
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// SearchEntries mirrors dataVisibleExpr and the ILIKE-per-term AND the SQL
// builds (postgres_repository.go's SearchEntries): a case-insensitive
// substring match against SearchText, all terms required, same viewer gate as
// ListPendingReview above. The service tests exercising this care about
// authorization, snippet computation and DTO shape; SearchEntriesMatchesAll…
// in the repository's own integration suite is the half that runs the real
// pg_trgm-backed predicate.
func (m *memRepo) SearchEntries(_ context.Context, f repository.SearchEntriesFilter) ([]repository.SearchEntryRow, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	var out []*domain.Entry
	for _, e := range m.entries {
		if e.TenantID != f.TenantID {
			continue
		}
		if !m.viewerMaySee(e, f.ViewerRole, f.ViewerUserID) {
			continue
		}
		if f.Locale != "" && e.Locale != f.Locale {
			continue
		}
		if f.Status != "" && e.Status != f.Status {
			continue
		}
		if !searchTextMatchesAllTerms(e.SearchText, f.Query) {
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	rows := make([]repository.SearchEntryRow, len(out))
	for i, e := range out {
		rows[i] = repository.SearchEntryRow{
			ID: e.ID, ContentTypeID: e.ContentTypeID, Locale: e.Locale, Status: e.Status,
			UpdatedAt: e.UpdatedAt, SearchText: e.SearchText,
		}
	}
	return rows, nil
}

// ListLocales mirrors the repository's GROUP BY locale: same tenant scope and
// viewerMaySee gate as SearchEntries above, optionally narrowed to one
// content type, counted and sorted by locale.
func (m *memRepo) ListLocales(_ context.Context, f repository.ListLocalesFilter) ([]repository.LocaleCountRow, error) {
	counts := map[string]int{}
	for _, e := range m.entries {
		if e.TenantID != f.TenantID {
			continue
		}
		if f.ContentTypeID != nil && e.ContentTypeID != *f.ContentTypeID {
			continue
		}
		if !m.viewerMaySee(e, f.ViewerRole, f.ViewerUserID) {
			continue
		}
		counts[e.Locale]++
	}
	locales := make([]string, 0, len(counts))
	for loc := range counts {
		locales = append(locales, loc)
	}
	sort.Strings(locales)
	out := make([]repository.LocaleCountRow, len(locales))
	for i, loc := range locales {
		out[i] = repository.LocaleCountRow{Locale: loc, Entries: counts[loc]}
	}
	return out, nil
}

func searchTextMatchesAllTerms(text, query string) bool {
	lower := strings.ToLower(text)
	for _, term := range strings.Fields(query) {
		if !strings.Contains(lower, strings.ToLower(term)) {
			return false
		}
	}
	return true
}

// EntryFieldAuthors mirrors the DISTINCT ON: newest write to a key wins, and a
// key nobody wrote in the window is ABSENT rather than present-and-empty —
// absence is what the console renders as unknown.
//
// "Newest" is arrival order here and `occurred_at DESC, id DESC` in Postgres,
// the same divergence the header above states for ListActivity and for the same
// reason: rows of one request share a timestamp to the microsecond, so the fake
// cannot sort on it without making the winner depend on slice luck.
func (m *memRepo) EntryFieldAuthors(_ context.Context, tenantID string, entryID uuid.UUID, since *time.Time) ([]*domain.FieldAuthor, error) {
	seen := map[string]bool{}
	var out []*domain.FieldAuthor
	for i := len(m.activity) - 1; i >= 0; i-- {
		a := m.activity[i]
		if a.TenantID != tenantID || a.TargetEntryID == nil || *a.TargetEntryID != entryID {
			continue
		}
		// Exclusive, matching the SQL's `>`.
		if since != nil && !a.OccurredAt.After(*since) {
			continue
		}
		for _, k := range a.ChangedKeys {
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, &domain.FieldAuthor{
				Key:          k,
				ActorKind:    a.ActorKind,
				ActorUserID:  a.ActorUserID,
				ActorAgentID: a.ActorAgentID,
				OccurredAt:   a.OccurredAt,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *memRepo) ReplaceEntryMedia(_ context.Context, _ string, entryID uuid.UUID, assetIDs []uuid.UUID) error {
	if m.links == nil {
		m.links = map[uuid.UUID][]uuid.UUID{}
	}
	m.links[entryID] = append([]uuid.UUID(nil), assetIDs...)
	return nil
}

// ReplaceEntryRelations mirrors ReplaceEntryMedia's shape for entry_relations
// (000050, ADR-024): a full delete-then-insert of the entry's WORKING-copy
// links, keyed by entry id, the same "one call replaces the whole set" contract
// checkRelationsIn's caller relies on.
func (m *memRepo) ReplaceEntryRelations(_ context.Context, _ string, entryID uuid.UUID, refs []repository.EntryRelationRef) error {
	if m.relations == nil {
		m.relations = map[uuid.UUID][]repository.EntryRelationRef{}
	}
	m.relations[entryID] = append([]repository.EntryRelationRef(nil), refs...)
	return nil
}

// EntryReferencedBy and MediaReferencedBy back GET .../referenced-by
// (ADR-024). No content_service_test.go case exercises the reverse-lookup
// service methods themselves — those live in the repository's integration
// tests, against the real SQL — so these fakes only need to satisfy the
// interface and answer the empty-result shape honestly.
func (m *memRepo) EntryReferencedBy(_ context.Context, _ string, targetID uuid.UUID, limit, offset int) (repository.ReferencedByResult, error) {
	var rows []repository.ReferencedByRow
	for entryID, refs := range m.relations {
		for _, ref := range refs {
			if ref.TargetID == targetID {
				rows = append(rows, m.referencedByRowFor(entryID, ref.FieldPath, true, false))
			}
		}
	}
	for entryID, refs := range m.publishedRelations {
		for _, ref := range refs {
			if ref.TargetID != targetID {
				continue
			}
			merged := false
			for i, r := range rows {
				if r.EntryID == entryID && r.FieldPath == ref.FieldPath {
					rows[i].Published = true
					merged = true
					break
				}
			}
			if !merged {
				rows = append(rows, m.referencedByRowFor(entryID, ref.FieldPath, false, true))
			}
		}
	}
	return paginateReferencedBy(rows, limit, offset), nil
}

// assetReferenced mirrors the Postgres orphan filter's dual NOT EXISTS: an
// asset is referenced when EITHER the working-copy links or the published
// snapshot links name it. ListMediaAssets(Orphan) and the sweep share it so
// the fake and the database agree on what "orphan" means.
func (m *memRepo) assetReferenced(assetID uuid.UUID) bool {
	for _, ids := range m.links {
		for _, id := range ids {
			if id == assetID {
				return true
			}
		}
	}
	for _, ids := range m.publishedLinks {
		for _, id := range ids {
			if id == assetID {
				return true
			}
		}
	}
	return false
}

func (m *memRepo) MediaReferencedBy(_ context.Context, tenantID string, assetID uuid.UUID, limit, offset int) (repository.ReferencedByResult, error) {
	var rows []repository.ReferencedByRow
	for entryID, ids := range m.links {
		for _, id := range ids {
			if id == assetID {
				rows = append(rows, m.referencedByRowFor(entryID, "", true, false))
			}
		}
	}
	for entryID, ids := range m.publishedLinks {
		for _, id := range ids {
			if id != assetID {
				continue
			}
			merged := false
			for i, r := range rows {
				if r.EntryID == entryID {
					rows[i].Published = true
					merged = true
					break
				}
			}
			if !merged {
				rows = append(rows, m.referencedByRowFor(entryID, "", false, true))
			}
		}
	}
	return paginateReferencedBy(rows, limit, offset), nil
}

// referencedByRowFor looks up the referrer's type name for the row, mirroring
// the real query's join to content_types — the title itself is left blank
// here, since no test asserts on it and domain.TitleFor needs the referrer's
// own field list.
func (m *memRepo) referencedByRowFor(entryID uuid.UUID, fieldPath string, draft, published bool) repository.ReferencedByRow {
	row := repository.ReferencedByRow{EntryID: entryID, FieldPath: fieldPath, Draft: draft, Published: published}
	for _, e := range m.entries {
		if e.ID != entryID {
			continue
		}
		row.ContentTypeID = e.ContentTypeID
		for _, t := range m.types {
			if t.ID == e.ContentTypeID {
				row.TypeName = t.Name
				break
			}
		}
		break
	}
	return row
}

func paginateReferencedBy(rows []repository.ReferencedByRow, limit, offset int) repository.ReferencedByResult {
	total := len(rows)
	if offset > len(rows) {
		offset = len(rows)
	}
	rows = rows[offset:]
	if limit > 0 && limit < len(rows) {
		rows = rows[:limit]
	}
	return repository.ReferencedByResult{Items: rows, Total: total}
}

// AssetIsPublished mirrors the SQL join: referenced by the SNAPSHOT of at least
// one published entry. It reads publishedLinks, not links — using the working
// copy is exactly the bug this mirrors away from.
func (m *memRepo) AssetIsPublished(_ context.Context, tenantID string, assetID uuid.UUID) (bool, error) {
	for entryID, ids := range m.publishedLinks {
		for _, id := range ids {
			if id != assetID {
				continue
			}
			for _, e := range m.entries {
				if e.ID == entryID && e.TenantID == tenantID && e.Status == domain.StatusPublished {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

var _ repository.ContentRepository = (*memRepo)(nil)

// staleReadRepo replays a pinned (stale) snapshot from GetEntry while the
// underlying store advances — simulating the read-modify-write race the
// repo-level version guard exists to catch, without needing real concurrency.
type staleReadRepo struct {
	*memRepo
	pinned *domain.Entry
}

func (s *staleReadRepo) GetEntry(ctx context.Context, tenantID string, ctID, id uuid.UUID) (*domain.Entry, error) {
	if s.pinned != nil && s.pinned.ID == id {
		cp := *s.pinned
		return &cp, nil
	}
	return s.memRepo.GetEntry(ctx, tenantID, ctID, id)
}

var _ repository.ContentRepository = (*staleReadRepo)(nil)

// --- helpers ----------------------------------------------------------------

// staticPlan resolves every tenant to the same quota (test convenience).
func staticPlan(q Quota) PlanResolver {
	return PlanFunc(func(context.Context, string) (Quota, error) { return q, nil })
}

func newSvc() (ContentService, *memRepo) {
	repo := &memRepo{}
	return NewContentService(repo, authz.NewAllowAllAuthorizer(), staticPlan(Quota{})), repo
}

func newSvcWithQuota(q Quota) (ContentService, *memRepo) {
	repo := &memRepo{}
	return NewContentService(repo, authz.NewAllowAllAuthorizer(), staticPlan(q)), repo
}

// mustTotal reads the admin audience's total. It fails rather than defaulting,
// because the pointer being nil means "this response paged by cursor" — a real
// difference a test must not paper over with a zero.
func mustTotal(t *testing.T, res *EntryListResult) int {
	t.Helper()
	if res.Total == nil {
		t.Fatal("total is absent — this is a cursor-paged response, not an offset-paged one")
	}
	return *res.Total
}

func ctxTenant(tenant string) context.Context {
	return authn.WithSubject(context.Background(), authn.Subject{
		UserID:   uuid.New(),
		TenantID: tenant,
		Roles:    []string{"member"},
	})
}

func orderTypeInput() CreateTypeInput {
	return CreateTypeInput{
		Name:  "order",
		Label: "Order",
		Fields: []FieldInput{
			{Key: "title", Type: domain.FieldTypeString, Required: true},
			{Key: "amount", Type: domain.FieldTypeNumber},
			{Key: "state", Type: domain.FieldTypeEnum, EnumValues: []string{"new", "paid"}},
		},
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// --- validatePayload (pure, table-driven) -----------------------------------

func TestValidatePayload(t *testing.T) {
	fields := []domain.Field{
		{Key: "title", Type: domain.FieldTypeString, Required: true},
		{Key: "amount", Type: domain.FieldTypeNumber},
		{Key: "active", Type: domain.FieldTypeBoolean},
		{Key: "state", Type: domain.FieldTypeEnum, EnumValues: []string{"new", "paid"}},
		{Key: "due", Type: domain.FieldTypeDate},
		{Key: "ref", Type: domain.FieldTypeRelation, RelationEntity: "Member"},
	}
	cases := []struct {
		name    string
		payload map[string]any
		code    string // "" = expect ok
	}{
		{"valid full", map[string]any{"title": "a", "amount": 9.5, "active": true, "state": "paid", "due": "2026-01-02", "ref": uuid.NewString()}, ""},
		{"valid minimal", map[string]any{"title": "a"}, ""},
		{"missing required", map[string]any{"amount": 1.0}, "CONTENT_FIELD_REQUIRED"},
		{"number mismatch", map[string]any{"title": "a", "amount": "NaN"}, "CONTENT_FIELD_TYPE_MISMATCH"},
		{"boolean mismatch", map[string]any{"title": "a", "active": "yes"}, "CONTENT_FIELD_TYPE_MISMATCH"},
		{"enum invalid", map[string]any{"title": "a", "state": "x"}, "CONTENT_FIELD_ENUM_INVALID"},
		{"enum not string", map[string]any{"title": "a", "state": 3.0}, "CONTENT_FIELD_TYPE_MISMATCH"},
		{"date invalid", map[string]any{"title": "a", "due": "not-a-date"}, "CONTENT_FIELD_TYPE_MISMATCH"},
		{"relation not string", map[string]any{"title": "a", "ref": 5.0}, "CONTENT_FIELD_TYPE_MISMATCH"},
		{"unknown key", map[string]any{"title": "a", "bogus": 1}, "CONTENT_FIELD_UNKNOWN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePayload(fields, tc.payload)
			if tc.code == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			ae, ok := apperrors.As(err)
			require.True(t, ok)
			assert.Equal(t, tc.code, ae.Code)
		})
	}
}

// --- tenant injection -------------------------------------------------------

func TestCreateEntry_InjectsSubjectTenant(t *testing.T) {
	svc, repo := newSvc()
	ctx := ctxTenant("tenant-a")

	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)

	dto, err := svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{"title": "T", "amount": 10.0, "state": "paid"}))
	require.NoError(t, err)

	require.Len(t, repo.entries, 1)
	assert.Equal(t, "tenant-a", repo.entries[0].TenantID, "tenant must come from the subject, not the request")
	assert.Equal(t, dto.ID, repo.entries[0].ID)
}

func TestCreateEntry_RejectsBadValues(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)

	_, err = svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{"amount": "NaN", "state": "x"}))
	require.Error(t, err)
	ae, ok := apperrors.As(err)
	require.True(t, ok)
	// title is required and listed first, so that's the first violation reported.
	assert.Equal(t, "CONTENT_FIELD_REQUIRED", ae.Code)
}

// --- PATCH semantics + optimistic locking -----------------------------------

func TestUpdateEntry_PartialMergePreservesUnsentFields(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)
	created, err := svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{"title": "T1", "amount": 10.0, "state": "new"}))
	require.NoError(t, err)

	// PATCH only `amount`. A full overwrite would drop the required `title` and
	// fail validation; a partial merge preserves title/state.
	updated, err := svc.UpdateEntry(ctx, "order", created.ID, mustJSON(t, map[string]any{"amount": 20.0}), 0)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(updated.Data, &got))
	assert.Equal(t, "T1", got["title"])
	assert.Equal(t, "new", got["state"])
	assert.Equal(t, 20.0, got["amount"])
}

func TestUpdateEntry_OptimisticLock(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)
	created, err := svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{"title": "T", "state": "new"}))
	require.NoError(t, err)
	require.Equal(t, 1, created.Version)

	conflict := func(err error) bool {
		ae, ok := apperrors.As(err)
		return ok && ae.Code == "CONTENT_VERSION_CONFLICT"
	}

	// A stale If-Match precondition is rejected as a conflict.
	_, err = svc.UpdateEntry(ctx, "order", created.ID, mustJSON(t, map[string]any{"amount": 5.0}), 99)
	require.Error(t, err)
	assert.True(t, conflict(err), "stale expected version must 409")

	// The correct version succeeds and bumps to 2.
	up1, err := svc.UpdateEntry(ctx, "order", created.ID, mustJSON(t, map[string]any{"amount": 5.0}), 1)
	require.NoError(t, err)
	assert.Equal(t, 2, up1.Version)

	// Reusing the now-stale version 1 conflicts — the lost update is prevented.
	_, err = svc.UpdateEntry(ctx, "order", created.ID, mustJSON(t, map[string]any{"amount": 6.0}), 1)
	require.Error(t, err)
	assert.True(t, conflict(err), "reused stale version must 409")
}

func TestUpdateEntry_GuardRejectsStaleReadWithoutIfMatch(t *testing.T) {
	// Even with no If-Match (expectedVersion=0), a write built on a stale read
	// must not silently clobber a newer save: the repo guards on the read version.
	base := &memRepo{}
	repo := &staleReadRepo{memRepo: base}
	svc := NewContentService(repo, authz.NewAllowAllAuthorizer(), staticPlan(Quota{}))
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)
	created, err := svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{"title": "T", "state": "new"}))
	require.NoError(t, err)

	// Snapshot the fresh v1 as a slow reader would hold it.
	v1 := *base.entries[0]
	// A concurrent writer bumps the stored version to 2.
	_, err = svc.UpdateEntry(ctx, "order", created.ID, mustJSON(t, map[string]any{"amount": 1.0}), 0)
	require.NoError(t, err)

	// Force the service to read the stale v1 and write with no If-Match.
	repo.pinned = &v1
	_, err = svc.UpdateEntry(ctx, "order", created.ID, mustJSON(t, map[string]any{"amount": 2.0}), 0)
	require.Error(t, err)
	ae, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, "CONTENT_VERSION_CONFLICT", ae.Code)
}

// --- cross-tenant isolation -------------------------------------------------

func TestCrossTenantIsolation(t *testing.T) {
	svc, _ := newSvc()
	ctxA := ctxTenant("tenant-a")
	ctxB := ctxTenant("tenant-b")

	// Both tenants define the same type; an entry is created only under A.
	_, err := svc.CreateContentType(ctxA, orderTypeInput())
	require.NoError(t, err)
	_, err = svc.CreateContentType(ctxB, orderTypeInput())
	require.NoError(t, err)

	created, err := svc.CreateEntry(ctxA, "order", mustJSON(t, map[string]any{"title": "secret", "amount": 1.0}))
	require.NoError(t, err)

	// Tenant B cannot read A's entry by id -> 404 (no existence leak).
	_, err = svc.GetEntry(ctxB, "order", created.ID, GetEntryInput{})
	require.Error(t, err)
	assert.True(t, apperrors.Is(err, apperrors.ErrNotFound))

	// Tenant B sees none of A's entries.
	listB, err := svc.ListEntries(ctxB, "order", ListEntriesInput{})
	require.NoError(t, err)
	assert.Equal(t, 0, mustTotal(t, listB))

	// Tenant A sees its own.
	listA, err := svc.ListEntries(ctxA, "order", ListEntriesInput{})
	require.NoError(t, err)
	assert.Equal(t, 1, mustTotal(t, listA))
}

// --- runtime-dynamic: add a field with no redeploy --------------------------

func TestAddField_GrowsSchemaAtRuntime(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)

	dto, err := svc.AddField(ctx, "order", FieldInput{Key: "note", Type: domain.FieldTypeText})
	require.NoError(t, err)
	require.Len(t, dto.Fields, 4)

	// The new field is immediately usable by entry writes.
	_, err = svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{"title": "T", "note": "hello"}))
	require.NoError(t, err)
}

// --- authz ------------------------------------------------------------------

func TestUnauthorized_NoSubject(t *testing.T) {
	svc, _ := newSvc()
	_, err := svc.ListContentTypes(context.Background())
	require.Error(t, err)
	ae, ok := apperrors.As(err)
	require.True(t, ok)
	assert.Equal(t, apperrors.ErrUnauthorized.Code, ae.Code)
}

// --- TKT-R4a: per-tenant content quota backstop -----------------------------

func quotaExceeded(err error) bool {
	ae, ok := apperrors.As(err)
	return ok && ae.Code == "CONTENT_QUOTA_EXCEEDED" && ae.HTTPStatus == 429
}

func TestCreateContentType_QuotaBackstop(t *testing.T) {
	svc, _ := newSvcWithQuota(Quota{MaxTypesPerTenant: 2})
	ctx := ctxTenant("tenant-a")

	// Two types allowed (distinct names).
	_, err := svc.CreateContentType(ctx, CreateTypeInput{Name: "one", Fields: orderTypeInput().Fields})
	require.NoError(t, err)
	_, err = svc.CreateContentType(ctx, CreateTypeInput{Name: "two", Fields: orderTypeInput().Fields})
	require.NoError(t, err)

	// Third trips the backstop.
	_, err = svc.CreateContentType(ctx, CreateTypeInput{Name: "three", Fields: orderTypeInput().Fields})
	require.Error(t, err)
	assert.True(t, quotaExceeded(err), "got %v", err)

	// Another tenant is unaffected — the cap is per-tenant.
	_, err = svc.CreateContentType(ctxTenant("tenant-b"), CreateTypeInput{Name: "one", Fields: orderTypeInput().Fields})
	require.NoError(t, err)
}

func TestCreateEntry_QuotaBackstop(t *testing.T) {
	svc, _ := newSvcWithQuota(Quota{MaxEntriesPerTenant: 1})
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)

	_, err = svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{"title": "a", "state": "new"}))
	require.NoError(t, err)

	// Second entry (tenant-wide count, regardless of type) is blocked.
	_, err = svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{"title": "b", "state": "new"}))
	require.Error(t, err)
	assert.True(t, quotaExceeded(err), "got %v", err)

	// A different tenant still writes.
	ctxB := ctxTenant("tenant-b")
	_, err = svc.CreateContentType(ctxB, orderTypeInput())
	require.NoError(t, err)
	_, err = svc.CreateEntry(ctxB, "order", mustJSON(t, map[string]any{"title": "c", "state": "new"}))
	require.NoError(t, err)
}

func TestQuota_ZeroMeansUnlimited(t *testing.T) {
	svc, _ := newSvcWithQuota(Quota{}) // both zero
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)
	for i := 0; i < 25; i++ {
		_, err := svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{"title": "x", "state": "new"}))
		require.NoError(t, err)
	}
}

func TestCreateContentType_FieldCapBackstop(t *testing.T) {
	svc, _ := newSvcWithQuota(Quota{MaxFieldsPerType: 2})
	ctx := ctxTenant("tenant-a")
	// Three fields at creation exceeds the per-type cap.
	_, err := svc.CreateContentType(ctx, CreateTypeInput{Name: "big", Fields: orderTypeInput().Fields})
	require.Error(t, err)
	assert.True(t, quotaExceeded(err), "got %v", err)

	// Two is allowed.
	_, err = svc.CreateContentType(ctx, CreateTypeInput{Name: "ok", Fields: []FieldInput{
		{Key: "a", Type: domain.FieldTypeString}, {Key: "b", Type: domain.FieldTypeString},
	}})
	require.NoError(t, err)
}

func TestAddField_FieldCapBackstop(t *testing.T) {
	svc, _ := newSvcWithQuota(Quota{MaxFieldsPerType: 3})
	ctx := ctxTenant("tenant-a")
	// order type has exactly 3 fields → at the cap.
	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)
	_, err = svc.AddField(ctx, "order", FieldInput{Key: "extra", Type: domain.FieldTypeString})
	require.Error(t, err)
	assert.True(t, quotaExceeded(err), "adding past the field cap must 429: %v", err)
}

func TestEntryBytesCap_CreateAndPatchGrowth(t *testing.T) {
	// Cap tuned just above a small stored payload; a PATCH that grows it trips.
	svc, _ := newSvcWithQuota(Quota{MaxEntryBytes: 40})
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)

	// A create whose normalized payload exceeds the cap is rejected outright.
	_, err = svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{
		"title": "a very long title that pushes the payload over the byte cap", "state": "new",
	}))
	require.Error(t, err)
	assert.True(t, quotaExceeded(err), "oversized create must 429: %v", err)

	// A small create succeeds, then a PATCH that grows the MERGED payload past
	// the cap is rejected — the cumulative-growth hole is closed.
	created, err := svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{"title": "x", "state": "new"}))
	require.NoError(t, err)
	_, err = svc.UpdateEntry(ctx, "order", created.ID, mustJSON(t, map[string]any{
		"title": "now a much much much longer title exceeding the cap",
	}), 0)
	require.Error(t, err)
	assert.True(t, quotaExceeded(err), "PATCH growth past cap must 429: %v", err)
}

// --- TKT-R4b: soft-threshold warnings + usage --------------------------------

func TestCreateEntry_SoftWarningCrossesThreshold(t *testing.T) {
	// Cap 5, soft 80% → warn when used reaches 4.
	svc, _ := newSvcWithQuota(Quota{MaxEntriesPerTenant: 5, SoftThresholdPct: 80})
	ctx := WithUsageWarnings(ctxTenant("tenant-a"))
	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		_, err := svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{"title": "x", "state": "new"}))
		require.NoError(t, err)
	}
	// After 3 entries: used=3 < 4, no warning yet.
	assert.Empty(t, UsageWarningsFrom(ctx))

	// 4th entry: used=4 = 80% of 5 → warning.
	_, err = svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{"title": "x", "state": "new"}))
	require.NoError(t, err)
	ws := UsageWarningsFrom(ctx)
	require.Len(t, ws, 1)
	assert.Equal(t, "entries=4/5", ws[0].String())
}

func TestSoftWarn_UnlimitedOrNoThresholdNeverWarns(t *testing.T) {
	ctx := WithUsageWarnings(context.Background())
	softWarn(ctx, "entries", 1000, 0, 80)   // unlimited
	softWarn(ctx, "entries", 1000, 1000, 0) // no soft pct
	assert.Empty(t, UsageWarningsFrom(ctx))
}

func TestUsage_ReportsPlanAndCounts(t *testing.T) {
	svc, _ := newSvcWithQuota(Quota{
		PlanName: "pro", MaxTypesPerTenant: 10, MaxEntriesPerTenant: 100, SoftThresholdPct: 80,
	})
	ctx := ctxTenant("tenant-a")
	_, err := svc.CreateContentType(ctx, orderTypeInput())
	require.NoError(t, err)
	_, err = svc.CreateEntry(ctx, "order", mustJSON(t, map[string]any{"title": "x", "state": "new"}))
	require.NoError(t, err)

	u, err := svc.Usage(ctx)
	require.NoError(t, err)
	assert.Equal(t, "pro", u.Plan)
	assert.Equal(t, 80, u.SoftThresholdPct)
	assert.Equal(t, 1, u.Types.Used)
	require.NotNil(t, u.Types.Limit)
	assert.Equal(t, 10, *u.Types.Limit)
	assert.Equal(t, 1, u.Entries.Used)
	assert.Equal(t, 100, *u.Entries.Limit)
}

func TestUsage_UnlimitedDimensionHasNilLimit(t *testing.T) {
	svc, _ := newSvcWithQuota(Quota{PlanName: "enterprise"}) // all zero = unlimited
	ctx := ctxTenant("tenant-a")
	u, err := svc.Usage(ctx)
	require.NoError(t, err)
	assert.Equal(t, "enterprise", u.Plan)
	assert.Nil(t, u.Types.Limit)
	assert.Nil(t, u.Entries.Limit)
}

// --- editorial status (draft / publish) --------------------------------------

// seedEntry creates a type + one entry and returns the entry.
func seedEntry(t *testing.T, svc ContentService, ctx context.Context) EntryDTO {
	t.Helper()
	if _, err := svc.CreateContentType(ctx, orderTypeInput()); err != nil {
		t.Fatalf("create type: %v", err)
	}
	e, err := svc.CreateEntry(ctx, "order", json.RawMessage(`{"title":"a"}`))
	if err != nil {
		t.Fatalf("create entry: %v", err)
	}
	return e
}

// A new entry must never be born published — otherwise a public delivery path
// would start serving content nobody chose to publish (ADR-004).
func TestCreateEntry_StartsAsDraft(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	e := seedEntry(t, svc, ctx)
	if e.Status != domain.StatusDraft {
		t.Fatalf("status=%q want %q", e.Status, domain.StatusDraft)
	}
	if e.PublishedAt != nil {
		t.Fatalf("published_at=%v want nil", e.PublishedAt)
	}
}

func TestSetEntryStatus_PublishThenUnpublish(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	e := seedEntry(t, svc, ctx)

	pub, err := svc.SetEntryStatus(ctx, "order", e.ID, domain.StatusPublished, 0)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if pub.Status != domain.StatusPublished {
		t.Fatalf("status=%q want published", pub.Status)
	}
	if pub.PublishedAt == nil {
		t.Fatal("published_at must be set when published")
	}

	un, err := svc.SetEntryStatus(ctx, "order", e.ID, domain.StatusDraft, 0)
	if err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if un.Status != domain.StatusDraft {
		t.Fatalf("status=%q want draft", un.Status)
	}
	// The DB CHECK enforces (status='published') = (published_at IS NOT NULL);
	// the service must clear the timestamp or the write would be rejected.
	if un.PublishedAt != nil {
		t.Fatalf("published_at=%v want nil after unpublish", un.PublishedAt)
	}
}

// Re-publishing must not rewrite published_at — a retried request should be a
// no-op, not a history edit.
func TestSetEntryStatus_RepublishIsIdempotent(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	e := seedEntry(t, svc, ctx)

	first, err := svc.SetEntryStatus(ctx, "order", e.ID, domain.StatusPublished, 0)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	second, err := svc.SetEntryStatus(ctx, "order", e.ID, domain.StatusPublished, 0)
	if err != nil {
		t.Fatalf("republish: %v", err)
	}
	if second.PublishedAt == nil || !second.PublishedAt.Equal(*first.PublishedAt) {
		t.Fatalf("published_at moved: %v -> %v", first.PublishedAt, second.PublishedAt)
	}
	if second.Version != first.Version {
		t.Fatalf("version moved on no-op republish: %d -> %d", first.Version, second.Version)
	}
}

func TestSetEntryStatus_RejectsUnknownStatus(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	e := seedEntry(t, svc, ctx)
	if _, err := svc.SetEntryStatus(ctx, "order", e.ID, "archived", 0); err == nil {
		t.Fatal("expected error for unknown status")
	}
}

// A publish carrying a stale If-Match must lose to the concurrent edit, exactly
// like a content update would.
func TestSetEntryStatus_VersionConflict(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	e := seedEntry(t, svc, ctx)
	if _, err := svc.UpdateEntry(ctx, "order", e.ID, json.RawMessage(`{"title":"b"}`), 0); err != nil {
		t.Fatalf("update: %v", err)
	}
	// e.Version is now stale (the update bumped it).
	_, err := svc.SetEntryStatus(ctx, "order", e.ID, domain.StatusPublished, e.Version)
	require.ErrorIs(t, err, repository.ErrVersionConflict)
}

// Listing with status=published must not leak drafts — this is the predicate a
// public delivery path will depend on.
func TestListEntries_StatusFilter(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	published := seedEntry(t, svc, ctx)
	if _, err := svc.CreateEntry(ctx, "order", json.RawMessage(`{"title":"draft-one"}`)); err != nil {
		t.Fatalf("create second: %v", err)
	}
	if _, err := svc.SetEntryStatus(ctx, "order", published.ID, domain.StatusPublished, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}

	all, err := svc.ListEntries(ctx, "order", ListEntriesInput{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if mustTotal(t, all) != 2 {
		t.Fatalf("total=%d want 2 (no status = all states)", mustTotal(t, all))
	}

	live, err := svc.ListEntries(ctx, "order", ListEntriesInput{Status: domain.StatusPublished})
	if err != nil {
		t.Fatalf("list published: %v", err)
	}
	if mustTotal(t, live) != 1 {
		t.Fatalf("total=%d want 1", mustTotal(t, live))
	}
	if live.Items[0].ID != published.ID {
		t.Fatalf("wrong entry returned: %s", live.Items[0].ID)
	}
}

func TestListEntries_RejectsUnknownStatus(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	seedEntry(t, svc, ctx)
	if _, err := svc.ListEntries(ctx, "order", ListEntriesInput{Status: "publised"}); err == nil {
		t.Fatal("expected error for unknown status, got nil (would silently list drafts)")
	}
}

// TestListEntries_QueryReachesTheRepository is ADR-021's `q` parameter,
// exercised at the per-type list rather than the cross-type Search endpoint.
// memRepo does not itself filter on Query — the same reason it leaves Filters
// unimplemented (see TestDelivery_FilterAndSortAreBoundToTheSnapshot's
// comment): the actual ILIKE-against-search_text predicate is proven against
// real Postgres in the repository integration suite. What this test pins is
// that the service forwards `q` to the repository intact rather than
// swallowing or mangling it.
func TestListEntries_QueryReachesTheRepository(t *testing.T) {
	svc, repo := newSvc()
	ctx := ctxTenant("t1")
	seedEntry(t, svc, ctx)

	repo.lastList = repository.ListEntriesFilter{}
	if _, err := svc.ListEntries(ctx, "order", ListEntriesInput{Query: "unicorn ranch"}); err != nil {
		t.Fatalf("list with q: %v", err)
	}
	if repo.lastList.Query != "unicorn ranch" {
		t.Fatalf("Query = %q, want %q — q did not reach the repository intact", repo.lastList.Query, "unicorn ranch")
	}
}

// TestListEntries_RejectsOverlongQuery is the 200-character cap on `q`
// (ADR-021): one request must not be able to force an unbounded number of
// ILIKE clauses onto the query.
func TestListEntries_RejectsOverlongQuery(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	seedEntry(t, svc, ctx)

	_, err := svc.ListEntries(ctx, "order", ListEntriesInput{Query: strings.Repeat("a", 201)})
	require.Error(t, err)
	assert.Equal(t, "CONTENT_SEARCH_QUERY_TOO_LONG", codeOf(t, err))
}

// --- public delivery credential (ADR-004 option A) ---------------------------

// ctxDelivery is a credential minted for the public delivery edge: scoped to a
// tenant, marked PublicDelivery. Deliberately given the "editor" role — the
// tests must prove the delivery constraint holds on its own, not because the
// role happened to be read-only.
func ctxDelivery(tenant string) context.Context {
	return authn.WithSubject(context.Background(), authn.Subject{
		UserID:         uuid.New(),
		TenantID:       tenant,
		TenantRole:     "editor",
		PublicDelivery: true,
	})
}

// The whole public surface rests on this: a delivery credential never sees a
// draft, however it asks. Asking for one is now REFUSED rather than silently
// rewritten to `published` — quietly answering a different question than the one
// asked lets the caller conclude the drafts do not exist, when the truth is it
// was never allowed to ask. Asking for `published`, which this audience CAN be
// given, is still honoured; that is the difference from filter/sort/offset,
// where no requested value is serviceable at all.
func TestDelivery_DraftStatusRefusedPublishedHonoured(t *testing.T) {
	svc, _ := newSvc()
	admin := ctxTenant("t1")
	live := seedEntry(t, svc, admin)
	if _, err := svc.CreateEntry(admin, "order", json.RawMessage(`{"title":"secret-draft"}`)); err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if _, err := svc.SetEntryStatus(admin, "order", live.ID, domain.StatusPublished, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}

	_, err := svc.ListEntries(ctxDelivery("t1"), "order", ListEntriesInput{Status: domain.StatusDraft})
	appErr, ok := err.(*apperrors.AppError)
	if !ok || appErr.Code != "CONTENT_STATUS_FORBIDDEN" {
		t.Fatalf("asking for drafts: got %v, want CONTENT_STATUS_FORBIDDEN", err)
	}
	if appErr.HTTPStatus != 403 {
		t.Fatalf("status = %d, want 403", appErr.HTTPStatus)
	}

	// Both the explicit `published` and the unspecified case still work, and
	// return exactly the published entry — never the draft.
	for _, name := range []string{domain.StatusPublished, ""} {
		res, err := svc.ListEntries(ctxDelivery("t1"), "order", ListEntriesInput{Status: name})
		if err != nil {
			t.Fatalf("status=%q must be accepted: %v", name, err)
		}
		if len(res.Items) != 1 || res.Items[0].ID != live.ID {
			t.Fatalf("status=%q: delivery saw %d entries (%+v) — must be exactly the published one", name, len(res.Items), res.Items)
		}
	}
}

// The property ADR-006 exists for: editing live content does not change what the
// public is served. Before the draft/published split there was one payload, so
// every save to a published entry went live immediately — publish was deliberate
// only for an entry's FIRST release.
func TestDelivery_EditsToPublishedEntryStayPrivateUntilRepublished(t *testing.T) {
	svc, _ := newSvc()
	admin := ctxTenant("t1")
	e := seedEntry(t, svc, admin)
	if _, err := svc.SetEntryStatus(admin, "order", e.ID, domain.StatusPublished, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if _, err := svc.UpdateEntry(admin, "order", e.ID, json.RawMessage(`{"title":"edited"}`), 0); err != nil {
		t.Fatalf("edit: %v", err)
	}

	got, err := svc.GetEntry(ctxDelivery("t1"), "order", e.ID, GetEntryInput{})
	if err != nil {
		t.Fatalf("delivery get: %v", err)
	}
	if string(got.Data) == `{"title":"edited"}` {
		t.Fatalf("delivery served the working copy: %s", got.Data)
	}

	// The editor, by contrast, sees their own edit and is told it is not live.
	adminView, err := svc.GetEntry(admin, "order", e.ID, GetEntryInput{})
	if err != nil {
		t.Fatalf("admin get: %v", err)
	}
	if !adminView.HasUnpublishedChanges {
		t.Fatalf("admin view must flag unpublished changes: %+v", adminView)
	}

	// Publishing again is what releases it — and must NOT be treated as a no-op
	// just because status is already 'published'.
	if _, err := svc.SetEntryStatus(admin, "order", e.ID, domain.StatusPublished, 0); err != nil {
		t.Fatalf("republish: %v", err)
	}
	got, err = svc.GetEntry(ctxDelivery("t1"), "order", e.ID, GetEntryInput{})
	if err != nil {
		t.Fatalf("delivery get after republish: %v", err)
	}
	if string(got.Data) != `{"title":"edited"}` {
		t.Fatalf("republish did not release the edit: %s", got.Data)
	}
	adminView, err = svc.GetEntry(admin, "order", e.ID, GetEntryInput{})
	if err != nil {
		t.Fatalf("admin get after republish: %v", err)
	}
	if adminView.HasUnpublishedChanges {
		t.Fatalf("nothing should be pending right after a publish: %+v", adminView)
	}
}

// OD2-023 F1 / ADR-006 Amendments 4 and 5. buildWhere and the ORDER BY run
// against `payload` (the working copy) unless told otherwise, so letting a
// public caller filter OR sort turns "did this row come back" — and "where in
// the page did it come back" — into an oracle on never-published text.
//
// Both parameters are now ACCEPTED for this audience and both are bound to the
// snapshot. The ordering half is the one that is easy to grant and forget: a
// filter that reads the working copy leaks by which rows appear, while a sort
// that reads the working copy leaks by their ORDER, and the rows themselves
// look perfectly correct either way.
//
// What this test can and cannot say: memRepo implements Sort but not Filters,
// so a claim that "secret-edit" was not MATCHED would pass no matter what the
// code did. The properties pinned here are the REQUEST the service issued — the
// predicate arrives with MatchPublished set and Status forced to published —
// and, for the sort, the ORDER the fake produces from the snapshot. That the
// repository evaluates both against published_payload in SQL is proven against
// real Postgres in repository/snapshot_filter_integration_test.go and
// repository/delivery_sort_integration_test.go.
func TestDelivery_FilterAndSortAreBoundToTheSnapshot(t *testing.T) {
	svc, repo := newSvc()
	admin := ctxTenant("t1")
	e := seedEntry(t, svc, admin)
	if _, err := svc.SetEntryStatus(admin, "order", e.ID, domain.StatusPublished, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// An edit that is saved but NOT published: the string a filter could probe.
	if _, err := svc.UpdateEntry(admin, "order", e.ID, json.RawMessage(`{"title":"secret-edit"}`), 0); err != nil {
		t.Fatalf("edit: %v", err)
	}

	t.Run("filter is accepted and reads published_payload", func(t *testing.T) {
		repo.lastList = repository.ListEntriesFilter{}
		if _, err := svc.ListEntries(ctxDelivery("t1"), "order", ListEntriesInput{
			Filters: []string{"title:eq:secret-edit"},
		}); err != nil {
			t.Fatalf("delivery filter must be accepted: %v", err)
		}
		got := repo.lastList
		if !got.MatchPublished {
			t.Fatal("MatchPublished is false — the predicate would probe the working copy")
		}
		if got.Status != domain.StatusPublished {
			t.Fatalf("Status = %q, want published — MatchPublished is only safe under the status override", got.Status)
		}
		if len(got.Filters) != 1 || got.Filters[0].Field.Key != "title" || got.Filters[0].Op != repository.OpEq || got.Filters[0].Value != "secret-edit" {
			t.Fatalf("filter did not reach the repository intact: %+v", got.Filters)
		}
	})

	t.Run("blank filter is not a filter", func(t *testing.T) {
		if _, err := svc.ListEntries(ctxDelivery("t1"), "order", ListEntriesInput{Filters: []string{"  "}}); err != nil {
			t.Fatalf("blank input must be accepted, got %v", err)
		}
	})

	t.Run("sort is accepted and ordered by the snapshot", func(t *testing.T) {
		repo.lastList = repository.ListEntriesFilter{}
		if _, err := svc.ListEntries(ctxDelivery("t1"), "order", ListEntriesInput{Sort: "title:asc"}); err != nil {
			t.Fatalf("delivery sort must be accepted: %v", err)
		}
		got := repo.lastList
		if got.Sort == nil || got.Sort.Field.Key != "title" || got.Sort.Desc {
			t.Fatalf("sort did not reach the repository intact: %+v", got.Sort)
		}
		if !got.MatchPublished {
			t.Fatal("MatchPublished is false — the ORDER BY would rank rows by the working copy")
		}
		if got.Status != domain.StatusPublished {
			t.Fatalf("Status = %q, want published — MatchPublished is only safe under the status override", got.Status)
		}
	})

	// The ordering itself, not just the request: a second published row whose
	// SNAPSHOT sorts after the first, while its WORKING copy sorts before it.
	// Read the working copy and the two rows swap, which is the leak — and the
	// page still looks entirely plausible.
	t.Run("the order follows the snapshot, not the unpublished edit", func(t *testing.T) {
		other, err := svc.CreateEntry(admin, "order", json.RawMessage(`{"title":"zzz-published"}`))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := svc.SetEntryStatus(admin, "order", other.ID, domain.StatusPublished, 0); err != nil {
			t.Fatalf("publish: %v", err)
		}
		// Saved, never published: as text it sorts FIRST, ahead of the other
		// row's published "public".
		if _, err := svc.UpdateEntry(admin, "order", other.ID, json.RawMessage(`{"title":"aaa-secret"}`), 0); err != nil {
			t.Fatalf("edit: %v", err)
		}
		res, err := svc.ListEntries(ctxDelivery("t1"), "order", ListEntriesInput{Sort: "title:asc"})
		if err != nil {
			t.Fatalf("delivery sort: %v", err)
		}
		if len(res.Items) != 2 {
			t.Fatalf("want both published rows, got %d", len(res.Items))
		}
		// Snapshot order is "public" < "zzz-published"; working-copy order is
		// "aaa-secret" < "secret-edit", which is the reverse.
		if res.Items[0].ID != e.ID || res.Items[1].ID != other.ID {
			t.Fatalf("page order followed the working copy: %s, %s", res.Items[0].ID, res.Items[1].ID)
		}
	})

	// The admin audience matches the WORKING copy, as every admin list does:
	// MatchPublished must stay off there, or an editor searching for their own
	// unpublished edit would be told it does not exist.
	t.Run("admin filter reads the working copy", func(t *testing.T) {
		repo.lastList = repository.ListEntriesFilter{}
		if _, err := svc.ListEntries(admin, "order", ListEntriesInput{
			Filters: []string{"title:eq:secret-edit"},
			Sort:    "title:asc",
		}); err != nil {
			t.Fatalf("admin filter/sort must not be rejected: %v", err)
		}
		if repo.lastList.MatchPublished {
			t.Fatal("MatchPublished leaked into an admin list")
		}
	})
}

// OD2-023 F2. dto.go states that has_unpublished_changes is withheld from the
// public because "a public reader has no business learning that unreleased edits
// exist" — but version and updated_at tracked the working copy and both move on
// every save, so polling reconstructed the withheld fact, and its timing.
func TestDelivery_VersionAndUpdatedAtDescribeTheSnapshotNotTheWorkingCopy(t *testing.T) {
	svc, repo := newSvc()
	admin := ctxTenant("t1")
	e := seedEntry(t, svc, admin)
	if _, err := svc.SetEntryStatus(admin, "order", e.ID, domain.StatusPublished, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}
	before, err := svc.GetEntry(ctxDelivery("t1"), "order", e.ID, GetEntryInput{})
	if err != nil {
		t.Fatalf("delivery get: %v", err)
	}

	// Two saves the public must not be able to detect.
	for _, p := range []string{`{"title":"edit-1"}`, `{"title":"edit-2"}`} {
		if _, err := svc.UpdateEntry(admin, "order", e.ID, json.RawMessage(p), 0); err != nil {
			t.Fatalf("edit %s: %v", p, err)
		}
	}

	after, err := svc.GetEntry(ctxDelivery("t1"), "order", e.ID, GetEntryInput{})
	if err != nil {
		t.Fatalf("delivery get after edits: %v", err)
	}
	if string(after.Data) != string(before.Data) {
		t.Fatalf("guard: Data moved (%s -> %s); the rest of this test proves nothing", before.Data, after.Data)
	}
	if after.Version != before.Version {
		t.Fatalf("version moved %d -> %d while data held still — that is the leak", before.Version, after.Version)
	}
	// updated_at is not merely frozen for this audience — it is absent. A field
	// under that name that never moves while the content does misinforms more
	// than no field at all (published_at cannot serve as it: a re-publish does
	// not move published_at either).
	if before.UpdatedAt != nil || after.UpdatedAt != nil {
		t.Fatalf("delivery must not carry updated_at at all: before=%v after=%v", before.UpdatedAt, after.UpdatedAt)
	}

	// Not merely frozen — it must report the snapshot's own identity. The DTO
	// does not expose published_version to any audience, so this reads the
	// stored entry directly rather than inferring it.
	var stored *domain.Entry
	for _, it := range repo.entries {
		if it.ID == e.ID {
			stored = it
		}
	}
	if stored == nil {
		t.Fatal("entry vanished from the fake repo")
	}
	adminView, err := svc.GetEntry(admin, "order", e.ID, GetEntryInput{})
	if err != nil {
		t.Fatalf("admin get: %v", err)
	}
	if after.Version != stored.PublishedVersion {
		t.Fatalf("delivery version = %d, want published_version %d", after.Version, stored.PublishedVersion)
	}
	if adminView.Version == after.Version {
		t.Fatalf("guard: working copy and snapshot versions coincide (%d) — the assertion is vacuous", after.Version)
	}
	// The admin still gets one, and it did move — otherwise "delivery has none"
	// would be proving nothing about a field nobody sets anyway.
	if adminView.UpdatedAt == nil {
		t.Fatal("the admin audience must still report updated_at")
	}
	if !adminView.UpdatedAt.After(stored.PublishedAt.Add(-time.Nanosecond)) {
		t.Fatalf("guard: admin updated_at (%s) did not advance past publish (%s) — the edits above did not register",
			adminView.UpdatedAt, stored.PublishedAt)
	}
}

// --- delivery keyset pagination ---------------------------------------------

// seedPublished creates n published entries and returns them newest-first, which
// is the order delivery pages in. Entries are seeded directly on the repo so
// created_at can be controlled: sharing a timestamp across rows is the case the
// id tiebreaker exists for, and the service cannot produce it on demand.
func seedPublished(t *testing.T, svc ContentService, repo *memRepo, admin context.Context, times []time.Time) []*domain.Entry {
	t.Helper()
	seedEntry(t, svc, admin) // creates the content type
	ct := repo.types[0]
	var out []*domain.Entry
	for i, ts := range times {
		payload := json.RawMessage(`{"title":"e` + string(rune('A'+i)) + `"}`)
		e := &domain.Entry{
			ID:               uuid.New(),
			TenantID:         "t1",
			ContentTypeID:    ct.ID,
			Payload:          payload,
			PublishedPayload: payload,
			Version:          1,
			PublishedVersion: 1,
			Status:           domain.StatusPublished,
			PublishedAt:      &ts,
			Locale:           domain.DefaultLocale,
			CreatedAt:        ts,
			UpdatedAt:        ts,
		}
		repo.entries = append(repo.entries, e)
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	return out
}

// Paging the whole set through the cursor must visit every row exactly once.
// This is the property offset paging silently loses on a non-unique sort key,
// and the reason the order carries an id tiebreaker.
func TestDelivery_CursorPagesEveryRowExactlyOnce(t *testing.T) {
	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		times []time.Time
	}{
		{"distinct created_at", []time.Time{
			base, base.Add(-time.Minute), base.Add(-2 * time.Minute),
			base.Add(-3 * time.Minute), base.Add(-4 * time.Minute),
		}},
		// The case a created_at-only cursor gets wrong: a seeding run writes
		// every row inside the same microsecond.
		{"identical created_at", []time.Time{base, base, base, base, base}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo := newSvc()
			want := seedPublished(t, svc, repo, ctxTenant("t1"), tc.times)

			var got []uuid.UUID
			cursor := ""
			for page := 0; ; page++ {
				if page > 10 {
					t.Fatal("did not terminate — next_cursor is not advancing")
				}
				res, err := svc.ListEntries(ctxDelivery("t1"), "order", ListEntriesInput{Limit: 2, Cursor: cursor})
				if err != nil {
					t.Fatalf("page %d: %v", page, err)
				}
				if res.Total != nil || res.Offset != nil {
					t.Fatalf("cursor response must not carry total/offset: %+v", res)
				}
				if res.HasMore == nil {
					t.Fatal("cursor response must state has_more explicitly, including false")
				}
				for _, it := range res.Items {
					got = append(got, it.ID)
				}
				if !*res.HasMore {
					if res.NextCursor != "" {
						t.Fatalf("last page must not hand out a next cursor: %q", res.NextCursor)
					}
					break
				}
				if res.NextCursor == "" {
					t.Fatal("has_more is true but no next cursor — the caller cannot continue")
				}
				cursor = res.NextCursor
			}

			if len(got) != len(want) {
				t.Fatalf("visited %d rows, want %d (%v)", len(got), len(want), got)
			}
			seen := map[uuid.UUID]bool{}
			for i, id := range got {
				if seen[id] {
					t.Fatalf("row %s visited twice", id)
				}
				seen[id] = true
				if id != want[i].ID {
					t.Fatalf("position %d: got %s want %s — page order is not the total order", i, id, want[i].ID)
				}
			}
		})
	}
}

// seedPublishedPayloads seeds published entries whose working copy and snapshot
// are the same document, one per payload, all sharing a created_at. The shared
// timestamp is deliberate: it makes the created_at order useless as a
// tiebreaker, so a page order that happens to be right can only be right
// because the sort key and the id tiebreaker did the work.
func seedPublishedPayloads(t *testing.T, svc ContentService, repo *memRepo, admin context.Context, payloads []string) []*domain.Entry {
	t.Helper()
	seedEntry(t, svc, admin) // creates the content type
	ct := repo.types[0]
	ts := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	before := len(repo.entries)
	for _, p := range payloads {
		e := &domain.Entry{
			ID:               uuid.New(),
			TenantID:         "t1",
			ContentTypeID:    ct.ID,
			Payload:          json.RawMessage(p),
			PublishedPayload: json.RawMessage(p),
			Version:          1,
			PublishedVersion: 1,
			Status:           domain.StatusPublished,
			PublishedAt:      &ts,
			Locale:           domain.DefaultLocale,
			CreatedAt:        ts,
			UpdatedAt:        ts,
		}
		repo.entries = append(repo.entries, e)
	}
	return repo.entries[before:]
}

// pageThrough walks the whole collection by cursor and returns the ids in the
// order they were visited, failing the test if the loop does not terminate.
func pageThrough(t *testing.T, svc ContentService, in ListEntriesInput, limit, guard int) []uuid.UUID {
	t.Helper()
	var got []uuid.UUID
	in.Limit = limit
	in.Cursor = ""
	for page := 0; ; page++ {
		if page > guard {
			t.Fatal("did not terminate — next_cursor is not advancing")
		}
		res, err := svc.ListEntries(ctxDelivery("t1"), "order", in)
		if err != nil {
			t.Fatalf("page %d (sort=%q): %v", page, in.Sort, err)
		}
		for _, it := range res.Items {
			got = append(got, it.ID)
		}
		if res.HasMore == nil {
			t.Fatal("cursor response must state has_more explicitly")
		}
		if !*res.HasMore {
			if res.NextCursor != "" {
				t.Fatalf("last page must not hand out a next cursor: %q", res.NextCursor)
			}
			return got
		}
		if res.NextCursor == "" {
			t.Fatal("has_more is true but no next cursor — the caller cannot continue")
		}
		in.Cursor = res.NextCursor
	}
}

// ADR-006 Amendment 5. A SORTED delivery page-through must visit every row
// exactly once, in exactly the order a single unpaged request would have
// produced.
//
// The dataset is built out of the two cases a keyset over a payload key gets
// wrong, and gets wrong SILENTLY:
//
//   - TIES. Several rows share one sort value, so the value alone cannot say
//     where a page ended. Without the id tiebreaker in both the ORDER BY and the
//     window, the rows inside a tie that straddles a page boundary are skipped
//     or repeated — and a tie is the normal case for an enum, a status, a price.
//   - MISSING VALUES. `amount` is optional, so some rows have no value at all.
//     They are NULL in SQL, and NULL is not a value a `<` or `>` window can step
//     past: the naive predicate evaluates to NULL, which is not true, so the
//     page-through stops dead at the first null row and reports the collection
//     as finished. Both directions, because Postgres's own NULLS default flips
//     with the direction and the fix must not.
//
// The oracle is the unpaged request rather than an expected list written out by
// hand: an expected list is a second implementation of the ordering rules, and
// when the two disagree the test says nothing about which one is wrong. "Paging
// preserves the order" is the property that actually matters, and the ordering
// rules themselves are asserted separately below.
func TestDelivery_SortedCursorPagesEveryRowExactlyOnce(t *testing.T) {
	payloads := []string{
		`{"title":"b","amount":10}`,
		`{"title":"a","amount":2}`,
		`{"title":"c","amount":10}`, // ties with the first on amount
		`{"title":"a"}`,             // no amount at all — the NULL block
		`{"title":"d","amount":2}`,  // ties with the second
		`{"title":"a"}`,             // ties on title AND has no amount
		`{"title":"e","amount":9}`,
	}
	for _, sortSpec := range []string{"amount:asc", "amount:desc", "title:asc", "title:desc"} {
		t.Run(sortSpec, func(t *testing.T) {
			svc, repo := newSvc()
			rows := seedPublishedPayloads(t, svc, repo, ctxTenant("t1"), payloads)

			// The order one unpaged request produces, and the ordering rules it
			// must satisfy.
			whole, err := svc.ListEntries(ctxDelivery("t1"), "order", ListEntriesInput{Sort: sortSpec, Limit: 100})
			if err != nil {
				t.Fatalf("unpaged: %v", err)
			}
			if len(whole.Items) != len(rows) {
				t.Fatalf("unpaged returned %d rows, want %d", len(whole.Items), len(rows))
			}
			assertSortedPage(t, whole.Items, sortSpec)

			var want []uuid.UUID
			for _, it := range whole.Items {
				want = append(want, it.ID)
			}
			got := pageThrough(t, svc, ListEntriesInput{Sort: sortSpec}, 2, len(rows)+2)

			if len(got) != len(want) {
				t.Fatalf("paged over %d rows, want %d", len(got), len(want))
			}
			seen := map[uuid.UUID]bool{}
			for i, id := range got {
				if seen[id] {
					t.Fatalf("row %s came back on two pages", id)
				}
				seen[id] = true
				if id != want[i] {
					t.Fatalf("position %d: got %s want %s — paging did not preserve the unpaged order", i, id, want[i])
				}
			}
		})
	}
}

// assertSortedPage checks the ordering rules themselves: monotonic in the sort
// key, rows with NO value forming a suffix in BOTH directions, and ties broken
// by id in the sort's direction.
func assertSortedPage(t *testing.T, items []EntryDTO, sortSpec string) {
	t.Helper()
	parts := strings.SplitN(sortSpec, ":", 2)
	key, desc := parts[0], parts[1] == "desc"
	valueOf := func(it EntryDTO) (float64, string, bool) {
		var doc map[string]any
		if err := json.Unmarshal(it.Data, &doc); err != nil {
			t.Fatalf("payload: %v", err)
		}
		switch v := doc[key].(type) {
		case nil:
			return 0, "", false
		case float64:
			return v, "", true
		case string:
			return 0, v, true
		default:
			t.Fatalf("unexpected %T for %q", v, key)
			return 0, "", false
		}
	}
	nullSeen := false
	for i, it := range items {
		n, s, ok := valueOf(it)
		if !ok {
			nullSeen = true
			continue
		}
		if nullSeen {
			t.Fatalf("position %d has a value but a row without one came earlier — missing values must sort LAST in both directions", i)
		}
		if i == 0 {
			continue
		}
		pn, ps, pok := valueOf(items[i-1])
		if !pok {
			t.Fatalf("position %d: a valued row follows a null row", i)
		}
		var cmp int
		switch {
		case pn != n:
			cmp = -1
			if pn > n {
				cmp = 1
			}
		case ps != s:
			cmp = -1
			if ps > s {
				cmp = 1
			}
		}
		if desc && cmp < 0 || !desc && cmp > 0 {
			t.Fatalf("position %d breaks %s: %v/%q then %v/%q", i, sortSpec, pn, ps, n, s)
		}
		if cmp == 0 && !idTieOK(items[i-1].ID, it.ID, desc) {
			t.Fatalf("position %d: tie on %q not broken by id in the %s direction", i, key, parts[1])
		}
	}
}

func idTieOK(prev, cur uuid.UUID, desc bool) bool {
	if desc {
		return prev.String() > cur.String()
	}
	return prev.String() < cur.String()
}

// A cursor is a position in ONE order. Resuming it inside a different order
// would have the database compare a title against a price: it answers, and the
// answer silently skips most of the collection. There is no correct
// translation, so the pair is refused (ADR-006 Amendment 5).
func TestDelivery_CursorAndSortMustAgree(t *testing.T) {
	svc, repo := newSvc()
	payloads := []string{
		`{"title":"a","amount":1}`, `{"title":"b","amount":2}`,
		`{"title":"c","amount":3}`, `{"title":"d","amount":4}`,
	}
	seedPublishedPayloads(t, svc, repo, ctxTenant("t1"), payloads)

	firstCursor := func(t *testing.T, sortSpec string) string {
		t.Helper()
		res, err := svc.ListEntries(ctxDelivery("t1"), "order", ListEntriesInput{Sort: sortSpec, Limit: 2})
		if err != nil {
			t.Fatalf("first page (sort=%q): %v", sortSpec, err)
		}
		if res.NextCursor == "" {
			t.Fatalf("no next cursor to test with (sort=%q)", sortSpec)
		}
		return res.NextCursor
	}
	sortedCursor := firstCursor(t, "amount:asc")
	plainCursor := firstCursor(t, "")

	for _, tc := range []struct {
		name   string
		cursor string
		sort   string
	}{
		{"different key", sortedCursor, "title:asc"},
		{"different direction", sortedCursor, "amount:desc"},
		{"sorted cursor, no sort", sortedCursor, ""},
		{"plain cursor, sorted request", plainCursor, "amount:asc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.ListEntries(ctxDelivery("t1"), "order",
				ListEntriesInput{Cursor: tc.cursor, Sort: tc.sort, Limit: 2})
			if err == nil {
				t.Fatal("a cursor from another order was accepted")
			}
			appErr, ok := err.(*apperrors.AppError)
			if !ok || appErr.Code != "CONTENT_CURSOR_INVALID" || appErr.HTTPStatus != 400 {
				t.Fatalf("got %v, want CONTENT_CURSOR_INVALID/400", err)
			}
			// The details have to name BOTH sides, or the caller cannot tell a
			// mismatch from a corrupt token and will retry the same request.
			if appErr.Details["reason"] == nil {
				t.Fatalf("no reason in details: %v", appErr.Details)
			}
			if _, ok := appErr.Details["cursor_sort"]; !ok {
				t.Fatalf("details must name the cursor's sort: %v", appErr.Details)
			}
			if _, ok := appErr.Details["request_sort"]; !ok {
				t.Fatalf("details must name the requested sort: %v", appErr.Details)
			}
		})
	}

	// The control: the matching pair still pages.
	if _, err := svc.ListEntries(ctxDelivery("t1"), "order",
		ListEntriesInput{Cursor: sortedCursor, Sort: "amount:asc", Limit: 2}); err != nil {
		t.Fatalf("the cursor's own sort was refused: %v", err)
	}
}

// Backward compatibility, asserted against a HAND-BUILT token rather than one
// this code minted: a round-trip through encodeCursor would pass even if the
// shape had changed on both sides at once, which is exactly the deploy that
// strands a consumer mid-page-through.
//
// The `{"t","i"}` shape is what every cursor was before Amendment 5, and an
// unsorted request must still accept it — and must still MINT it, so a consumer
// that stored one is not handed something its own retry logic cannot round-trip.
func TestDelivery_PreAmendmentCursorStillPages(t *testing.T) {
	svc, repo := newSvc()
	rows := seedPublishedPayloads(t, svc, repo, ctxTenant("t1"), []string{
		`{"title":"a"}`, `{"title":"b"}`, `{"title":"c"}`, `{"title":"d"}`,
	})
	// Newest-first over a shared created_at is id DESC, so the largest id is
	// page 1 and a cursor naming it resumes at the second row.
	ordered := append([]*domain.Entry{}, rows...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID.String() > ordered[j].ID.String() })

	legacy := base64.RawURLEncoding.EncodeToString([]byte(
		`{"t":"` + ordered[0].CreatedAt.UTC().Format(time.RFC3339Nano) + `","i":"` + ordered[0].ID.String() + `"}`))
	res, err := svc.ListEntries(ctxDelivery("t1"), "order", ListEntriesInput{Cursor: legacy, Limit: 10})
	if err != nil {
		t.Fatalf("a cursor minted before Amendment 5 was refused: %v", err)
	}
	if len(res.Items) != len(ordered)-1 {
		t.Fatalf("resumed with %d rows, want %d", len(res.Items), len(ordered)-1)
	}
	for i, it := range res.Items {
		if it.ID != ordered[i+1].ID {
			t.Fatalf("position %d: got %s want %s", i, it.ID, ordered[i+1].ID)
		}
	}

	// And the shape it hands out for an unsorted page is still that one — no
	// k/d/v, which is what makes the paragraph above true in the other
	// direction.
	page, err := svc.ListEntries(ctxDelivery("t1"), "order", ListEntriesInput{Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(page.NextCursor)
	if err != nil {
		t.Fatalf("next cursor is not base64url: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("next cursor is not a JSON object: %s", raw)
	}
	if len(fields) != 2 || fields["t"] == nil || fields["i"] == nil {
		t.Fatalf("unsorted cursor shape changed: %s", raw)
	}

	// A SORTED page mints the other shape, and it carries the sort it was made
	// under — that is what decodeCursor compares against.
	sorted, err := svc.ListEntries(ctxDelivery("t1"), "order", ListEntriesInput{Sort: "title:asc", Limit: 2})
	if err != nil {
		t.Fatalf("sorted first page: %v", err)
	}
	raw, err = base64.RawURLEncoding.DecodeString(sorted.NextCursor)
	if err != nil {
		t.Fatalf("sorted cursor is not base64url: %v", err)
	}
	var sc struct {
		K string  `json:"k"`
		D string  `json:"d"`
		V *string `json:"v"`
		I string  `json:"i"`
	}
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatalf("sorted cursor is not a JSON object: %s", raw)
	}
	if sc.K != "title" || sc.D != "asc" || sc.I == "" {
		t.Fatalf("sorted cursor does not name its order: %s", raw)
	}
	if sc.V == nil || *sc.V != "b" {
		t.Fatalf("sorted cursor value = %v, want the second row's title: %s", sc.V, raw)
	}
}

// A row with no value for the sort key is a POSITION, not the absence of one:
// the cursor has to be able to name "somewhere inside the trailing null block",
// or the page-through stops at the first such row and calls the collection
// finished. `"v": null` is that name, and it must survive the round trip.
func TestDelivery_CursorResumesInsideTheNullBlock(t *testing.T) {
	svc, repo := newSvc()
	// Three rows without `amount` and one with, so a page boundary is forced
	// INSIDE the null block whichever way it is ordered.
	rows := seedPublishedPayloads(t, svc, repo, ctxTenant("t1"), []string{
		`{"title":"a","amount":5}`, `{"title":"b"}`, `{"title":"c"}`, `{"title":"d"}`,
	})
	for _, dir := range []string{"asc", "desc"} {
		t.Run(dir, func(t *testing.T) {
			got := pageThrough(t, svc, ListEntriesInput{Sort: "amount:" + dir}, 2, len(rows)+2)
			if len(got) != len(rows) {
				t.Fatalf("visited %d rows, want %d — the window did not step through the nulls", len(got), len(rows))
			}
			seen := map[uuid.UUID]bool{}
			for _, id := range got {
				if seen[id] {
					t.Fatalf("row %s came back twice", id)
				}
				seen[id] = true
			}
			if got[0] != rows[0].ID {
				t.Fatalf("the only valued row must come first in both directions, got %s", got[0])
			}
		})
	}
}

// A cursor is a round-trip token, not a caller-authored one.
func TestDelivery_RejectsTamperedCursor(t *testing.T) {
	svc, _ := newSvc()
	seedEntry(t, svc, ctxTenant("t1"))
	for _, bad := range []string{"not-base64!!", "YWJj" /* base64 "abc", not JSON */, ""} {
		if bad == "" {
			continue // empty means "first page", tested above
		}
		_, err := svc.ListEntries(ctxDelivery("t1"), "order", ListEntriesInput{Cursor: bad})
		if err == nil {
			t.Fatalf("tampered cursor %q was accepted", bad)
		}
		appErr, ok := err.(*apperrors.AppError)
		if !ok || appErr.Code != "CONTENT_CURSOR_INVALID" {
			t.Fatalf("cursor %q: got %v, want CONTENT_CURSOR_INVALID", bad, err)
		}
	}
}

// Each audience refuses the other's paging parameter rather than ignoring it.
// Silently dropping either one lets a caller believe it paged when it re-read
// page 1 — and the delivery audience has no `total` to notice that against.
func TestPaging_AudiencesRefuseEachOthersParameter(t *testing.T) {
	svc, _ := newSvc()
	admin := ctxTenant("t1")
	seedEntry(t, svc, admin)

	_, err := svc.ListEntries(ctxDelivery("t1"), "order", ListEntriesInput{Offset: 10})
	appErr, ok := err.(*apperrors.AppError)
	if !ok || appErr.Code != "CONTENT_OFFSET_FORBIDDEN" {
		t.Fatalf("delivery offset: got %v, want CONTENT_OFFSET_FORBIDDEN", err)
	}

	_, err = svc.ListEntries(admin, "order", ListEntriesInput{Cursor: "anything"})
	appErr, ok = err.(*apperrors.AppError)
	if !ok || appErr.Code != "CONTENT_CURSOR_UNSUPPORTED" {
		t.Fatalf("admin cursor: got %v, want CONTENT_CURSOR_UNSUPPORTED", err)
	}

	// Admin offset paging is untouched by any of this.
	res, err := svc.ListEntries(admin, "order", ListEntriesInput{Offset: 0})
	if err != nil {
		t.Fatalf("admin offset paging must still work: %v", err)
	}
	if res.Total == nil || res.Offset == nil {
		t.Fatalf("admin response must still carry total/offset: %+v", res)
	}
}

// The fake's comparison must be semantic, or the service tests above prove
// nothing about the real thing. Everything written THROUGH the service is
// canonicalised by validateAndNormalize, so byte equality and jsonb equality
// happen to coincide on that path — which is why this seeds the repo directly.
// Rows written by a migration or by hand carry no such guarantee, and the
// database compares them semantically either way.
func TestMemRepo_MirrorsJSONBSemantics(t *testing.T) {
	cases := []struct {
		name              string
		payload, snapshot string
	}{
		// Verified against Postgres 16: '{"b":2,"a": 1}'::jsonb reads back as
		// '{"a": 1, "b": 2}' — key order and whitespace are normalised away.
		{"key order and whitespace", `{"b":2,"a": 1}`, `{"a": 1, "b": 2}`},
		// Verified against Postgres 16: jsonb-equal, but the TEXT differs, so a
		// byte comparison would call this an edit. The database does not.
		{"numeric form", `{"n":1.0}`, `{"n":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &domain.Entry{
				Status:           domain.StatusPublished,
				Version:          7,
				PublishedVersion: 4,
				Payload:          json.RawMessage(tc.payload),
				PublishedPayload: json.RawMessage(tc.snapshot),
			}
			if string(e.Payload) == string(e.PublishedPayload) {
				t.Fatal("guard: the two payloads must differ as bytes, or this proves nothing")
			}
			setUnpublishedChanges(e)
			if e.HasUnpublishedChanges {
				t.Fatalf("same content written two ways is not an edit: %s vs %s", tc.payload, tc.snapshot)
			}
		})
	}
}

// version bumps on EVERY write, including one that stores what the row already
// held — so a version-only criterion told editors they had unreleased edits when
// they had none. The payload comparison is the deciding test (ADR-006).
func TestAdmin_SavingIdenticalContentIsNotAnUnpublishedChange(t *testing.T) {
	svc, _ := newSvc()
	admin := ctxTenant("t1")
	e := seedEntry(t, svc, admin)
	if _, err := svc.SetEntryStatus(admin, "order", e.ID, domain.StatusPublished, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}
	live, err := svc.GetEntry(admin, "order", e.ID, GetEntryInput{})
	if err != nil {
		t.Fatalf("admin get: %v", err)
	}

	// Save exactly what is already live.
	if _, err := svc.UpdateEntry(admin, "order", e.ID, live.Data, 0); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	after, err := svc.GetEntry(admin, "order", e.ID, GetEntryInput{})
	if err != nil {
		t.Fatalf("admin get after re-save: %v", err)
	}
	if after.Version == live.Version {
		t.Fatalf("guard: the write must bump version (%d), or this test proves nothing", after.Version)
	}
	if after.HasUnpublishedChanges {
		t.Fatalf("nothing was changed, so nothing is pending: %+v", after)
	}

	// And with nothing pending, publish really is a no-op — no version bump, no
	// new snapshot. Under the old criterion this would have released again.
	republished, err := svc.SetEntryStatus(admin, "order", e.ID, domain.StatusPublished, 0)
	if err != nil {
		t.Fatalf("republish: %v", err)
	}
	if republished.Version != after.Version {
		t.Fatalf("publish with nothing pending must be a no-op: version %d -> %d", after.Version, republished.Version)
	}
}

// The list path must swap payloads too — a leak here would be just as public as
// one through GetEntry, and it is the path delivery actually uses in bulk.
func TestDelivery_ListServesSnapshotNotWorkingCopy(t *testing.T) {
	svc, _ := newSvc()
	admin := ctxTenant("t1")
	e := seedEntry(t, svc, admin)
	if _, err := svc.SetEntryStatus(admin, "order", e.ID, domain.StatusPublished, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := svc.UpdateEntry(admin, "order", e.ID, json.RawMessage(`{"title":"unreleased"}`), 0); err != nil {
		t.Fatalf("edit: %v", err)
	}

	res, err := svc.ListEntries(ctxDelivery("t1"), "order", ListEntriesInput{})
	if err != nil {
		t.Fatalf("delivery list: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("items=%d want 1", len(res.Items))
	}
	if string(res.Items[0].Data) == `{"title":"unreleased"}` {
		t.Fatalf("delivery list served the working copy: %s", res.Items[0].Data)
	}
	if res.Items[0].HasUnpublishedChanges {
		t.Fatalf("delivery must not learn that unreleased edits exist: %+v", res.Items[0])
	}
}

// TestDelivery_UnpublishKeepsTheSnapshotAndStillHidesIt is 驗證計畫第 12 條 at
// the service layer, and it takes BOTH assertions because either one alone
// passes for the wrong reason: prove only that delivery cannot see it and
// nothing pins §5.1's retained snapshot; prove only that the snapshot survives
// and it may already be leaking.
//
// It replaces TestDelivery_UnpublishClearsTheSnapshot, which asserted the
// pre-§5.1 rule. That test kept passing after §5.1 shipped because memRepo went
// on nulling the column the SQL had stopped nulling — so the stale fake and the
// stale test agreed with each other, and the integration suite that disagreed
// with both of them was in another package.
//
// Liveness is read from STATUS, never from snapshot-nullness, which is the whole
// direction §5.1 moves in: the two used to be the same condition and are not any
// more.
func TestDelivery_UnpublishKeepsTheSnapshotAndStillHidesIt(t *testing.T) {
	svc, repo := newSvc()
	admin := ctxTenant("t1")
	e := seedEntry(t, svc, admin)
	if _, err := svc.SetEntryStatus(admin, "order", e.ID, domain.StatusPublished, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := svc.SetEntryStatus(admin, "order", e.ID, domain.StatusDraft, 0); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	var found bool
	for _, it := range repo.entries {
		if it.ID != e.ID {
			continue
		}
		found = true
		if len(it.PublishedPayload) == 0 {
			t.Fatalf("ADR-014 §5.1: the snapshot must outlive a retract so the entry can go back up unchanged")
		}
		if it.Status != domain.StatusDraft {
			t.Fatalf("status is what says it is offline, got %q", it.Status)
		}
	}
	if !found {
		t.Fatal("entry vanished from the repository")
	}
	if _, err := svc.GetEntry(ctxDelivery("t1"), "order", e.ID, GetEntryInput{}); err == nil {
		t.Fatalf("delivery must not resolve an unpublished entry even though its snapshot is still stored")
	}
}

// No status asked for at all must also mean published-only, not "all states".
func TestDelivery_ListDefaultsToPublishedOnly(t *testing.T) {
	svc, _ := newSvc()
	admin := ctxTenant("t1")
	seedEntry(t, svc, admin) // stays draft

	res, err := svc.ListEntries(ctxDelivery("t1"), "order", ListEntriesInput{})
	if err != nil {
		t.Fatalf("delivery list: %v", err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("items=%d want 0 — an unpublished tenant must expose nothing", len(res.Items))
	}
}

// Fetching an unpublished entry by id must 404, not 403 — otherwise the
// endpoint is an existence oracle for unpublished ids.
func TestDelivery_GetDraftIsNotFound(t *testing.T) {
	svc, _ := newSvc()
	admin := ctxTenant("t1")
	draft := seedEntry(t, svc, admin)

	_, err := svc.GetEntry(ctxDelivery("t1"), "order", draft.ID, GetEntryInput{})
	require.ErrorIs(t, err, apperrors.ErrNotFound)
}

func TestDelivery_GetPublishedSucceeds(t *testing.T) {
	svc, _ := newSvc()
	admin := ctxTenant("t1")
	e := seedEntry(t, svc, admin)
	if _, err := svc.SetEntryStatus(admin, "order", e.ID, domain.StatusPublished, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}

	got, err := svc.GetEntry(ctxDelivery("t1"), "order", e.ID, GetEntryInput{})
	if err != nil {
		t.Fatalf("delivery get: %v", err)
	}
	if got.ID != e.ID || got.Status != domain.StatusPublished {
		t.Fatalf("got %+v", got)
	}
}

// Every write verb must be refused for a delivery credential regardless of the
// role it carries — the credential, not the role, is the constraint.
func TestDelivery_AllWritesRefused(t *testing.T) {
	svc, _ := newSvc()
	admin := ctxTenant("t1")
	e := seedEntry(t, svc, admin)
	del := ctxDelivery("t1")

	t.Run("create entry", func(t *testing.T) {
		_, err := svc.CreateEntry(del, "order", json.RawMessage(`{"title":"x"}`))
		require.ErrorIs(t, err, apperrors.ErrForbidden)
	})
	t.Run("update entry", func(t *testing.T) {
		_, err := svc.UpdateEntry(del, "order", e.ID, json.RawMessage(`{"title":"x"}`), 0)
		require.ErrorIs(t, err, apperrors.ErrForbidden)
	})
	t.Run("delete entry", func(t *testing.T) {
		require.ErrorIs(t, svc.DeleteEntry(del, "order", e.ID), apperrors.ErrForbidden)
	})
	t.Run("publish", func(t *testing.T) {
		_, err := svc.SetEntryStatus(del, "order", e.ID, domain.StatusPublished, 0)
		require.ErrorIs(t, err, apperrors.ErrForbidden)
	})
	t.Run("create content type", func(t *testing.T) {
		_, err := svc.CreateContentType(del, CreateTypeInput{Name: "leak"})
		require.ErrorIs(t, err, apperrors.ErrForbidden)
	})
	t.Run("add field", func(t *testing.T) {
		_, err := svc.AddField(del, "order", FieldInput{Key: "k", Type: domain.FieldTypeString})
		require.ErrorIs(t, err, apperrors.ErrForbidden)
	})

	// Schema mutation, all six. These are not covered by the entry verbs above:
	// they route through authorize() with ActionContentUpdate and
	// ActionContentSchemaWrite, and isReadAction lists the read verbs explicitly
	// so a new verb fails closed — this is what proves the new ones landed on the
	// closed side of that list rather than being forgotten.
	t.Run("update content type", func(t *testing.T) {
		_, err := svc.UpdateContentType(del, "order", UpdateTypeInput{Label: "x"})
		require.ErrorIs(t, err, apperrors.ErrForbidden)
	})
	t.Run("rename content type", func(t *testing.T) {
		_, err := svc.RenameContentType(del, "order", RenameInput{Name: "leak"})
		require.ErrorIs(t, err, apperrors.ErrForbidden)
	})
	t.Run("delete content type", func(t *testing.T) {
		require.ErrorIs(t, svc.DeleteContentType(del, "order"), apperrors.ErrForbidden)
	})
	t.Run("update field", func(t *testing.T) {
		_, err := svc.UpdateField(del, "order", "title", UpdateFieldInput{Label: strPtr("x")})
		require.ErrorIs(t, err, apperrors.ErrForbidden)
	})
	t.Run("rename field", func(t *testing.T) {
		_, err := svc.RenameField(del, "order", "title", RenameInput{Key: "headline"})
		require.ErrorIs(t, err, apperrors.ErrForbidden)
	})
	t.Run("delete field", func(t *testing.T) {
		// force=true too: consent from a credential that may not write at all is
		// still not consent.
		for _, force := range []bool{false, true} {
			_, err := svc.DeleteField(del, "order", "amount", force)
			require.ErrorIs(t, err, apperrors.ErrForbidden)
		}
	})
}

// strPtr keeps the delivery table above readable; the generic ptrTo lives in
// schema_mutation_test.go alongside the tests that need it for three types.
func strPtr(s string) *string { return &s }

// Tenancy still comes from the credential, never from the caller: a delivery
// token for t1 cannot read t2's published content.
func TestDelivery_CannotCrossTenant(t *testing.T) {
	svc, _ := newSvc()
	other := ctxTenant("t2")
	e := seedEntry(t, svc, other)
	if _, err := svc.SetEntryStatus(other, "order", e.ID, domain.StatusPublished, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// t1's delivery credential: t1 has no "order" type at all.
	if _, err := svc.ListEntries(ctxDelivery("t1"), "order", ListEntriesInput{}); err == nil {
		t.Fatal("expected error — t1 has no such type; must not fall through to t2")
	}
	_, err := svc.GetEntry(ctxDelivery("t1"), "order", e.ID, GetEntryInput{})
	require.Error(t, err, "delivery token for t1 must not read t2's entry")
}

// --- locale / translations ---------------------------------------------------

// The reason this model was chosen over a locale-keyed payload: each language
// publishes on its own schedule.
func TestLocale_TranslationsPublishIndependently(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	en := seedEntry(t, svc, ctx)

	zh, err := svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload: json.RawMessage(`{"title":"嗨"}`), Locale: "zh-TW", TranslationOf: &en.ID,
	})
	require.NoError(t, err)
	require.Equal(t, "zh-TW", zh.Locale)
	require.Equal(t, en.TranslationGroupID, zh.TranslationGroupID, "a translation joins the source's group")

	// Publish only the English one.
	if _, err := svc.SetEntryStatus(ctx, "order", en.ID, domain.StatusPublished, 0); err != nil {
		t.Fatalf("publish en: %v", err)
	}

	live, err := svc.ListEntries(ctx, "order", ListEntriesInput{Status: domain.StatusPublished})
	require.NoError(t, err)
	require.Equal(t, 1, mustTotal(t, live), "the untranslated sibling must stay unpublished")
	require.Equal(t, en.ID, live.Items[0].ID)
}

func TestLocale_DefaultsToDefaultLocaleAndOwnGroup(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	e := seedEntry(t, svc, ctx)
	require.Equal(t, domain.DefaultLocale, e.Locale)
	require.NotEqual(t, uuid.Nil, e.TranslationGroupID, "a standalone entry is a group of one")
}

// One row per locale per group — otherwise "the English version" is ambiguous.
func TestLocale_DuplicateTranslationRejected(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	en := seedEntry(t, svc, ctx)

	_, err := svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload: json.RawMessage(`{"title":"a"}`), Locale: "fr", TranslationOf: &en.ID,
	})
	require.NoError(t, err)

	_, err = svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload: json.RawMessage(`{"title":"b"}`), Locale: "fr", TranslationOf: &en.ID,
	})
	require.ErrorIs(t, err, repository.ErrTranslationExists)
}

func TestLocale_ListFiltersByLocale(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	en := seedEntry(t, svc, ctx)
	_, err := svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload: json.RawMessage(`{"title":"x"}`), Locale: "ja", TranslationOf: &en.ID,
	})
	require.NoError(t, err)

	all, err := svc.ListEntries(ctx, "order", ListEntriesInput{})
	require.NoError(t, err)
	require.Equal(t, 2, mustTotal(t, all), "no locale = every language")

	ja, err := svc.ListEntries(ctx, "order", ListEntriesInput{Locale: "ja"})
	require.NoError(t, err)
	require.Equal(t, 1, mustTotal(t, ja))
	require.Equal(t, "ja", ja.Items[0].Locale)
}

func TestLocale_RejectsInvalidTag(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	seedEntry(t, svc, ctx)

	_, err := svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload: json.RawMessage(`{"title":"a"}`), Locale: "zh TW",
	})
	require.Error(t, err, "a locale with whitespace must be rejected before SQL")

	_, err = svc.ListEntries(ctx, "order", ListEntriesInput{Locale: "../etc"})
	require.Error(t, err)
}

func TestLocale_ListTranslationsReturnsWholeGroup(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	en := seedEntry(t, svc, ctx)
	for _, loc := range []string{"zh-TW", "ja"} {
		_, err := svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
			Payload: json.RawMessage(`{"title":"x"}`), Locale: loc, TranslationOf: &en.ID,
		})
		require.NoError(t, err)
	}
	items, err := svc.ListTranslations(ctx, "order", en.ID)
	require.NoError(t, err)
	require.Len(t, items, 3, "the group is the source plus its translations")
}

// A delivery credential must not see unpublished siblings through the group
// view either — otherwise it would be a way around the published-only rule.
func TestLocale_DeliverySeesOnlyPublishedTranslations(t *testing.T) {
	svc, _ := newSvc()
	ctx := ctxTenant("t1")
	en := seedEntry(t, svc, ctx)
	_, err := svc.CreateLocalizedEntry(ctx, "order", CreateLocalizedInput{
		Payload: json.RawMessage(`{"title":"draft-zh"}`), Locale: "zh-TW", TranslationOf: &en.ID,
	})
	require.NoError(t, err)
	if _, err := svc.SetEntryStatus(ctx, "order", en.ID, domain.StatusPublished, 0); err != nil {
		t.Fatalf("publish: %v", err)
	}

	items, err := svc.ListTranslations(ctxDelivery("t1"), "order", en.ID)
	require.NoError(t, err)
	require.Len(t, items, 1, "the unpublished translation must not leak through the group view")
	require.Equal(t, domain.DefaultLocale, items[0].Locale)
}

// A translation cannot be grafted onto another tenant's entry.
func TestLocale_TranslationOfForeignEntryRejected(t *testing.T) {
	svc, _ := newSvc()
	other := seedEntry(t, svc, ctxTenant("t2"))

	if _, err := svc.CreateContentType(ctxTenant("t1"), orderTypeInput()); err != nil {
		t.Fatalf("create type: %v", err)
	}
	_, err := svc.CreateLocalizedEntry(ctxTenant("t1"), "order", CreateLocalizedInput{
		Payload: json.RawMessage(`{"title":"a"}`), Locale: "en", TranslationOf: &other.ID,
	})
	require.Error(t, err, "translation_of must not resolve across tenants")
}
