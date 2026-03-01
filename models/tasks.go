package models

import "time"

// Action Types for the Worker
const (
    ActionImmediate = "immediate_prorated" // Charge now for mid-month signup/upgrade
    ActionRenewal   = "renewal"            // Standard global cycle charge (e.g. on the 1st)
    ActionUpgrade   = "upgrade"            // Scheduled plan changes
)

// Billing Cycle Types
const (
    BillingMonthly = "MONTHLY"
    BillingAnnual  = "ANNUAL"
)

// BillingTask represents the payload sent to RabbitMQ workers
type BillingTask struct {
    RunID                  string    `json:"run_id"`
    BillingType            string    `json:"billing_type"` // "MONTHLY" or "ANNUAL"
    WorkspaceID            int       `json:"workspace_id"`
    CreatorID              int       `json:"creator_id"`
    SubscriptionID         int       `json:"subscription_id"`
    Action                 string    `json:"action"`       
    PlanToBill             int       `json:"plan_to_bill"` 
    ProviderSubscriptionID string    `json:"provider_subscription_id"`
    
    // Amount is used when Action is ActionImmediate. 
    // If Action is ActionRenewal, the worker should use the PlanToBill price.
    Amount                 float64   `json:"amount"`       
    IsProrated             bool      `json:"is_prorated"`
    
    // Period details for logging and billing transparency
    BillingPeriodStart     time.Time `json:"billing_period_start"`
    BillingPeriodEnd       time.Time `json:"billing_period_end"`
}

type RecordingTask struct {
    ID              int    `json:"id"`
    Status          string `json:"status"`
    StorageID       string `json:"storage_id"`
    StorageServerIP string `json:"storage_server_ip"`
    Trim            string `json:"trim"`
}

type FailedBillingTask struct {
    RunID          string `json:"run_id"`
    WorkspaceID    int    `json:"workspace_id"`
    SubscriptionID int    `json:"subscription_id"`
    CreatorID      int    `json:"creator_id"`
    Reason         string `json:"reason"`
}

type PaymentReceiptTask struct {
    RunID          string  `json:"run_id"`
    WorkspaceID    int     `json:"workspace_id"`
    SubscriptionID int     `json:"subscription_id"`
    CreatorID      int     `json:"creator_id"`
    CardLast4      string  `json:"card_last_4"`
    CardBrand      string  `json:"card_brand"`
    PaymentAmount  float64 `json:"payment_amount"`
    Timestamp      int64   `json:"timestamp"`
}