package repository

import (
	"context"
	"fmt"

	"lineblocs.com/scheduler/internal/db"
	"lineblocs.com/scheduler/models"
)

// DebitRepo provides database access for user debits.
type DebitRepo struct {
	db db.DBTX
}

// NewDebitRepo creates a new DebitRepo.
func NewDebitRepo(dbtx db.DBTX) *DebitRepo {
	return &DebitRepo{db: dbtx}
}

// WithTx returns a copy using the given transaction.
func (r *DebitRepo) WithTx(tx db.DBTX) *DebitRepo {
	return &DebitRepo{db: tx}
}

// GetForPeriod returns all debits for a user within a billing period.
func (r *DebitRepo) GetForPeriod(ctx context.Context, userID int, start, end string) ([]models.DebitRow, error) {
	var debits []models.DebitRow
	err := r.db.SelectContext(ctx, &debits,
		"SELECT id, source, module_id, cents, user_id, workspace_id, created_at FROM users_debits WHERE user_id = ? AND created_at BETWEEN ? AND ?",
		userID, start, end)
	if err != nil {
		return nil, fmt.Errorf("debit_repo: get for period: %w", err)
	}
	return debits, nil
}

// CreateNumberRentalDebit creates number rental debits for all DIDs in a workspace.
// Fixes the defer-in-loop issue from utils.go by using ExecContext directly.
func (r *DebitRepo) CreateNumberRentalDebit(ctx context.Context, workspaceID, userID int, start string) error {
	var dids []models.DIDRow
	err := r.db.SelectContext(ctx, &dids,
		"SELECT id, monthly_cost FROM did_numbers WHERE workspace_id = ?", workspaceID)
	if err != nil {
		return fmt.Errorf("debit_repo: query dids: %w", err)
	}

	for _, did := range dids {
		_, err := r.db.ExecContext(ctx,
			`INSERT INTO users_debits (source, status, cents, module_id, user_id, workspace_id, created_at)
			VALUES ('NUMBER_RENTAL', 'INCOMPLETE', ?, ?, ?, ?, ?)`,
			did.MonthlyCost, did.ID, userID, workspaceID, start)
		if err != nil {
			return fmt.Errorf("debit_repo: create rental debit for DID %d: %w", did.ID, err)
		}
	}

	return nil
}
