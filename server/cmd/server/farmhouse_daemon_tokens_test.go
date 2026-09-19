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

	"github.com/gorilla/websocket"

	"github.com/multica-ai/multica/server/internal/auth"
)

// Farmhouse (V21): a workspace owner or admin, acting as a human, mints a
// daemon token (mdt_) for a runtime pod. The token works only on /api/daemon/*
// in its own workspace, the runtimes it registers belong to its minter (so
// their tasks get task tokens), and revoking it refuses the token at once and
// closes its daemon's socket, since claims over an open socket aren't rechecked.
func TestFarmhouseDaemonTokens(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	ctx := context.Background()

	call := func(t *testing.T, token, method, path string, body any) (int, []byte) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			reader = bytes.NewReader(raw)
		}
		req, err := http.NewRequest(method, testServer.URL+path, reader)
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
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}
	tokensPath := "/api/workspaces/" + testWorkspaceID + "/daemon-tokens"
	const daemonID = "farmhouse-v21-daemon"

	type minted struct {
		ID, WorkspaceID, DaemonID, Token string
		CreatedBy                        *string
	}
	mint := func(t *testing.T) minted {
		t.Helper()
		status, body := call(t, testToken, http.MethodPost, tokensPath, map[string]any{"daemon_id": daemonID, "expires_in_days": 2})
		if status != http.StatusCreated {
			t.Fatalf("mint: status = %d, body %s", status, body)
		}
		var out struct {
			ID          string  `json:"id"`
			WorkspaceID string  `json:"workspace_id"`
			DaemonID    string  `json:"daemon_id"`
			Token       string  `json:"token"`
			CreatedBy   *string `json:"created_by"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode mint response: %v", err)
		}
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM daemon_token WHERE id = $1`, out.ID) })
		return minted{out.ID, out.WorkspaceID, out.DaemonID, out.Token, out.CreatedBy}
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE daemon_id = $1`, daemonID)
	})

	token := mint(t)

	t.Run("minting records the workspace, daemon and minter", func(t *testing.T) {
		if !strings.HasPrefix(token.Token, "mdt_") || token.WorkspaceID != testWorkspaceID || token.DaemonID != daemonID {
			t.Fatalf("minted %+v", token)
		}
		if token.CreatedBy == nil || *token.CreatedBy != testUserID {
			t.Fatalf("created_by = %v, want %s", token.CreatedBy, testUserID)
		}
	})

	t.Run("bad requests are refused", func(t *testing.T) {
		for _, body := range []map[string]any{
			{"daemon_id": ""},
			{"daemon_id": strings.Repeat("d", 201)},
			{"daemon_id": daemonID, "expires_in_days": 0},
			{"daemon_id": daemonID, "expires_in_days": 366},
		} {
			if status, out := call(t, testToken, http.MethodPost, tokensPath, body); status != http.StatusBadRequest {
				t.Fatalf("mint %v: status = %d, body %s", body, status, out)
			}
		}
	})

	t.Run("the list shows tokens without their values", func(t *testing.T) {
		status, body := call(t, testToken, http.MethodGet, tokensPath, nil)
		if status != http.StatusOK || !strings.Contains(string(body), token.ID) {
			t.Fatalf("list: status = %d, body %s", status, body)
		}
		if strings.Contains(string(body), token.Token) || strings.Contains(string(body), `"token"`) {
			t.Fatalf("the list carries a token value: %s", body)
		}
	})

	t.Run("only a human owner or admin mints", func(t *testing.T) {
		// A plain member of the workspace.
		const email = "farmhouse-v21-member@multica.ai"
		testPool.Exec(ctx, `DELETE FROM "user" WHERE email = $1`, email)
		var memberID string
		if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('V21 member', $1) RETURNING id`, email).Scan(&memberID); err != nil {
			t.Fatalf("create user: %v", err)
		}
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, memberID) })
		if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')`, testWorkspaceID, memberID); err != nil {
			t.Fatalf("add member: %v", err)
		}
		memberJWT, err := generateTestJWT(memberID, email, "V21 member")
		if err != nil {
			t.Fatalf("member JWT: %v", err)
		}
		if status, body := call(t, memberJWT, http.MethodPost, tokensPath, map[string]any{"daemon_id": daemonID}); status != http.StatusForbidden {
			t.Fatalf("member mint: status = %d, body %s", status, body)
		}

		// An agent's task token, although its user owns the workspace.
		var agentID string
		if err := testPool.QueryRow(ctx, `SELECT id::text FROM agent WHERE workspace_id = $1 ORDER BY created_at LIMIT 1`, testWorkspaceID).Scan(&agentID); err != nil {
			t.Fatalf("load agent: %v", err)
		}
		taskToken, err := auth.GenerateAgentTaskToken()
		if err != nil {
			t.Fatalf("generate task token: %v", err)
		}
		var taskTokenID string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO task_token (token_hash, task_id, agent_id, workspace_id, user_id, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id::text
		`, auth.HashToken(taskToken), ensureAgentTask(t, agentID), agentID, testWorkspaceID, testUserID, time.Now().Add(time.Hour)).Scan(&taskTokenID); err != nil {
			t.Fatalf("insert task token: %v", err)
		}
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM task_token WHERE id = $1`, taskTokenID) })
		for _, tc := range []struct{ method, path string }{
			{http.MethodPost, tokensPath},
			{http.MethodDelete, tokensPath + "/" + token.ID},
		} {
			if status, body := call(t, taskToken, tc.method, tc.path, map[string]any{"daemon_id": daemonID}); status != http.StatusForbidden {
				t.Fatalf("task token %s %s: status = %d, body %s", tc.method, tc.path, status, body)
			}
		}
	})

	register := func(t *testing.T, tok, workspaceID string) (int, []byte) {
		t.Helper()
		return call(t, tok, http.MethodPost, "/api/daemon/register", map[string]any{
			"workspace_id": workspaceID, "daemon_id": daemonID, "device_name": "fh-v21",
			"runtimes": []map[string]any{{"name": "Bob (fh-v21)", "type": "bob", "version": "test", "status": "online"}},
		})
	}
	var runtimeID string

	t.Run("its runtimes belong to the minter", func(t *testing.T) {
		status, body := register(t, token.Token, testWorkspaceID)
		if status != http.StatusOK {
			t.Fatalf("register: status = %d, body %s", status, body)
		}
		var out struct {
			Runtimes []struct {
				ID string `json:"id"`
			} `json:"runtimes"`
		}
		if err := json.Unmarshal(body, &out); err != nil || len(out.Runtimes) != 1 {
			t.Fatalf("register response %s", body)
		}
		runtimeID = out.Runtimes[0].ID
		var owner string
		if err := testPool.QueryRow(ctx, `SELECT coalesce(owner_id::text, '') FROM agent_runtime WHERE id = $1`, runtimeID).Scan(&owner); err != nil {
			t.Fatalf("load runtime: %v", err)
		}
		if owner != testUserID {
			t.Fatalf("runtime owner = %q, want the minter %s", owner, testUserID)
		}
	})

	t.Run("it stays in its workspace and off user routes", func(t *testing.T) {
		const slug = "farmhouse-v21-other"
		testPool.Exec(ctx, `DELETE FROM workspace WHERE slug = $1`, slug)
		var otherID string
		if err := testPool.QueryRow(ctx, `INSERT INTO workspace (name, slug, description) VALUES ('Farmhouse V21 other', $1, '') RETURNING id`, slug).Scan(&otherID); err != nil {
			t.Fatalf("create workspace: %v", err)
		}
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, otherID) })
		if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, otherID, testUserID); err != nil {
			t.Fatalf("add member: %v", err)
		}
		if status, body := register(t, token.Token, otherID); status != http.StatusNotFound {
			t.Fatalf("register in another workspace: status = %d, body %s", status, body)
		}
		status, body := call(t, token.Token, http.MethodGet, "/api/daemon/workspaces", nil)
		if status != http.StatusOK || !strings.Contains(string(body), testWorkspaceID) || strings.Contains(string(body), otherID) {
			t.Fatalf("daemon workspaces: status = %d, body %s", status, body)
		}
		for _, path := range []string{"/api/me", "/api/workspaces", "/api/issues", "/api/tokens"} {
			if status, _ := call(t, token.Token, http.MethodGet, path, nil); status != http.StatusUnauthorized {
				t.Fatalf("GET %s with the daemon token: status = %d, want 401", path, status)
			}
		}
		// Nor does it reach another daemon's runtimes in its own workspace: another runtime pod's.
		if status, body := call(t, token.Token, http.MethodPost, "/api/daemon/register", map[string]any{
			"workspace_id": testWorkspaceID, "daemon_id": "farmhouse-v21-peer", "device_name": "fh-v21-peer",
			"runtimes": []map[string]any{{"name": "Bob (fh-v21-peer)", "type": "bob", "version": "test", "status": "online"}},
		}); status != http.StatusForbidden {
			t.Fatalf("register as another daemon: status = %d, body %s", status, body)
		}
		var peerID string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, owner_id, last_seen_at)
			VALUES ($1, 'farmhouse-v21-peer', 'Bob (fh-v21-peer)', 'local', 'bob', 'online', 'fh-v21-peer', '{}'::jsonb, $2, now())
			RETURNING id`, testWorkspaceID, testUserID).Scan(&peerID); err != nil {
			t.Fatalf("create peer runtime: %v", err)
		}
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE id = $1`, peerID) })
		for _, tc := range []struct{ method, path string }{
			{http.MethodPost, "/api/daemon/runtimes/" + peerID + "/tasks/claim"},
			{http.MethodGet, "/api/daemon/runtimes/" + peerID + "/tasks/pending"},
		} {
			if status, body := call(t, token.Token, tc.method, tc.path, nil); status != http.StatusNotFound {
				t.Fatalf("%s %s: status = %d, body %s", tc.method, tc.path, status, body)
			}
		}
		if status, body := call(t, token.Token, http.MethodGet, "/api/daemon/runtimes/"+runtimeID+"/tasks/pending", nil); status != http.StatusOK {
			t.Fatalf("own runtime's pending tasks: status = %d, body %s", status, body)
		}
		// Another workspace's admin can't revoke this workspace's token through their own workspace.
		if status, body := call(t, testToken, http.MethodDelete, "/api/workspaces/"+otherID+"/daemon-tokens/"+token.ID, nil); status != http.StatusNotFound {
			t.Fatalf("revoke through another workspace: status = %d, body %s", status, body)
		}
	})

	t.Run("revoking refuses it at once and closes its socket", func(t *testing.T) {
		if runtimeID == "" {
			t.Skip("no runtime registered")
		}
		wsURL := "ws" + strings.TrimPrefix(testServer.URL, "http") + "/api/daemon/ws?runtime_ids=" + runtimeID
		conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer " + token.Token}})
		if err != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			t.Fatalf("daemon socket: %v (status %d)", err, status)
		}
		defer conn.Close()
		closed := make(chan error, 1)
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					closed <- err
					return
				}
			}
		}()

		if status, body := call(t, testToken, http.MethodDelete, tokensPath+"/"+token.ID, nil); status != http.StatusNoContent {
			t.Fatalf("revoke: status = %d, body %s", status, body)
		}
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("the daemon's socket stayed open after its token was revoked")
		}
		if status, _ := call(t, token.Token, http.MethodGet, "/api/daemon/workspaces", nil); status != http.StatusUnauthorized {
			t.Fatalf("daemon workspaces after revoke: status = %d, want 401", status)
		}
		if _, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer " + token.Token}}); err == nil {
			t.Fatal("a revoked daemon token reopened its socket")
		} else if resp != nil && resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("reconnect after revoke: status = %d, want 401", resp.StatusCode)
		}
		if status, _ := call(t, testToken, http.MethodDelete, tokensPath+"/"+token.ID, nil); status != http.StatusNotFound {
			t.Fatalf("revoking twice: status = %d, want 404", status)
		}
	})
}
