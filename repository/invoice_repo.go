package repository

import (
	"context"
	"fmt"

	"lineblocs.com/scheduler/internal/db"
	"lineblocs.com/scheduler/models"
)

// InvoiceRepo provides database access for invoices.
type InvoiceRepo struct {
	db db.DBTX
}

// NewInvoiceRepo creates a new InvoiceRepo.
func NewInvoiceRepo(dbtx db.DBTX) *InvoiceRepo {
	return &InvoiceRepo{db: dbtx}
}

// WithTx returns a copy using the given transaction.
func (r *InvoiceRepo) WithTx(tx db.DBTX) *InvoiceRepo {
	return &InvoiceRepo{db: tx}
}

// Create inserts a new invoice and returns its ID.
func (r *InvoiceRepo) Create(ctx context.Context, p *models.InvoiceCreateParams) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		`INSERT INTO users_invoices (cents, cents_including_taxes, call_costs, recording_costs, fax_costs,
		membership_costs, number_costs, status, user_id, workspace_id, created_at, updated_at, source, tax_metadata)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Cents, p.CentsIncludingTax, p.CallCosts, p.RecordingCosts, p.FaxCosts,
		p.MembershipCosts, p.NumberCosts, p.Status, p.UserID, p.WorkspaceID,
		p.Now, p.Now, p.Source, p.TaxMetadata)
	if err != nil {
		return 0, fmt.Errorf("invoice_repo: create: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("invoice_repo: last insert id: %w", err)
	}
	return id, nil
}

// MarkComplete marks an invoice as COMPLETE with card payment.
func (r *InvoiceRepo) MarkComplete(ctx context.Context, id int64, centsCollected int64, confirmationNumber string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE users_invoices SET status = 'COMPLETE', source = 'CARD', cents_collected = ?, confirmation_number = ? WHERE id = ?`,
		centsCollected, confirmationNumber, id)
	if err != nil {
		return fmt.Errorf("invoice_repo: mark complete: %w", err)
	}
	return nil
}

// MarkCompleteCredits marks an invoice as COMPLETE with credits payment.
func (r *InvoiceRepo) MarkCompleteCredits(ctx context.Context, id int64, centsCollected int64, confirmationNumber string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE users_invoices SET status = 'COMPLETE', source = 'CREDITS', cents_collected = ?, confirmation_number = ? WHERE id = ?`,
		centsCollected, confirmationNumber, id)
	if err != nil {
		return fmt.Errorf("invoice_repo: mark complete credits: %w", err)
	}
	return nil
}

// MarkIncomplete marks an invoice as INCOMPLETE.
func (r *InvoiceRepo) MarkIncomplete(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE users_invoices SET status = 'INCOMPLETE' WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("invoice_repo: mark incomplete: %w", err)
	}
	return nil
}

// MarkIncompleteCard marks an invoice as INCOMPLETE with card info and last attempt time.
func (r *InvoiceRepo) MarkIncompleteCard(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE users_invoices SET source = 'CARD', status = 'INCOMPLETE', num_attempts = num_attempts + 1, last_attempted = NOW() WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("invoice_repo: mark incomplete card: %w", err)
	}
	return nil
}

// GetIncomplete returns all incomplete invoices with their workspace info.
// Fixes the SQL syntax bug from the original retry_failed_billing_attempts.go
func (r *InvoiceRepo) GetIncomplete(ctx context.Context) ([]models.InvoiceRow, error) {
	var invoices []models.InvoiceRow
	err := r.db.SelectContext(ctx, &invoices,
		`SELECT ui.id, ui.cents, ui.workspace_id, ui.user_id, ui.status
		FROM users_invoices ui
		INNER JOIN workspaces w ON w.id = ui.workspace_id
		WHERE ui.status = 'INCOMPLETE'`)
	if err != nil {
		return nil, fmt.Errorf("invoice_repo: get incomplete: %w", err)
	}
	return invoices, nil
}
