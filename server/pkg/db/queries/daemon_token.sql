-- name: CreateDaemonToken :one
INSERT INTO daemon_token (token_hash, workspace_id, daemon_id, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetDaemonTokenByHash :one
SELECT * FROM daemon_token
WHERE token_hash = $1 AND expires_at > now();

-- name: DeleteDaemonTokensByWorkspaceAndDaemons :many
-- Deletes every daemon_token row matching the (workspace_id, daemon_id)
-- pairs implied by `daemon_ids`. Used by the member-revocation flow to
-- nuke tokens for all runtimes a leaving member owned in one shot.
-- Returns token_hash so the caller can invalidate auth.DaemonTokenCache
-- before the 10-minute TTL expires — without that invalidate, a daemon
-- can keep using its stale token until cache eviction even though the
-- DB row is gone.
DELETE FROM daemon_token
WHERE workspace_id = @workspace_id
  AND daemon_id = ANY(@daemon_ids::text[])
RETURNING token_hash;

-- name: DeleteExpiredDaemonTokens :exec
DELETE FROM daemon_token
WHERE expires_at <= now();

-- name: CreateDaemonTokenByUser :one
-- Farmhouse: a daemon token minted by a workspace admin, who owns the runtimes it registers.
INSERT INTO daemon_token (token_hash, workspace_id, daemon_id, expires_at, created_by)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListDaemonTokensByWorkspace :many
-- Farmhouse: the API-minted daemon tokens of a workspace, without their hashes.
SELECT id, workspace_id, daemon_id, expires_at, created_at, created_by FROM daemon_token
WHERE workspace_id = $1 AND created_by IS NOT NULL
ORDER BY created_at;

-- name: DeleteDaemonTokenByID :one
-- Farmhouse: revoke one daemon token. Returns token_hash so the caller can invalidate
-- auth.DaemonTokenCache at once, and daemon_id so it can close the daemon's connections.
DELETE FROM daemon_token
WHERE id = $1 AND workspace_id = $2
RETURNING token_hash, daemon_id;
