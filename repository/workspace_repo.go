package repository

import (
	"context"

	helpers "github.com/Lineblocs/go-helpers"
	"lineblocs.com/scheduler/internal/db"
	"lineblocs.com/scheduler/models"
)

// WorkspaceRepo provides database access for workspaces, users, DIDs, and calls.
type WorkspaceRepo struct {
	db db.DBTX
}

// NewWorkspaceRepo creates a new WorkspaceRepo.
func NewWorkspaceRepo(dbtx db.DBTX) *WorkspaceRepo {
	return &WorkspaceRepo{db: dbtx}
}

// WithTx returns a copy using the given transaction.
func (r *WorkspaceRepo) WithTx(tx db.DBTX) *WorkspaceRepo {
	return &WorkspaceRepo{db: tx}
}

func (r *WorkspaceRepo) GetUserCount(ctx context.Context, workspaceID int) (int, error) {
	var count int
	err := r.db.GetContext(ctx, &count, "SELECT COUNT(*) FROM workspaces_users WHERE workspace_id = ?", workspaceID)
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (r *WorkspaceRepo) GetDID(ctx context.Context, id int) (*models.DIDRow, error) {
	var did models.DIDRow
	err := r.db.GetContext(ctx, &did, "SELECT id, monthly_cost FROM did_numbers WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	return &did, nil
}

func (r *WorkspaceRepo) GetCall(ctx context.Context, id int) (*models.CallRow, error) {
	var call models.CallRow
	err := r.db.GetContext(ctx, &call, "SELECT id, duration FROM calls WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	return &call, nil
}

// GetWorkspaceFromDB delegates to helpers for backward compatibility with the old interface.
func (r *WorkspaceRepo) GetWorkspaceFromDB(id int) (*helpers.Workspace, error) {
	return helpers.GetWorkspaceFromDB(id)
}

func (r *WorkspaceRepo) GetUserFromDB(id int) (*helpers.User, error) {
	return helpers.GetUserFromDB(id)
}

func (r *WorkspaceRepo) GetWorkspaceBillingInfo(workspace *helpers.Workspace) (*helpers.WorkspaceBillingInfo, error) {
	return helpers.GetWorkspaceBillingInfo(workspace)
}

func (r *WorkspaceRepo) GetDIDFromDB(id int) (*helpers.DIDNumber, error) {
	return helpers.GetDIDFromDB(id)
}

func (r *WorkspaceRepo) GetCallFromDB(id int) (*helpers.Call, error) {
	return helpers.GetCallFromDB(id)
}
