-- name: InsertAgentNonce :execrows
-- Agent-auth path (app.agent GUC). The unique (site_id, nonce) index makes a
-- replayed nonce a no-op via ON CONFLICT, returning 0 rows affected.
INSERT INTO agent_nonces (site_id, nonce)
VALUES ($1, $2)
ON CONFLICT (site_id, nonce) DO NOTHING;

-- name: PruneAgentNonces :execrows
-- Drops nonces older than the freshness window; called opportunistically.
DELETE FROM agent_nonces
WHERE created_at < $1;
