package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/williamlabdev/saas-forge/internal/notification/domain"
	tenantdomain "github.com/williamlabdev/saas-forge/internal/tenant/domain"
)

// fakeRoster is a minimal TenantRoster double: a fixed member list per
// tenant, or an error to simulate the tenant lookup itself failing.
type fakeRoster struct {
	members map[uuid.UUID][]tenantdomain.Member
	err     error
}

func (f *fakeRoster) Members(_ context.Context, tenantID uuid.UUID) ([]tenantdomain.Member, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.members[tenantID], nil
}

// countingEmailSender lets a test assert SystemNotifier attempted (or did
// not attempt) the best-effort email leg, without a real provider.
type countingEmailSender struct {
	sent int
	err  error
}

func (c *countingEmailSender) Send(context.Context, uuid.UUID, string, string) error {
	c.sent++
	return c.err
}

func TestSystemNotifier_NotifyTenantRoles_ExcludesSelf(t *testing.T) {
	tenantID := uuid.New()
	owner := uuid.New()
	admin := uuid.New()
	roster := &fakeRoster{members: map[uuid.UUID][]tenantdomain.Member{
		tenantID: {
			{UserID: owner, Role: "owner"},
			{UserID: admin, Role: "admin"},
		},
	}}
	repo := &memRepo{}
	n := NewSystemNotifier(repo, roster, NewNoopEmailSender())

	err := n.NotifyTenantRoles(context.Background(), tenantID.String(), []string{"owner", "admin"}, owner, domain.KindSchemaProposal, "t", "b", "")
	require.NoError(t, err)

	require.Len(t, repo.items, 1, "the excluded member must not receive a row")
	assert.Equal(t, admin, repo.items[0].UserID)
}

func TestSystemNotifier_NotifyTenantRoles_FiltersByRole(t *testing.T) {
	tenantID := uuid.New()
	owner := uuid.New()
	editor := uuid.New()
	viewer := uuid.New()
	roster := &fakeRoster{members: map[uuid.UUID][]tenantdomain.Member{
		tenantID: {
			{UserID: owner, Role: "owner"},
			{UserID: editor, Role: "editor"},
			{UserID: viewer, Role: "viewer"},
		},
	}}
	repo := &memRepo{}
	n := NewSystemNotifier(repo, roster, NewNoopEmailSender())

	err := n.NotifyTenantRoles(context.Background(), tenantID.String(), []string{"owner", "admin"}, uuid.Nil, domain.KindEntryPendingReview, "t", "b", "")
	require.NoError(t, err)

	require.Len(t, repo.items, 1, "only members whose role is in the requested set are notified")
	assert.Equal(t, owner, repo.items[0].UserID)
}

func TestSystemNotifier_NotifyTenantRoles_UUIDNilExcludesNobody(t *testing.T) {
	tenantID := uuid.New()
	owner := uuid.New()
	admin := uuid.New()
	roster := &fakeRoster{members: map[uuid.UUID][]tenantdomain.Member{
		tenantID: {
			{UserID: owner, Role: "owner"},
			{UserID: admin, Role: "admin"},
		},
	}}
	repo := &memRepo{}
	n := NewSystemNotifier(repo, roster, NewNoopEmailSender())

	err := n.NotifyTenantRoles(context.Background(), tenantID.String(), []string{"owner", "admin"}, uuid.Nil, domain.KindWebhookDead, "t", "b", "")
	require.NoError(t, err)

	assert.Len(t, repo.items, 2, "uuid.Nil (no human actor) excludes nobody")
}

func TestSystemNotifier_NotifyTenantRoles_MalformedTenantIDFails(t *testing.T) {
	repo := &memRepo{}
	n := NewSystemNotifier(repo, &fakeRoster{}, NewNoopEmailSender())

	err := n.NotifyTenantRoles(context.Background(), "not-a-uuid", []string{"owner"}, uuid.Nil, domain.KindGeneral, "t", "b", "")
	require.Error(t, err, "unlike NotifyUser's single-recipient degrade, there is no member list to fall back to")
	assert.Empty(t, repo.items)
}

func TestSystemNotifier_NotifyTenantRoles_RosterErrorFailsWhole(t *testing.T) {
	repo := &memRepo{}
	n := NewSystemNotifier(repo, &fakeRoster{err: errors.New("tenant lookup down")}, NewNoopEmailSender())

	err := n.NotifyTenantRoles(context.Background(), uuid.New().String(), []string{"owner"}, uuid.Nil, domain.KindGeneral, "t", "b", "")
	require.Error(t, err)
	assert.Empty(t, repo.items)
}

func TestSystemNotifier_NotifyUser_SetsTenantAndLink(t *testing.T) {
	repo := &memRepo{}
	n := NewSystemNotifier(repo, &fakeRoster{}, NewNoopEmailSender())
	tenantID := uuid.New()

	err := n.NotifyUser(context.Background(), uuid.New(), tenantID.String(), domain.KindScheduleSucceeded, "t", "b", "/content/entries/1")
	require.NoError(t, err)

	require.Len(t, repo.items, 1)
	got := repo.items[0]
	require.NotNil(t, got.TenantID)
	assert.Equal(t, tenantID, *got.TenantID)
	require.NotNil(t, got.Link)
	assert.Equal(t, "/content/entries/1", *got.Link)
	assert.Equal(t, domain.KindScheduleSucceeded, got.Kind)
}

func TestSystemNotifier_NotifyUser_EmptyTenantDegradesToNoTenant(t *testing.T) {
	repo := &memRepo{}
	n := NewSystemNotifier(repo, &fakeRoster{}, NewNoopEmailSender())

	err := n.NotifyUser(context.Background(), uuid.New(), "", domain.KindGeneral, "t", "b", "")
	require.NoError(t, err)

	require.Len(t, repo.items, 1)
	assert.Nil(t, repo.items[0].TenantID)
	assert.Nil(t, repo.items[0].Link)
}

func TestSystemNotifier_NotifyUser_MalformedTenantDegradesRatherThanFails(t *testing.T) {
	repo := &memRepo{}
	n := NewSystemNotifier(repo, &fakeRoster{}, NewNoopEmailSender())

	err := n.NotifyUser(context.Background(), uuid.New(), "not-a-uuid", domain.KindGeneral, "t", "b", "")
	require.NoError(t, err, "a bad tenant string degrades to no tenant, it does not fail a committed event")

	require.Len(t, repo.items, 1)
	assert.Nil(t, repo.items[0].TenantID)
}

func TestSystemNotifier_NotifyUser_EmailIsBestEffort(t *testing.T) {
	repo := &memRepo{}
	email := &countingEmailSender{err: errors.New("smtp down")}
	n := NewSystemNotifier(repo, &fakeRoster{}, email)

	err := n.NotifyUser(context.Background(), uuid.New(), "", domain.KindGeneral, "t", "b", "")
	require.NoError(t, err, "an email failure must not fail the call — the in-app row already landed")
	assert.Len(t, repo.items, 1, "the in-app write still happened")
	assert.Equal(t, 1, email.sent, "email was attempted once, best-effort")
}
