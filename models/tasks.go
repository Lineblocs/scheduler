package models

// BillingTask represents the payload sent to RabbitMQ workers
type BillingTask struct {
	RunID                  string `json:"run_id"`
	BillingType            string `json:"billing_type"` // "monthly" or "annual"
	WorkspaceID            int    `json:"workspace_id"`
	CreatorID              int    `json:"creator_id"`
	SubscriptionID         int    `json:"subscription_id"`
	Action                 string `json:"action"`       // "renewal" or "upgrade"
	PlanToBill             int    `json:"plan_to_bill"` // The plan ID they are actually being charged for
	ProviderSubscriptionID string `json:"provider_subscription_id"`
}

type RecordingTask struct {
	ID              int    `json:"id"`
	Status          string `json:"status"`
	StorageID       int    `json:"storage_id"`
	StorageServerIP string `json:"storage_server_ip"`
	Trim            string `json:"trim"`
}