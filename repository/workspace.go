package repository

import (
	helpers "github.com/Lineblocs/go-helpers"
)

type WorkspaceRepository interface {
	GetWorkspaceFromDB(id int) (*helpers.Workspace, error)
	GetWorkspaceBillingInfo(workspace *helpers.Workspace) (*helpers.WorkspaceBillingInfo, error)
	GetUserFromDB(id int) (*helpers.User, error)
	GetDIDFromDB(id int) (*helpers.DIDNumber, error)
	GetCallFromDB(id int) (*helpers.Call, error)
}

type WorkspaceService struct{}

func NewWorkspaceService() WorkspaceRepository {
	return &WorkspaceService{}
}

func (ws *WorkspaceService) GetWorkspaceFromDB(id int) (*helpers.Workspace, error) {
	return helpers.GetWorkspaceFromDB(id)
}

func (ws *WorkspaceService) GetUserFromDB(id int) (*helpers.User, error) {
	return helpers.GetUserFromDB(id)
}

func (ws *WorkspaceService) GetWorkspaceBillingInfo(workspace *helpers.Workspace) (*helpers.WorkspaceBillingInfo, error) {
	return helpers.GetWorkspaceBillingInfo(workspace)
}

func (ws *WorkspaceService) GetDIDFromDB(id int) (*helpers.DIDNumber, error) {
	return helpers.GetDIDFromDB(id)
}

func (ws *WorkspaceService) GetCallFromDB(id int) (*helpers.Call, error) {
	return helpers.GetCallFromDB(id)
}
