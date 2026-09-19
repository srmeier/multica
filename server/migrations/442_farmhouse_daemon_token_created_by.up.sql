-- Farmhouse: daemon tokens minted through the API (POST /api/workspaces/{id}/daemon-tokens) record the
-- workspace admin who minted them. That user owns the runtimes the token registers, so the claim
-- handler can mint task tokens for their tasks. Tokens minted for remote MCP keep created_by NULL.
-- No foreign key: DaemonRegister checks that the creator is still a member of the workspace, and
-- deleting a workspace deletes its daemon tokens (workspace_delete.sql).
ALTER TABLE daemon_token ADD COLUMN IF NOT EXISTS created_by UUID;
