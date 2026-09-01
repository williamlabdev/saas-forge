-- name: CreateUser :exec
INSERT INTO users (
    id, username, username_lookup_hash, email_lookup_hash,
    email_encrypted, email_encrypted_nonce,
    display_name_encrypted, display_name_encrypted_nonce,
    phone_encrypted, phone_encrypted_nonce,
    preferences, status, status_version, created_at, updated_at, deleted_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16
);

-- name: UserByID :one
SELECT id, username, username_lookup_hash, email_lookup_hash,
    email_encrypted, email_encrypted_nonce,
    display_name_encrypted, display_name_encrypted_nonce,
    phone_encrypted, phone_encrypted_nonce,
    preferences, status, status_version, created_at, updated_at, deleted_at
FROM users
WHERE id = $1;

-- name: UserByEmailHash :one
SELECT id, username, username_lookup_hash, email_lookup_hash,
    email_encrypted, email_encrypted_nonce,
    display_name_encrypted, display_name_encrypted_nonce,
    phone_encrypted, phone_encrypted_nonce,
    preferences, status, status_version, created_at, updated_at, deleted_at
FROM users
WHERE email_lookup_hash = $1;

-- name: UserByUsernameHash :one
SELECT id, username, username_lookup_hash, email_lookup_hash,
    email_encrypted, email_encrypted_nonce,
    display_name_encrypted, display_name_encrypted_nonce,
    phone_encrypted, phone_encrypted_nonce,
    preferences, status, status_version, created_at, updated_at, deleted_at
FROM users
WHERE username_lookup_hash = $1;

-- name: UpdateUser :exec
UPDATE users SET
    username = $2,
    username_lookup_hash = $3,
    email_lookup_hash = $4,
    email_encrypted = $5,
    email_encrypted_nonce = $6,
    display_name_encrypted = $7,
    display_name_encrypted_nonce = $8,
    phone_encrypted = $9,
    phone_encrypted_nonce = $10,
    preferences = $11,
    status = $12,
    deleted_at = $13
WHERE id = $1;

-- name: MergeUserPreferences :execrows
UPDATE users
SET preferences = preferences || $2::jsonb
WHERE id = $1 AND status <> 'deleted';

-- name: ReplaceUserPreferences :execrows
UPDATE users
SET preferences = $2::jsonb
WHERE id = $1 AND status <> 'deleted';

-- name: SoftDeleteUser :execrows
UPDATE users
SET status = 'deleted', deleted_at = now()
WHERE id = $1 AND status <> 'deleted';

-- name: BumpUserStatusVersion :one
UPDATE users
SET status_version = status_version + 1
WHERE id = $1
RETURNING status_version;

-- name: ListUsersFirstPage :many
SELECT id, username, username_lookup_hash, email_lookup_hash,
    email_encrypted, email_encrypted_nonce,
    display_name_encrypted, display_name_encrypted_nonce,
    phone_encrypted, phone_encrypted_nonce,
    preferences, status, status_version, created_at, updated_at, deleted_at
FROM users
WHERE (
    sqlc.arg(status_filter)::text = 'all' AND status <> 'deleted' AND deleted_at IS NULL
    OR sqlc.arg(status_filter)::text = 'deleted' AND status = 'deleted'
    OR sqlc.arg(status_filter)::text NOT IN ('all', 'deleted') AND status::text = sqlc.arg(status_filter)::text AND deleted_at IS NULL
)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: ListUsersAfterCursor :many
SELECT id, username, username_lookup_hash, email_lookup_hash,
    email_encrypted, email_encrypted_nonce,
    display_name_encrypted, display_name_encrypted_nonce,
    phone_encrypted, phone_encrypted_nonce,
    preferences, status, status_version, created_at, updated_at, deleted_at
FROM users
WHERE (
    sqlc.arg(status_filter)::text = 'all' AND status <> 'deleted' AND deleted_at IS NULL
    OR sqlc.arg(status_filter)::text = 'deleted' AND status = 'deleted'
    OR sqlc.arg(status_filter)::text NOT IN ('all', 'deleted') AND status::text = sqlc.arg(status_filter)::text AND deleted_at IS NULL
)
AND (created_at, id) < (sqlc.arg(cursor_created_at)::timestamptz, sqlc.arg(cursor_id)::uuid)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit)::int;
