package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	helpers "github.com/Lineblocs/go-helpers"
	"go.uber.org/zap"
	"lineblocs.com/scheduler/internal/activity"
	"lineblocs.com/scheduler/models"
	"lineblocs.com/scheduler/repository"
)

// ProratedActivity handles immediate prorated charges (signup/upgrade).
type ProratedActivity struct {
	invoiceSvc      *InvoiceService
	chargeSvc       *ChargeService
	workspaceRepo   *repository.WorkspaceRepo
	subscriptionRepo *repository.SubscriptionRepo
	paymentRepo     *repository.PaymentRepo
	log             *zap.Logger
}

// NewProratedActivity creates a new ProratedActivity.
func NewProratedActivity(
	invoiceSvc *InvoiceService,
	chargeSvc *ChargeService,
	wsRepo *repository.WorkspaceRepo,
	subRepo *repository.SubscriptionRepo,
	payRepo *repository.PaymentRepo,
	log *zap.Logger,
) *ProratedActivity {
	return &ProratedActivity{
		invoiceSvc:      invoiceSvc,
		chargeSvc:       chargeSvc,
		workspaceRepo:   wsRepo,
		subscriptionRepo: subRepo,
		paymentRepo:     payRepo,
		log:             log,
	}
}

func (a *ProratedActivity) Name() string { return "billing.prorated" }

func (a *ProratedActivity) Retry() activity.RetryPolicy {
	return activity.RetryPolicy{
		MaxAttempts:     2,
		InitialInterval: 5 * time.Second,
		MaxInterval:     1 * time.Minute,
		BackoffFactor:   2.0,
	}
}

func (a *ProratedActivity) Timeout() time.Duration { return 2 * time.Minute }

func (a *ProratedActivity) Execute(ctx context.Context, input []byte) ([]byte, error) {
	var task models.BillingTask
	if err := json.Unmarshal(input, &task); err != nil {
		return nil, fmt.Errorf("prorated_activity: unmarshal: %w", err)
	}

	log := a.log.With(
		zap.Int("workspace_id", task.WorkspaceID),
		zap.String("run_id", task.RunID),
	)

	workspace, err := a.workspaceRepo.GetWorkspaceFromDB(task.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("prorated_activity: get workspace: %w", err)
	}

	user, err := a.workspaceRepo.GetUserFromDB(task.CreatorID)
	if err != nil {
		return nil, fmt.Errorf("prorated_activity: get user: %w", err)
	}

	subscription, err := a.subscriptionRepo.GetSubscription(task.SubscriptionID)
	if err != nil {
		return nil, fmt.Errorf("prorated_activity: get subscription: %w", err)
	}

	plans, err := a.subscriptionRepo.GetServicePlans()
	if err != nil {
		return nil, fmt.Errorf("prorated_activity: get plans: %w", err)
	}

	var plan *helpers.ServicePlan
	for _, p := range plans {
		if p.Id == subscription.CurrentPlanId {
			plan = &p
			break
		}
	}
	if plan == nil {
		return nil, fmt.Errorf("prorated_activity: plan not found")
	}

	billingInfo, err := a.workspaceRepo.GetWorkspaceBillingInfo(workspace)
	if err != nil {
		return nil, fmt.Errorf("prorated_activity: get billing info: %w", err)
	}

	provider, stripeKey, err := a.paymentRepo.GetBillingParams(ctx)
	if err != nil {
		return nil, fmt.Errorf("prorated_activity: get billing params: %w", err)
	}

	now := time.Now()
	costs := &CostResult{
		MembershipCosts: int64(task.Amount * 100),
		TotalCosts:      int64(task.Amount * 100),
		InvoiceDesc:     fmt.Sprintf("Initial prorated charge for %s plan", task.BillingType),
	}

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
	if task.BillingType == "ANNUAL" {
		nextDate = time.Date(now.Year()+1, 1, 1, 0, 0, 0, 0, now.Location())
	} else {
		nextDate = time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, now.Location())
	}

	if err := a.subscriptionRepo.UpdateNextBillingDate(ctx, task.SubscriptionID, nextDate); err != nil {
		return nil, fmt.Errorf("prorated_activity: update anchor: %w", err)
	}

	log.Info("prorated billing complete", zap.Float64("amount", task.Amount))
	return nil, nil
}
