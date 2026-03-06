package models

import (
	"database/sql"
	"time"
)

// InvoiceRow represents a row in users_invoices.
type InvoiceRow struct {
	ID                 int64          `db:"id"`
	Cents              int64          `db:"cents"`
	CentsIncludingTax  int64          `db:"cents_including_taxes"`
	CentsCollected     int64          `db:"cents_collected"`
	CallCosts          int64          `db:"call_costs"`
	RecordingCosts     int64          `db:"recording_costs"`
	FaxCosts           int64          `db:"fax_costs"`
	MembershipCosts    int64          `db:"membership_costs"`
	NumberCosts        int64          `db:"number_costs"`
	Status             string         `db:"status"`
	Source             string         `db:"source"`
	UserID             int            `db:"user_id"`
	WorkspaceID        int            `db:"workspace_id"`
	ConfirmationNumber sql.NullString `db:"confirmation_number"`
	TaxMetadata        sql.NullString `db:"tax_metadata"`
	NumAttempts        int            `db:"num_attempts"`
	LastAttempted      sql.NullTime   `db:"last_attempted"`
	CreatedAt          time.Time      `db:"created_at"`
	UpdatedAt          time.Time      `db:"updated_at"`
}

// InvoiceCreateParams holds parameters for creating an invoice.
type InvoiceCreateParams struct {
	Cents              int64
	CentsIncludingTax  int64
	CallCosts          int64
	RecordingCosts     int64
	FaxCosts           int64
	MembershipCosts    int64
	NumberCosts        int64
	Status             string
	Source             string
	UserID             int
	WorkspaceID        int
	TaxMetadata        string
	Now                time.Time
}
