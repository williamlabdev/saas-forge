package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/williamlabdev/saas-forge/internal/pkg/pagination"
	"github.com/williamlabdev/saas-forge/internal/user/domain"
	"github.com/williamlabdev/saas-forge/internal/user/repository/sqlc"
)

// ListPage returns up to limit users for the given filter and optional keyset cursor.
func (r *PostgresUserRepository) ListPage(
	ctx context.Context,
	statusFilter string,
	cursor *pagination.UserCursor,
	limit int,
) ([]*domain.User, error) {
	var rows []userRow
	if cursor == nil {
		recs, err := r.q.ListUsersFirstPage(ctx, sqlc.ListUsersFirstPageParams{
			StatusFilter: statusFilter,
			PageLimit:    int32(limit), //nolint:gosec // G115: limit is pagination.ClampLimit-bounded to [1,100].
		})
		if err != nil {
			return nil, mapPgError(err)
		}
		rows = make([]userRow, len(recs))
		for i, rec := range recs {
			rows[i] = sqlcListFirstToRow(rec)
		}
	} else {
		recs, err := r.q.ListUsersAfterCursor(ctx, sqlc.ListUsersAfterCursorParams{
			StatusFilter:    statusFilter,
			CursorCreatedAt: timeToPG(cursor.CreatedAt),
			CursorID:        uuidToPG(cursor.ID),
			PageLimit:       int32(limit), //nolint:gosec // G115: limit is pagination.ClampLimit-bounded to [1,100].
		})
		if err != nil {
			return nil, mapPgError(err)
		}
		rows = make([]userRow, len(recs))
		for i, rec := range recs {
			rows[i] = sqlcListAfterToRow(rec)
		}
	}

	out := make([]*domain.User, 0, len(rows))
	for _, row := range rows {
		u, err := r.rowToDomain(row)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

func sqlcListFirstToRow(rec sqlc.ListUsersFirstPageRow) userRow {
	return listFieldsToRow(
		rec.ID, rec.Username, rec.UsernameLookupHash, rec.EmailLookupHash,
		rec.EmailEncrypted, rec.EmailEncryptedNonce,
		rec.DisplayNameEncrypted, rec.DisplayNameEncryptedNonce,
		rec.PhoneEncrypted, rec.PhoneEncryptedNonce,
		rec.Preferences, rec.Status, rec.StatusVersion,
		rec.CreatedAt, rec.UpdatedAt, rec.DeletedAt,
	)
}

func sqlcListAfterToRow(rec sqlc.ListUsersAfterCursorRow) userRow {
	return listFieldsToRow(
		rec.ID, rec.Username, rec.UsernameLookupHash, rec.EmailLookupHash,
		rec.EmailEncrypted, rec.EmailEncryptedNonce,
		rec.DisplayNameEncrypted, rec.DisplayNameEncryptedNonce,
		rec.PhoneEncrypted, rec.PhoneEncryptedNonce,
		rec.Preferences, rec.Status, rec.StatusVersion,
		rec.CreatedAt, rec.UpdatedAt, rec.DeletedAt,
	)
}

func listFieldsToRow(
	id pgtype.UUID,
	username string,
	usernameLookupHash, emailLookupHash []byte,
	emailEncrypted, emailEncryptedNonce []byte,
	displayNameEncrypted, displayNameEncryptedNonce []byte,
	phoneEncrypted, phoneEncryptedNonce []byte,
	preferences []byte,
	status string,
	statusVersion int32,
	createdAt, updatedAt pgtype.Timestamptz,
	deletedAt pgtype.Timestamptz,
) userRow {
	return userRow{
		ID:                        mustUUID(id),
		Username:                  username,
		UsernameLookupHash:        usernameLookupHash,
		EmailLookupHash:           emailLookupHash,
		EmailEncrypted:            emailEncrypted,
		EmailEncryptedNonce:       emailEncryptedNonce,
		DisplayNameEncrypted:      displayNameEncrypted,
		DisplayNameEncryptedNonce: displayNameEncryptedNonce,
		PhoneEncrypted:            phoneEncrypted,
		PhoneEncryptedNonce:       phoneEncryptedNonce,
		Preferences:               preferences,
		Status:                    statusFromPG(status),
		StatusVersion:             int(statusVersion),
		CreatedAt:                 createdAt.Time,
		UpdatedAt:                 updatedAt.Time,
		DeletedAt:                 timeFromPG(deletedAt),
	}
}
