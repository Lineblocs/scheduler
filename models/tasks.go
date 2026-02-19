package models

type BillingTask struct {
    WorkspaceID int    `json:"workspace_id"`
    CreatorID   int    `json:"creator_id"`
    BillingType string `json:"billing_type"`
    RunID       string `json:"run_id"`
}