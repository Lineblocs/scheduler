package repository

import (
	"context"
	"fmt"
	"time"

	helpers "github.com/Lineblocs/go-helpers"
	"lineblocs.com/scheduler/internal/db"
	"lineblocs.com/scheduler/models"
)

// SubscriptionRepo provides database access for subscriptions.
type SubscriptionRepo struct {
	db db.DBTX
}

// NewSubscriptionRepo creates a new SubscriptionRepo.
func NewSubscriptionRepo(dbtx db.DBTX) *SubscriptionRepo {
	return &SubscriptionRepo{db: dbtx}
}

// WithTx returns a copy using the given transaction.
func (r *SubscriptionRepo) WithTx(tx db.DBTX) *SubscriptionRepo {
	return &SubscriptionRepo{db: tx}
}

// GetByID returns a subscription by ID.
func (r *SubscriptionRepo) GetByID(ctx context.Context, id int) (*models.SubscriptionRow, error) {
	var sub models.SubscriptionRow
	err := r.db.GetContext(ctx, &sub,
		`SELECT id, workspace_id, status, billing_cycle, current_plan_id,
		scheduled_plan_id, scheduled_effective_date, provider_subscription_id,
		next_billing_date, last_billed_at, created_at, updated_at
		FROM subscriptions WHERE id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("subscription_repo: get by id: %w", err)
	}
	return &sub, nil
}

// GetActive returns all active subscriptions for a billing cycle that are due.
func (r *SubscriptionRepo) GetActive(ctx context.Context, billingCycle string) ([]models.SubscriptionRow, error) {
	var subs []models.SubscriptionRow
	err := r.db.SelectContext(ctx, &subs,
		`SELECT s.id, s.workspace_id, s.status, s.billing_cycle, s.current_plan_id,
		s.scheduled_plan_id, s.scheduled_effective_date, s.provider_subscription_id,
		s.next_billing_date, s.last_billed_at, s.created_at, s.updated_at
		FROM subscriptions s
		JOIN workspaces w ON s.workspace_id = w.id
		WHERE s.status = 'ACTIVE'
		AND s.billing_cycle = ?
		AND (s.next_billing_date IS NULL OR s.next_billing_date <= NOW())`, billingCycle)
	if err != nil {
		return nil, fmt.Errorf("subscription_repo: get active: %w", err)
	}
	return subs, nil
}

// UpdateNextBillingDate sets the next billing date and updates last_billed_at.
func (r *SubscriptionRepo) UpdateNextBillingDate(ctx context.Context, id int, nextDate time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE subscriptions SET next_billing_date = ?, last_billed_at = NOW(), updated_at = NOW() WHERE id = ?`,
		nextDate, id)
	if err != nil {
		return fmt.Errorf("subscription_repo: update next billing date: %w", err)
	}
	return nil
}

// GetServicePlans delegates to helpers for backward compatibility.
func (r *SubscriptionRepo) GetServicePlans() ([]helpers.ServicePlan, error) {
	return helpers.GetServicePlans2()
}

// GetSubscription delegates to helpers for backward compatibility.
func (r *SubscriptionRepo) GetSubscription(subID int) (*helpers.Subscription, error) {
	return helpers.GetSubscriptionFromDB(subID)
}
