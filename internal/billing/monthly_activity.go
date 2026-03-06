package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	helpers "github.com/Lineblocs/go-helpers"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"lineblocs.com/scheduler/internal/activity"
	"lineblocs.com/scheduler/models"
	"lineblocs.com/scheduler/repository"
)

// MonthlyActivity handles monthly billing.
type MonthlyActivity struct {
	db              *sqlx.DB
	costCalc        *CostCalculator
	invoiceSvc      *InvoiceService
	chargeSvc       *ChargeService
	workspaceRepo   *repository.WorkspaceRepo
	subscriptionRepo *repository.SubscriptionRepo
	paymentRepo     *repository.PaymentRepo
	log             *zap.Logger
}

// NewMonthlyActivity creates a new MonthlyActivity.
func NewMonthlyActivity(
	db *sqlx.DB,
	costCalc *CostCalculator,
	invoiceSvc *InvoiceService,
	chargeSvc *ChargeService,
	wsRepo *repository.WorkspaceRepo,
	subRepo *repository.SubscriptionRepo,
	payRepo *repository.PaymentRepo,
	log *zap.Logger,
) *MonthlyActivity {
	return &MonthlyActivity{
		db:              db,
		costCalc:        costCalc,
		invoiceSvc:      invoiceSvc,
		chargeSvc:       chargeSvc,
		workspaceRepo:   wsRepo,
		subscriptionRepo: subRepo,
		paymentRepo:     payRepo,
		log:             log,
	}
}

func (a *MonthlyActivity) Name() string { return "billing.monthly" }

func (a *MonthlyActivity) Retry() activity.RetryPolicy {
	return activity.RetryPolicy{
		MaxAttempts:     3,
		InitialInterval: 10 * time.Second,
		MaxInterval:     5 * time.Minute,
		BackoffFactor:   2.0,
	}
}

func (a *MonthlyActivity) Timeout() time.Duration { return 5 * time.Minute }

func (a *MonthlyActivity) Execute(ctx context.Context, input []byte) ([]byte, error) {
	var task models.BillingTask
	if err := json.Unmarshal(input, &task); err != nil {
		return nil, fmt.Errorf("monthly_activity: unmarshal: %w", err)
	}

	log := a.log.With(
		zap.Int("workspace_id", task.WorkspaceID),
		zap.String("run_id", task.RunID),
	)

	return a.process(ctx, task, log, 1)
}

func (a *MonthlyActivity) process(ctx context.Context, task models.BillingTask, log *zap.Logger, periodMultiplier int) ([]byte, error) {
	// Load billing data
	subscription, err := a.subscriptionRepo.GetSubscription(task.SubscriptionID)
	if err != nil {
		return nil, fmt.Errorf("monthly_activity: get subscription: %w", err)
	}

	provider, stripeKey, err := a.paymentRepo.GetBillingParams(ctx)
	if err != nil {
		return nil, fmt.Errorf("monthly_activity: get billing params: %w", err)
	}

	now := time.Now()
	var periodStart time.Time
	if periodMultiplier == 1 {
		periodStart = now.AddDate(0, -1, 0)
	} else {
		periodStart = now.AddDate(-1, 0, 0)
	}

	workspace, err := a.workspaceRepo.GetWorkspaceFromDB(task.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("monthly_activity: get workspace: %w", err)
	}

	user, err := a.workspaceRepo.GetUserFromDB(task.CreatorID)
	if err != nil {
		return nil, fmt.Errorf("monthly_activity: get user: %w", err)
	}

	plans, err := a.subscriptionRepo.GetServicePlans()
	if err != nil {
		return nil, fmt.Errorf("monthly_activity: get plans: %w", err)
	}

	var plan *helpers.ServicePlan
	for _, p := range plans {
		if p.Id == subscription.CurrentPlanId {
			plan = &p
			break
		}
	}
	if plan == nil {
		return nil, fmt.Errorf("monthly_activity: plan not found for subscription %d", task.SubscriptionID)
	}

	billingInfo, err := a.workspaceRepo.GetWorkspaceBillingInfo(workspace)
	if err != nil {
		return nil, fmt.Errorf("monthly_activity: get billing info: %w", err)
	}

	baseCosts, err := helpers.GetBaseCosts()
	if err != nil {
		return nil, fmt.Errorf("monthly_activity: get base costs: %w", err)
	}

	// Calculate costs
	costInput := &CostInput{
		WorkspaceID:      task.WorkspaceID,
		CreatorID:        task.CreatorID,
		Plan:             plan,
		BaseCosts:        baseCosts,
		BillingInfo:      billingInfo,
		PeriodMultiplier: periodMultiplier,
		PeriodStart:      periodStart,
		PeriodEnd:        now,
	}

	costs, err := a.costCalc.Calculate(ctx, costInput)
	if err != nil {
		return nil, fmt.Errorf("monthly_activity: calculate costs: %w", err)
	}

	// Create and charge invoice
	chargeFunc := a.chargeSvc.MakeChargeFunc(provider, stripeKey, 0, user, workspace, nil)

	_, err = a.invoiceSvc.CreateAndCharge(ctx, &CreateAndChargeInput{
		Costs:       costs,
		Workspace:   workspace,
		User:        user,
		BillingInfo: billingInfo,
		Plan:        plan,
		ChargeFunc:  chargeFunc,
		Now:         now,
	})
	if err != nil {
		return nil, err
	}

	// Update subscription anchor
	var nextDate time.Time
	if periodMultiplier == 1 {
		nextDate = time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, now.Location())
	} else {
		nextDate = time.Date(now.Year()+1, 1, 1, 0, 0, 0, 0, now.Location())
	}

	if err := a.subscriptionRepo.UpdateNextBillingDate(ctx, task.SubscriptionID, nextDate); err != nil {
		return nil, fmt.Errorf("monthly_activity: update anchor: %w", err)
	}

	log.Info("billing complete", zap.Int64("total_cents", costs.TotalCosts))
	return nil, nil
}
