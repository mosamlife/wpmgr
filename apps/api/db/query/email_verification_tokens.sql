-- name: InsertEmailVerificationToken :one
-- Run under Pool.InAgentTx (app.agent='on'). desired_plan is a nullable M16
-- "sign up into a plan" hint (Phase 0); pass nil for an ordinary signup.
INSERT INTO email_verification_tokens (user_id, token_hash, expires_at, desired_plan)
VALUES (@user_id, @token_hash, @expires_at, @desired_plan)
RETURNING *;

-- name: ConsumeEmailVerificationToken :one
-- Atomically consume an unused, unexpired verification token. The returned
-- row's desired_plan (if any) is single-use: it is gone with the token.
UPDATE email_verification_tokens
SET used_at = now()
WHERE token_hash = @token_hash
  AND used_at IS NULL
  AND expires_at > now()
RETURNING *;

-- name: InvalidateUserEmailVerificationTokens :exec
UPDATE email_verification_tokens
SET used_at = now()
WHERE user_id = @user_id AND used_at IS NULL;

-- name: GetLatestDesiredPlanForUser :one
-- Looks up the most recent desired_plan captured across a user's
-- verification tokens (active or already consumed/invalidated), so a resent
-- verification link can carry the SAME plan intent forward onto its new
-- token instead of losing it when the prior token is invalidated.
SELECT desired_plan FROM email_verification_tokens
WHERE user_id = @user_id
ORDER BY created_at DESC
LIMIT 1;

-- name: SetUserPending :exec
UPDATE users SET status = 'pending', updated_at = now() WHERE id = $1;

-- name: MarkUserEmailVerified :exec
-- Activate + mark verified (used on self-serve activation and trusted bootstrap).
UPDATE users SET status = 'active', email_verified_at = now(), updated_at = now() WHERE id = $1;
