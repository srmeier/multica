package handler

// Farmhouse: daemon tokens (mdt_) minted and revoked through the API, so a runtime pod can run its
// daemon with a credential bound to one workspace and one daemon id instead of a user PAT. The
// minter, a workspace owner or admin acting as a human, owns the runtimes the token registers
// (DaemonRegister), so task tokens act as them inside that workspace only.

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/auth"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	daemonTokenDefaultDays = 30
	daemonTokenMaxDays     = 365
)

type CreateDaemonTokenRequest struct {
	DaemonID      string `json:"daemon_id"`
	ExpiresInDays *int   `json:"expires_in_days"`
}

type DaemonTokenResponse struct {
	ID          string  `json:"id"`
	WorkspaceID string  `json:"workspace_id"`
	DaemonID    string  `json:"daemon_id"`
	ExpiresAt   string  `json:"expires_at"`
	CreatedAt   string  `json:"created_at"`
	CreatedBy   *string `json:"created_by"`
	// Token is set only in the response that mints it.
	Token string `json:"token,omitempty"`
}

func daemonTokenToResponse(id, workspaceID pgtype.UUID, daemonID string, expiresAt, createdAt pgtype.Timestamptz,
	createdBy pgtype.UUID) DaemonTokenResponse {
	resp := DaemonTokenResponse{
		ID: uuidToString(id), WorkspaceID: uuidToString(workspaceID), DaemonID: daemonID,
		ExpiresAt: timestampToString(expiresAt), CreatedAt: timestampToString(createdAt),
	}
	if createdBy.Valid {
		by := uuidToString(createdBy)
		resp.CreatedBy = &by
	}
	return resp
}

// CreateDaemonToken mints an mdt_ for the workspace in the URL and the daemon id in the body.
func (h *Handler) CreateDaemonToken(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	var req CreateDaemonTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.DaemonID = strings.TrimSpace(req.DaemonID)
	if req.DaemonID == "" || len(req.DaemonID) > 200 {
		writeError(w, http.StatusBadRequest, "daemon_id is required (at most 200 characters)")
		return
	}
	days := daemonTokenDefaultDays
	if req.ExpiresInDays != nil {
		days = *req.ExpiresInDays
	}
	if days < 1 || days > daemonTokenMaxDays {
		writeError(w, http.StatusBadRequest, "expires_in_days must be between 1 and 365")
		return
	}
	raw, err := auth.GenerateDaemonToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	row, err := h.Queries.CreateDaemonTokenByUser(r.Context(), db.CreateDaemonTokenByUserParams{
		TokenHash:   auth.HashToken(raw),
		WorkspaceID: workspaceID,
		DaemonID:    req.DaemonID,
		ExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(time.Duration(days) * 24 * time.Hour), Valid: true},
		CreatedBy:   parseUUID(userID),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create daemon token")
		return
	}
	resp := daemonTokenToResponse(row.ID, row.WorkspaceID, row.DaemonID, row.ExpiresAt, row.CreatedAt, row.CreatedBy)
	resp.Token = raw
	writeJSON(w, http.StatusCreated, resp)
}

// ListDaemonTokens lists the workspace's API-minted daemon tokens, never their values.
func (h *Handler) ListDaemonTokens(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	rows, err := h.Queries.ListDaemonTokensByWorkspace(r.Context(), workspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list daemon tokens")
		return
	}
	resp := make([]DaemonTokenResponse, len(rows))
	for i, row := range rows {
		resp[i] = daemonTokenToResponse(row.ID, row.WorkspaceID, row.DaemonID, row.ExpiresAt, row.CreatedAt, row.CreatedBy)
	}
	writeJSON(w, http.StatusOK, resp)
}

// RevokeDaemonToken deletes one daemon token and drops it from the token cache, so the next request
// with it is refused.
func (h *Handler) RevokeDaemonToken(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	tokenID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "tokenId"), "token id")
	if !ok {
		return
	}
	hash, err := h.Queries.DeleteDaemonTokenByID(r.Context(), db.DeleteDaemonTokenByIDParams{ID: tokenID, WorkspaceID: workspaceID})
	if err != nil {
		writeError(w, http.StatusNotFound, "daemon token not found")
		return
	}
	h.DaemonTokenCache.Invalidate(r.Context(), hash)
	w.WriteHeader(http.StatusNoContent)
}
