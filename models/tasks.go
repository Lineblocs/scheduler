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
    RunID               string            `json:"run_id"`
    WorkspaceID         int               `json:"workspace_id"`
    CreatorID           int               `json:"creator_id"`
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
    RunID               string            `json:"run_id"`
    WorkspaceID         int               `json:"workspace_id"`
    CreatorID           int               `json:"creator_id"`
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