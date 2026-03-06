package repository

import (
	"context"
	"fmt"

	"lineblocs.com/scheduler/internal/db"
	"lineblocs.com/scheduler/models"
)

// PaymentRepo provides database access for payment-related queries.
type PaymentRepo struct {
	db db.DBTX
}

// NewPaymentRepo creates a new PaymentRepo.
func NewPaymentRepo(dbtx db.DBTX) *PaymentRepo {
	return &PaymentRepo{db: dbtx}
}

// WithTx returns a copy using the given transaction.
func (r *PaymentRepo) WithTx(tx db.DBTX) *PaymentRepo {
	return &PaymentRepo{db: tx}
}

// GetBillingParams returns the payment gateway and stripe key from the database.
func (r *PaymentRepo) GetBillingParams(ctx context.Context) (provider string, stripeKey string, err error) {
	err = r.db.GetContext(ctx, &provider, "SELECT payment_gateway FROM customizations")
	if err != nil {
		return "", "", fmt.Errorf("payment_repo: get payment gateway: %w", err)
	}

	err = r.db.GetContext(ctx, &stripeKey, "SELECT stripe_private_key FROM api_credentials")
	if err != nil {
		return "", "", fmt.Errorf("payment_repo: get stripe key: %w", err)
	}

	return provider, stripeKey, nil
}

// GetFaxesForPeriod returns faxes for a workspace in a billing period.
func (r *PaymentRepo) GetFaxesForPeriod(ctx context.Context, workspaceID int, start, end string) ([]models.FaxRow, error) {
	var faxes []models.FaxRow
	err := r.db.SelectContext(ctx, &faxes,
		"SELECT id, created_at FROM faxes WHERE workspace_id = ? AND created_at BETWEEN ? AND ?",
		workspaceID, start, end)
	if err != nil {
		return nil, fmt.Errorf("payment_repo: get faxes: %w", err)
	}
	return faxes, nil
}
