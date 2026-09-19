package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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
		// V22: routes that reached past the task token's workspace.
		{http.MethodPost, "/api/cli-token"},
		{http.MethodPatch, "/api/me/onboarding"},
		{http.MethodPost, "/api/me/onboarding/complete"},
		{http.MethodPost, "/api/me/onboarding/cloud-waitlist"},
		{http.MethodPost, "/api/me/onboarding/runtime-bootstrap"},
		{http.MethodPost, "/api/me/onboarding/no-runtime-bootstrap"},
		{http.MethodGet, "/api/invitations"},
		{http.MethodGet, "/api/invitations/" + someID},
		{http.MethodPost, pluginBridgePrefix + "/hooks/some-key"},
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

// Farmhouse (V22): a task token is bound to one workspace. Its user's membership
// in another workspace mustn't let it list that workspace or download its
// attachments by id.
func TestFarmhouseTaskTokenStaysInItsWorkspace(t *testing.T) {
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
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM task_token WHERE id = $1`, tokenID) })

	const slug = "farmhouse-v22-other"
	_, _ = testPool.Exec(ctx, `DELETE FROM workspace WHERE slug = $1`, slug)
	var otherID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description) VALUES ('Farmhouse V22 other', $1, '') RETURNING id
	`, slug).Scan(&otherID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, otherID) })
	if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, otherID, testUserID); err != nil {
		t.Fatalf("add member: %v", err)
	}
	attachment := func(ws string) string {
		var id string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO attachment (workspace_id, uploader_type, uploader_id, filename, url, content_type, size_bytes)
			VALUES ($1, 'member', $2, 'v22.txt', 'https://cdn.test/v22.txt', 'text/plain', 3)
			RETURNING id`, ws, testUserID).Scan(&id); err != nil {
			t.Fatalf("seed attachment: %v", err)
		}
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM attachment WHERE id = $1`, id) })
		return id
	}
	otherAtt, ownAtt := attachment(otherID), attachment(testWorkspaceID)

	get := func(t *testing.T, token, path string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, testServer.URL+path, nil)
		if err != nil {
			t.Fatalf("create request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Workspace-ID", otherID) // ignored for a task token
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("perform request: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	t.Run("the workspace list has only the bound workspace", func(t *testing.T) {
		status, body := get(t, taskToken, "/api/workspaces")
		var list []struct {
			ID string `json:"id"`
		}
		if status != http.StatusOK || json.Unmarshal([]byte(body), &list) != nil {
			t.Fatalf("status = %d, body %s", status, body)
		}
		if len(list) != 1 || list[0].ID != testWorkspaceID {
			t.Fatalf("task token listed %v, want only %s", list, testWorkspaceID)
		}
		status, body = get(t, testToken, "/api/workspaces")
		if status != http.StatusOK || !strings.Contains(body, otherID) {
			t.Fatalf("human token: status = %d, other workspace listed = %v", status, strings.Contains(body, otherID))
		}
	})
	t.Run("another workspace's attachment is not found", func(t *testing.T) {
		if status, body := get(t, taskToken, "/api/attachments/"+otherAtt+"/download"); status != http.StatusNotFound ||
			!strings.Contains(body, "attachment not found") {
			t.Fatalf("task token: status = %d, body %s", status, body)
		}
		for _, tc := range []struct{ token, att string }{{taskToken, ownAtt}, {testToken, otherAtt}} {
			if _, body := get(t, tc.token, "/api/attachments/"+tc.att+"/download"); strings.Contains(body, "attachment not found") {
				t.Fatalf("attachment %s refused: %s", tc.att, body)
			}
		}
	})
}
