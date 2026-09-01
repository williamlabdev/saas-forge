package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/williamlabdev/saas-forge/internal/cms/content/domain"
	"github.com/williamlabdev/saas-forge/internal/pkg/authn"
	"github.com/williamlabdev/saas-forge/internal/pkg/authz"
	apperrors "github.com/williamlabdev/saas-forge/internal/pkg/errors"
)

// verbSet allows exactly the actions it is handed and refuses everything else.
//
// Needed because the property under test cannot be expressed with the real RBAC
// authorizer: every role that grants content:update also grants
// content:schema:amend, so a role-based test can never produce a caller holding
// one without the other. That is not an argument that the split is untestable —
// it is the reason the split exists. A credential scoped to entry writing is
// exactly such a caller, and the first one will be an agent credential.
type verbSet map[string]bool

func (v verbSet) Allow(_ context.Context, in authz.Input) error {
	if v[in.Action] {
		return nil
	}
	return apperrors.ErrForbidden
}

// Splitting the additive schema verbs off content:update (ADR-013 §B) changes
// nothing for any person — owner, admin and editor hold both verbs before and
// after. So there is no behavioural mutation that turns this red, and without a
// test naming the property directly, the split would be indistinguishable from
// a no-op and could be quietly reverted.
//
// The property: holding entry writes on a type must not carry the power to
// reshape that type.
func TestEntryWriteGrantDoesNotCarrySchemaAmend(t *testing.T) {
	entryWriter := verbSet{
		ActionContentList:   true,
		ActionContentRead:   true,
		ActionContentCreate: true,
		ActionContentUpdate: true,
		// Deliberately absent: content:schema:amend, :write, :plan.
	}
	svc := NewContentService(&memRepo{}, entryWriter, staticPlan(Quota{}))
	ctx := authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(), TenantID: "t1", TenantRole: "editor",
	})

	_, err := svc.CreateContentType(ctx, CreateTypeInput{
		Name:   "post",
		Fields: []FieldInput{{Key: "title", Type: domain.FieldTypeString}},
	})
	require.NoError(t, err, "content:create must still create a type — otherwise the refusals below prove nothing")

	// The three that moved. Each must now be refused for a caller that holds
	// every content verb.
	_, err = svc.AddField(ctx, "post", FieldInput{Key: "body", Type: domain.FieldTypeString})
	require.ErrorIs(t, err, apperrors.ErrForbidden, "AddField must not ride on content:update")

	label := "Posts"
	_, err = svc.UpdateContentType(ctx, "post", UpdateTypeInput{Label: label})
	require.ErrorIs(t, err, apperrors.ErrForbidden, "UpdateContentType must not ride on content:update")

	_, err = svc.UpdateField(ctx, "post", "title", UpdateFieldInput{Label: &label})
	require.ErrorIs(t, err, apperrors.ErrForbidden, "UpdateField must not ride on content:update")

	// The positive control, and the half that makes the refusals mean something:
	// this caller really is an entry writer. Without it every assertion above
	// would also pass against an authorizer that refuses everything.
	_, err = svc.CreateEntry(ctx, "post", mustJSON(t, map[string]any{"title": "a"}))
	require.NoError(t, err, "content:create must still write an entry")

	list, err := svc.ListEntries(ctx, "post", ListEntriesInput{})
	require.NoError(t, err, "content:list must still list entries")
	require.Len(t, list.Items, 1)
}

// The mirror: granting the amend verb restores exactly the three methods above
// and nothing more. This pins that the split did not smuggle other endpoints
// along with it — a check the entry-writer test cannot make, since it asserts
// absence.
func TestSchemaAmendGrantRestoresTheThreeAndNotDestruction(t *testing.T) {
	amender := verbSet{
		ActionContentList:        true,
		ActionContentRead:        true,
		ActionContentCreate:      true,
		ActionContentUpdate:      true,
		ActionContentSchemaAmend: true,
		// Still absent: content:schema:write.
	}
	svc := NewContentService(&memRepo{}, amender, staticPlan(Quota{}))
	ctx := authn.WithSubject(context.Background(), authn.Subject{
		UserID: uuid.New(), TenantID: "t1", TenantRole: "editor",
	})

	_, err := svc.CreateContentType(ctx, CreateTypeInput{
		Name:   "post",
		Fields: []FieldInput{{Key: "title", Type: domain.FieldTypeString}},
	})
	require.NoError(t, err)

	_, err = svc.AddField(ctx, "post", FieldInput{Key: "body", Type: domain.FieldTypeString})
	require.NoError(t, err)

	label := "Posts"
	_, err = svc.UpdateContentType(ctx, "post", UpdateTypeInput{Label: label})
	require.NoError(t, err)

	_, err = svc.UpdateField(ctx, "post", "title", UpdateFieldInput{Label: &label})
	require.NoError(t, err)

	// Destruction stays behind the stricter verb, and so does editing a
	// permission list — the two things that must NOT come along with amend.
	_, err = svc.DeleteField(ctx, "post", "body", false)
	require.ErrorIs(t, err, apperrors.ErrForbidden, "amend must not carry field deletion")

	admins := []string{"admin"}
	_, err = svc.UpdateContentType(ctx, "post", UpdateTypeInput{Label: label, ReadRoles: &admins})
	require.ErrorIs(t, err, apperrors.ErrForbidden, "amend must not carry permission-list edits")
}
