package repository

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
)

// ADR-009's DATA layer on the two CROSS-TYPE reads — the release queue (ADR-014
// §2) and the activity stream (§3).
//
// WHY THESE TWO NEED A TEST OF THEIR OWN. Every other entry read names one
// content type, so the service loads it and calls guardTypeRead / guardOwned
// before touching the database, and type_permission_integration_test.go covers
// that path. These two name NO type — that is why they are separate queries —
// so there was nothing for a service-level guard to hold, and the data layer was
// simply missing from them: an editor off a type's read_roles read that type's
// rows out of both, titles included, and a confined editor saw colleagues'
// drafts. The fix is a WHERE clause in each, which means memRepo cannot see it
// and this file is the only place the rule is actually executed.
//
// The fixtures deliberately cover BOTH POLARITIES in one tenant, because the
// bug this guards against is reading one list with the other's meaning:
// read_roles empty means EVERYONE may read, own_only_roles empty means NOBODY is
// confined. A fixture with only one restricted type would pass under either
// reading of the other.
func TestCrossTypeReadsApplyDataPermission(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION") == "1" || !dockerUp() {
		t.Skip("integration test requires Docker")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("crosstype"),
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
	require.NoError(t, err)
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()

	_, err = pool.Exec(ctx, loadContentRLSMigrations(t))
	require.NoError(t, err, "migrate")

	repo := NewPostgresContentRepository(pool, nil)

	// The two people. editorID is the caller under test throughout; ownerID is
	// the control that must keep seeing everything, so that a predicate which
	// simply hides too much cannot pass.
	editorID, ownerID := uuid.New(), uuid.New()

	// THREE TYPES, one per rule:
	//   open   — no declaration at all: the baseline every caller sees.
	//   secret — read_roles ["owner"]: the type-level refusal.
	//   mine   — own_only_roles ["editor"]: confinement, which hides ROWS and not
	//            the type, so an editor still sees their own.
	openType, secretType, mineType := uuid.New(), uuid.New(), uuid.New()
	for _, tc := range []struct {
		id                 uuid.UUID
		name               string
		readRoles, ownOnly []string
	}{
		{openType, "open", nil, nil},
		{secretType, "secret", []string{"owner"}, nil},
		{mineType, "mine", nil, []string{"editor"}},
	} {
		_, err := pool.Exec(ctx, `
			INSERT INTO content_types (id, tenant_id, name, label, read_roles, own_only_roles)
			VALUES ($1,'t1',$2,'',$3,$4)`,
			tc.id, tc.name, orEmptyRoles(tc.readRoles), orEmptyRoles(tc.ownOnly))
		require.NoError(t, err)
	}

	insertEntry := func(typeID uuid.UUID, title string, createdBy any) uuid.UUID {
		t.Helper()
		id := uuid.New()
		_, err := pool.Exec(ctx, `
			INSERT INTO entries (id, tenant_id, content_type_id, payload, status, created_by)
			VALUES ($1,'t1',$2,$3,$4,$5)`,
			id, typeID, `{"title":"`+title+`"}`, domain.StatusDraft, createdBy)
		require.NoError(t, err)
		return id
	}

	openEntry := insertEntry(openType, "open", ownerID)
	secretEntry := insertEntry(secretType, "secret", ownerID)
	mineOwn := insertEntry(mineType, "mine-own", editorID)
	mineOther := insertEntry(mineType, "mine-other", ownerID)
	// A row predating the provenance columns: created_by NULL. Confinement
	// resolves with `=`, never IS NOT DISTINCT FROM, so it matches nobody. That
	// is fail-closed and it is the direction the rest of the stack already took
	// (buildWhere's confinement clause says so in as many words) — asserted here
	// because "matches nobody" is exactly the kind of edge a rewrite would flip
	// to "matches everybody" without any other test noticing.
	mineOrphan := insertEntry(mineType, "mine-orphan", nil)

	t.Run("queue: an editor sees the open type, not the restricted one, and only their own where confined", func(t *testing.T) {
		rows, err := repo.ListPendingReview(ctx, PendingReviewFilter{
			TenantID: "t1", ViewerRole: "editor", ViewerUserID: editorID,
		})
		require.NoError(t, err)
		got := idSet(rows)

		assert.True(t, got[openEntry], "an unrestricted type is visible")
		assert.False(t, got[secretEntry], "read_roles ['owner'] hides the type from an editor")
		assert.True(t, got[mineOwn], "confinement hides rows, not the type: the editor's own row stays")
		assert.False(t, got[mineOther], "a colleague's row under own_only is not the editor's to see")
		assert.False(t, got[mineOrphan], "a row with no recorded author matches nobody when confined")
	})

	t.Run("queue: an owner sees every row, so the predicate is not merely hiding things", func(t *testing.T) {
		rows, err := repo.ListPendingReview(ctx, PendingReviewFilter{
			TenantID: "t1", ViewerRole: "owner", ViewerUserID: ownerID,
		})
		require.NoError(t, err)
		got := idSet(rows)

		for _, id := range []uuid.UUID{openEntry, secretEntry, mineOwn, mineOther, mineOrphan} {
			assert.True(t, got[id], "owner reads every type and is confined by none")
		}
	})

	// --- the activity stream --------------------------------------------------
	//
	// Its data layer joins by NAME (the table holds no content_type_id) and its
	// confinement half resolves against the ACTOR, not the entry's author — see
	// ListActivity for why the entry's author is unavailable to it. Both
	// departures need their own assertions; neither is visible from the queue's.
	insertActivity := func(targetType, title string, actor uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.New()
		entryID := uuid.New()
		var titleTarget any
		if title != "" {
			titleTarget = entryID
		}
		_, err := pool.Exec(ctx, `
			INSERT INTO content_activity
			  (id, tenant_id, actor_kind, actor_user_id, action, target_type, target_entry_id, target_title, outcome)
			VALUES ($1,'t1','human',$2,'content.update',$3,$4,$5,'success')`,
			id, actor, targetType, titleTarget, title)
		require.NoError(t, err)
		return id
	}

	openAct := insertActivity("open", "open title", ownerID)
	secretAct := insertActivity("secret", "secret title", ownerID)
	mineActOwn := insertActivity("mine", "mine own", editorID)
	mineActOther := insertActivity("mine", "mine other", ownerID)
	// An action concerning no single type — a schema verb, a collection read.
	// target_type '' matches no content_types row, so it must survive both halves
	// or the stream loses everything that is not an entry edit.
	typelessAct := insertActivity("", "", ownerID)
	// A name that resolves to nothing: the type was renamed after the row was
	// written, and this table is append-only so the old name stays forever.
	// Visible BY DESIGN — see ListActivity for why fail-closed here would delete
	// history from the audit record for every reader on the first rename.
	renamedAct := insertActivity("gone", "renamed away", ownerID)

	t.Run("stream: read_roles hides a type's rows, confinement narrows to the caller's own actions", func(t *testing.T) {
		rows, err := repo.ListActivity(ctx, ActivityFilter{
			TenantID: "t1", ViewerRole: "editor", ViewerUserID: editorID,
		})
		require.NoError(t, err)
		got := activityIDSet(rows)

		assert.True(t, got[openAct], "an unrestricted type is visible")
		assert.False(t, got[secretAct], "read_roles ['owner'] hides the type's rows, target_title with them")
		assert.True(t, got[mineActOwn], "under confinement the editor keeps their OWN actions")
		assert.False(t, got[mineActOther], "and not a colleague's, which is what confinement means")
		assert.True(t, got[typelessAct], "an action naming no type is not an entry read and stays")
		assert.True(t, got[renamedAct], "a name that no longer resolves fails OPEN, deliberately")
	})

	t.Run("stream: an owner sees every row", func(t *testing.T) {
		rows, err := repo.ListActivity(ctx, ActivityFilter{
			TenantID: "t1", ViewerRole: "owner", ViewerUserID: ownerID,
		})
		require.NoError(t, err)
		got := activityIDSet(rows)

		for _, id := range []uuid.UUID{openAct, secretAct, mineActOwn, mineActOther, typelessAct, renamedAct} {
			assert.True(t, got[id], "owner reads every type and is confined by none")
		}
	})

	// An UNDESCRIBED caller — the zero ViewerRole a future caller of the
	// repository could pass by forgetting the field. It must see LESS, not more:
	// an empty role is on no read_roles list. Asserted because the alternative
	// spelling (treating "" as unrestricted, which reads naturally if you match
	// the lists by shape) would make every forgotten field a silent full leak.
	t.Run("an empty viewer role is refused restricted types, not granted them", func(t *testing.T) {
		rows, err := repo.ListPendingReview(ctx, PendingReviewFilter{TenantID: "t1"})
		require.NoError(t, err)
		got := idSet(rows)

		assert.True(t, got[openEntry], "a type with no declaration is open to anyone, including an unnamed role")
		assert.False(t, got[secretEntry], "a restricted type is not readable by a role nobody granted")
	})
}

func idSet(rows []*domain.Entry) map[uuid.UUID]bool {
	out := map[uuid.UUID]bool{}
	for _, e := range rows {
		out[e.ID] = true
	}
	return out
}

func activityIDSet(rows []*domain.Activity) map[uuid.UUID]bool {
	out := map[uuid.UUID]bool{}
	for _, a := range rows {
		out[a.ID] = true
	}
	return out
}
