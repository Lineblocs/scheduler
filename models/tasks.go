package models

import "time"

// Action Types for the Worker
const (
    ActionImmediate = "immediate_prorated" // Charge now for mid-month signup/upgrade
    ActionRenewal   = "renewal"            // Standard global cycle charge (e.g. on the 1st)
    ActionUpgrade   = "upgrade"            // Scheduled plan changes
tionUpgrade   = "upgrade"            // Scheduled plan changes
)

type InvoiceLineItem struct {
    KeyName     string `json:"key_name"`
    Name        string `json:"name"`
    Cents       int    `json:"cents"`
    IsRecurring bool   `json:"is_recurring"`
}

type MonthlyInvoiceTask struct {
    AlreadyGenerated    bool              `json:"already_generated"`
    RunID               string            `json:"run_id"`
    WorkspaceID         int               `json:"workspace_id"`
    CreatorID           int               `json:"creator_id"`
    InvoiceId           string            `json:"invoice_id"`
    InvoiceNo           string            `json:"invoice_no"`
    DueDate             time.Time         `json:"due_date"`
    Cents               int               `json:"cents"`
    CentsIncludingTaxes int               `json:"cents_including_taxes"`
    CentsTaxes          int               `json:"cents_taxes"`
    TaxMetadata         map[string]string `json:"tax_metadata"`
    CallCosts           int               `json:"call_costs"`
    RecordingCosts      int               `json:"recording_costs"`
    FaxCosts            int               `json:"fax_costs"`
    MembershipCosts     int               `json:"membership_costs"`
    NumberCosts         int               `json:"number_costs"`
    LineItems           []InvoiceLineItem `json:"line_items"`
}

type AnnualInvoiceTask struct {
    AlreadyGenerated    bool              `json:"already_generated"`
    RunID               string            `json:"run_id"`
    WorkspaceID         int               `json:"workspace_id"`
    CreatorID           int               `json:"creator_id"`
    InvoiceId           string            `json:"invoice_id"`
    InvoiceNo           string            `json:"invoice_no"`
    DueDate             time.Time         `json:"due_date"`
    Cents               int               `json:"cents"`
    CentsIncludingTaxes int               `json:"cents_including_taxes"`
    CentsTaxes          int               `json:"cents_taxes"`
    TaxMetadata         map[string]string `json:"tax_metadata"`
    CallCosts           int               `json:"call_costs"`
    RecordingCosts      int               `json:"recording_costs"`
    FaxCosts            int               `json:"fax_costs"`
    MembershipCosts     int               `json:"membership_costs"`
    NumberCosts         int               `json:"number_costs"`
    LineItems           []InvoiceLineItem `json:"line_items"`
}

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
    UserID                 int       `json:"user_id"`
    SubscriptionID         int       `json:"subscription_id"`
    Action                 string    `json:"action"`       
    PlanToBill             int       `json:"plan_to_bill"` 
    CancelPlan             bool       `json:"cancel_plan"` 
    ProviderSubscriptionID string    `json:"provider_subscription_id"`
    InvoiceID              string    `json:"invoice_id"`
    PaymentMethodID        string    `json:"payment_method_id"`
    BackupPaymentMethodID        string    `json:"backup_payment_method_id"`
    CardLast4      string `json:"card_last_4"`
	CardBrand      string`json:"card_brand"`
    
    // Amount is used when Action is ActionImmediate. 
    // If Action is ActionRenewal, the worker should use the PlanToBill price.
    Amount                 float64   `json:"amount"`       
    IsProrated             bool      `json:"is_prorated"`
    IsFreeTrial            bool      `json:"is_free_trial"`
    FreeTrialEnded            bool      `json:"free_trial_ended"`
    
    // Period details for logging and billing transparency
    BillingPeriodStart     time.Time `json:"billing_period_start"`
    BillingPeriodEnd       time.Time `json:"billing_period_end"`
    NextBillingDate        string    `json:"next_billing_date"`
    DueDate        string    `json:"due_date"`
    RefundIDs      []string  `json:"refund_ids"`

}

type RecordingTask struct {
    ID                    int    `json:"id"`
    WorkspaceID           int    `json:"workspace_id"`
    Status                string `json:"status"`
    StorageID             string `json:"storage_id"`
    StorageServerIP       string `json:"storage_server_ip"`
    Trim                  string `json:"trim"`
    CreateAISummary       bool   `json:"trim"`
    GenerateCallAnalytics bool   `json:"generate_call_analytics"`
    RelocationAttempts    int    `json:"relocation_attempts"`
}

type FailedBillingTask struct {
    RunID          string `json:"run_id"`
    WorkspaceID    int    `json:"workspace_id"`
    SubscriptionID int    `json:"subscription_id"`
    CreatorID      int    `json:"creator_id"`
    Reason         string `json:"reason"`
    PaymentType    string `json:"payment_type"`
    CardLast4      string `json:"card_last_4"`
    CardBrand      string `json:"card_brand"`
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

type SuspensionTask struct {
    ID                   int64     `json:"id"`
    WorkspaceID          int       `json:"workspace_id"`
    InvoiceID          int       `json:"invoice_id"`
    SuspensionInitiatedAt          time.Time `json:"suspension_initiated_at"`
    GracePeriodExtension *int      `json:"grace_period_extension"`
    Reason               string    `json:"reason"`
    Status               string     `json:"status"`
    IsFollowUp           bool      `json:"is_follow_up"`
}

type WorkspaceUpgradeTask struct {
    RunID                  string    `json:"run_id"`
    WorkspaceID            int       `json:"workspace_id"`
    CreatorID            int       `json:"creator_id"`
    SubscriptionID         int       `json:"subscription_id"`
    UpgradeFee            int       `json:"upgrade_fee"`
    PaymentMethodID        string    `json:"payment_method_id"`
    CardID        int    `json:"card_id"`
    CardLast4      string `json:"card_last_4"`
	CardBrand      string`json:"card_brand"`
    CurrentPlan            int       `json:"current_plan"`
    ScheduledPlan            int       `json:"scheduled_plan"`
    ScheduledEffectiveDate string    `json:"scheduled_effective_date"`
}

type WorkspaceUpgradeResultTask struct {
	RunID          string `json:"run_id"`
	WorkspaceID    int    `json:"workspace_id"`
	SubscriptionID int    `json:"subscription_id"`
	CreatorID      int    `json:"creator_id"`
	PlanName       string `json:"plan_name"`
	PlanID         int    `json:"plan_id"`
	Action         string `json:"action"`
	Timestamp      int    `json:"timestamp"`
}

type AlertMessageTask struct {
	RunID       string `json:"run_id"`
	WorkspaceID int    `json:"workspace_id"`
	Action      string `json:"action"`
}

type CallFraudTask struct {
    WorkspaceID              int       `json:"workspace_id"`
    StartDatetimeOfFraudCheck time.Time `json:"start_datetime_of_fraud_check"`
    AccountRiskLevel         string    `json:"account_risk_level"`
}


