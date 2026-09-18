-- Farmhouse: daemon tokens minted through the API (POST /api/workspaces/{id}/daemon-tokens) record the
-- workspace admin who minted them. That user owns the runtimes the token registers, so the claim
-- handler can mint task tokens for their tasks. Tokens minted for remote MCP keep created_by NULL.
ALTER TABLE daemon_token ADD COLUMN created_by UUID REFERENCES "user"(id) ON DELETE CASCADE;
