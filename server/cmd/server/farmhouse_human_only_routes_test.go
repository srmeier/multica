package main

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/auth"
)

// Farmhouse: an agent's task token acts as its runtime's owner, so account-level
// routes must refuse it — otherwise a prompt-injected agent could mint PATs that
// outlive its task, change the account, create workspaces or bind the account to
// an outside identity. The same routes keep working for the account holder.
func TestFarmhouseAccountRoutesRejectTaskTokens(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}

	ctx := context.Background()
	var agentID string
	if err := testPool.QueryRow(ctx, `
		SELECT id::text FROM agent WHERE workspace_id = $1 ORDER BY created_at LIMIT 1
	`, testWorkspaceID).Scan(&agentID); err != nil {
		t.Fatalf("load integration-test agent: %v", err)
	}
	taskID := ensureAgentTask(t, agentID)
	taskToken, err := auth.GenerateAgentTaskToken()
	if err != nil {
		t.Fatalf("generate task token: %v", err)
	}
	var tokenID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO task_token (token_hash, task_id, agent_id, workspace_id, user_id, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id::text
	`, auth.HashToken(taskToken), taskID, agentID, testWorkspaceID, testUserID, time.Now().Add(time.Hour)).Scan(&tokenID); err != nil {
		t.Fatalf("insert task token: %v", err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), `DELETE FROM task_token WHERE id = $1`, tokenID); err != nil {
			t.Logf("delete task token fixture: %v", err)
		}
	})

	do := func(t *testing.T, token, method, path string) int {
		t.Helper()
		req, err := http.NewRequest(method, testServer.URL+path, bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatalf("create request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("perform request: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	const someID = "00000000-0000-0000-0000-000000000097"
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/tokens"},
		{http.MethodPost, "/api/tokens"},
		{http.MethodPost, "/api/tokens/current/renew"},
		{http.MethodDelete, "/api/tokens/" + someID},
		{http.MethodPatch, "/api/me"},
		{http.MethodPost, "/api/workspaces"},
		{http.MethodPost, "/api/invitations/" + someID + "/accept"},
		{http.MethodPost, "/api/invitations/" + someID + "/decline"},
		{http.MethodPost, "/api/share-links/join"},
		{http.MethodPost, "/api/slack/binding/redeem"},
		{http.MethodPost, "/api/lark/binding/redeem"},
		{http.MethodPost, "/api/telegram/binding/redeem"},
		{http.MethodGet, "/api/integrations/composio/connections"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			if got := do(t, taskToken, tc.method, tc.path); got != http.StatusForbidden {
				t.Fatalf("task token: status = %d, want 403", got)
			}
		})
	}

	t.Run("the account holder still lists their tokens", func(t *testing.T) {
		if got := do(t, testToken, http.MethodGet, "/api/tokens"); got != http.StatusOK {
			t.Fatalf("human token: status = %d, want 200", got)
		}
	})
	t.Run("a task token still reads its identity and workspaces", func(t *testing.T) {
		for _, path := range []string{"/api/me", "/api/workspaces"} {
			if got := do(t, taskToken, http.MethodGet, path); got != http.StatusOK {
				t.Fatalf("task token GET %s: status = %d, want 200", path, got)
			}
		}
	})
}
