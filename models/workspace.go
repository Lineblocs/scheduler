package models

import (
	"database/sql"
	"time"
)

// DIDRow represents a DID number from the database.
type DIDRow struct {
	ID          int `db:"id"`
	MonthlyCost int `db:"monthly_cost"`
	WorkspaceID int `db:"workspace_id"`
}

// CallRow represents a call record from the database.
type CallRow struct {
	ID             int `db:"id"`
	DurationNumber int `db:"duration"`
}

// SubscriptionRow represents a subscription from the database.
type SubscriptionRow struct {
	ID                     int            `db:"id"`
	WorkspaceID            int            `db:"workspace_id"`
	Status                 string         `db:"status"`
	BillingCycle           string         `db:"billing_cycle"`
	CurrentPlanID          int            `db:"current_plan_id"`
	ScheduledPlanID        sql.NullInt64  `db:"scheduled_plan_id"`
	ScheduledEffectiveDate sql.NullTime   `db:"scheduled_effective_date"`
	ProviderSubscriptionID sql.NullString `db:"provider_subscription_id"`
	NextBillingDate        sql.NullTime   `db:"next_billing_date"`
	LastBilledAt           sql.NullTime   `db:"last_billed_at"`
	CreatedAt              time.Time      `db:"created_at"`
	UpdatedAt              time.Time      `db:"updated_at"`
}

// DebitRow represents a user debit from the database.
type DebitRow struct {
	ID          int       `db:"id"`
	Source      string    `db:"source"`
	ModuleID    int       `db:"module_id"`
	Cents       int64     `db:"cents"`
	UserID      int       `db:"user_id"`
	WorkspaceID int       `db:"workspace_id"`
	CreatedAt   time.Time `db:"created_at"`
}

// RecordingRow represents a recording from the database.
type RecordingRow struct {
	ID              int            `db:"id"`
	Status          string         `db:"status"`
	StorageID       string         `db:"storage_id"`
	StorageServerIP string         `db:"storage_server_ip"`
	Trim            sql.NullString `db:"trim"`
	Size            float64        `db:"size"`
	UserID          int            `db:"user_id"`
	S3URL           sql.NullString `db:"s3_url"`
	CreatedAt       time.Time      `db:"created_at"`
}

// FaxRow represents a fax record from the database.
type FaxRow struct {
	ID          int       `db:"id"`
	WorkspaceID int       `db:"workspace_id"`
	CreatedAt   time.Time `db:"created_at"`
}

