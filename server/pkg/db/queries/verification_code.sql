-- name: CreateVerificationCode :one
INSERT INTO verification_code (email, code, expires_at)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetLatestVerificationCode :one
SELECT * FROM verification_code
WHERE email = $1
  AND used = FALSE
  AND expires_at > now()
  AND attempts < 5
ORDER BY created_at DESC
LIMIT 1;

-- name: MarkVerificationCodeUsed :exec
UPDATE verification_code
SET used = TRUE
WHERE id = $1;

-- name: IncrementVerificationCodeAttempts :exec
UPDATE verification_code
SET attempts = attempts + 1
WHERE id = $1;

-- name: GetLatestCodeByEmail :one
SELECT * FROM verification_code
WHERE email = $1
ORDER BY created_at DESC
LIMIT 1;

-- Kept for a day, so failed attempts count toward the daily limit (Farmhouse).
-- name: DeleteExpiredVerificationCodes :exec
DELETE FROM verification_code
WHERE expires_at < now() - interval '1 day';

-- Failed attempts at an email's codes since a time: past a daily limit, it gets no new code (Farmhouse).
-- name: CountVerificationAttemptsSince :one
SELECT coalesce(sum(attempts), 0)::int AS attempts FROM verification_code
WHERE email = $1 AND created_at > $2;
