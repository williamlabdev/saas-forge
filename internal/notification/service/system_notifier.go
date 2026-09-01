package service

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/williamlabdev/saas-forge/internal/notification/domain"
	"github.com/williamlabdev/saas-forge/internal/notification/repository"
)

// SystemNotifier writes a notification that no caller asked for.
//
// WHY IT DOES NOT AUTHORIZE, when NotificationService.Create right next to it
// does. Create serves an HTTP caller, and the subject it authorizes is the
// recipient — it can only ever write to the mailbox of the person making the
// request. There is no "send to someone else" in that shape and this is not
// that shape: the sender here is the platform reacting to an event, so there is
// no subject in context to authorize and no principal whose permissions could
// be the answer. The activity recorder writes straight to its repository for
// the same reason.
//
// WHAT KEEPS THIS FROM BECOMING A BACKDOOR. The recipient is chosen by the
// caller, so every call site has to justify its own choice; that justification
// belongs at the call site, not here (see notifyProposalFiled). What this type
// deliberately does NOT offer is a read — a system notifier that could list
// somebody's mailbox would be a permission bypass rather than a write nobody
// authorized.
type SystemNotifier struct {
	repo repository.NotificationRepository
}

func NewSystemNotifier(repo repository.NotificationRepository) *SystemNotifier {
	return &SystemNotifier{repo: repo}
}

// NotifyUser drops one notification into userID's mailbox.
//
// Title and body are platform-authored copy, not caller input, so they are not
// run through the validator that guards CreateInput — there is no untrusted
// string here to reject. Call sites that would interpolate user or tenant data
// into either are the ones that need to think about what they are copying in.
func (n *SystemNotifier) NotifyUser(ctx context.Context, userID uuid.UUID, title, body string) error {
	return n.repo.Create(ctx, &domain.Notification{
		ID:        uuid.New(),
		UserID:    userID,
		Title:     title,
		Body:      body,
		CreatedAt: time.Now().UTC(),
	})
}
