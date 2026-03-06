package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"
	"lineblocs.com/scheduler/internal/activity"
	"lineblocs.com/scheduler/models"
	"lineblocs.com/scheduler/repository"

	"github.com/jmoiron/sqlx"
)

// AnnualActivity handles annual billing.
// It reuses the same CostCalculator with PeriodMultiplier=12.
type AnnualActivity struct {
	monthly *MonthlyActivity
}

// NewAnnualActivity creates a new AnnualActivity.
func NewAnnualActivity(
	db *sqlx.DB,
	costCalc *CostCalculator,
	invoiceSvc *InvoiceService,
	chargeSvc *ChargeService,
	wsRepo *repository.WorkspaceRepo,
	subRepo *repository.SubscriptionRepo,
	payRepo *repository.PaymentRepo,
	log *zap.Logger,
) *AnnualActivity {
	return &AnnualActivity{
		monthly: NewMonthlyActivity(db, costCalc, invoiceSvc, chargeSvc, wsRepo, subRepo, payRepo, log),
	}
}

func (a *AnnualActivity) Name() string { return "billing.annual" }

func (a *AnnualActivity) Retry() activity.RetryPolicy {
	return activity.RetryPolicy{
		MaxAttempts:     3,
		InitialInterval: 30 * time.Second,
		MaxInterval:     10 * time.Minute,
		BackoffFactor:   2.0,
	}
}

func (a *AnnualActivity) Timeout() time.Duration { return 10 * time.Minute }

func (a *AnnualActivity) Execute(ctx context.Context, input []byte) ([]byte, error) {
	var task models.BillingTask
	if err := json.Unmarshal(input, &task); err != nil {
		return nil, fmt.Errorf("annual_activity: unmarshal: %w", err)
	}

	log := a.monthly.log.With(
		zap.Int("workspace_id", task.WorkspaceID),
		zap.String("run_id", task.RunID),
		zap.String("type", "annual"),
	)

	return a.monthly.process(ctx, task, log, 12)
}
