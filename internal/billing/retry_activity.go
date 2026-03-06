package billing

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
	"lineblocs.com/scheduler/internal/activity"
	"lineblocs.com/scheduler/repository"
)

// RetryActivity retries failed billing attempts.
type RetryActivity struct {
	invoiceRepo *repository.InvoiceRepo
	workspaceRepo *repository.WorkspaceRepo
	paymentRepo *repository.PaymentRepo
	chargeSvc   *ChargeService
	log         *zap.Logger
}

// NewRetryActivity creates a new RetryActivity.
func NewRetryActivity(
	invRepo *repository.InvoiceRepo,
	wsRepo *repository.WorkspaceRepo,
	payRepo *repository.PaymentRepo,
	chargeSvc *ChargeService,
	log *zap.Logger,
) *RetryActivity {
	return &RetryActivity{
		invoiceRepo:   invRepo,
		workspaceRepo: wsRepo,
		paymentRepo:   payRepo,
		chargeSvc:     chargeSvc,
		log:           log,
	}
}

func (a *RetryActivity) Name() string { return "billing.retry" }

func (a *RetryActivity) Retry() activity.RetryPolicy {
	return activity.RetryPolicy{
		MaxAttempts:     2,
		InitialInterval: 30 * time.Second,
		MaxInterval:     5 * time.Minute,
		BackoffFactor:   2.0,
	}
}

func (a *RetryActivity) Timeout() time.Duration { return 10 * time.Minute }

func (a *RetryActivity) Execute(ctx context.Context, _ []byte) ([]byte, error) {
	invoices, err := a.invoiceRepo.GetIncomplete(ctx)
	if err != nil {
		return nil, fmt.Errorf("retry_activity: get incomplete: %w", err)
	}

	a.log.Info("retrying failed invoices", zap.Int("count", len(invoices)))

	for _, inv := range invoices {
		log := a.log.With(zap.Int64("invoice_id", inv.ID), zap.Int("workspace_id", inv.WorkspaceID))

		workspace, err := a.workspaceRepo.GetWorkspaceFromDB(inv.WorkspaceID)
		if err != nil {
			log.Error("error getting workspace", zap.Error(err))
			continue
		}

		user, err := a.workspaceRepo.GetUserFromDB(inv.UserID)
		if err != nil {
			log.Error("error getting user", zap.Error(err))
			continue
		}

		provider, stripeKey, err := a.paymentRepo.GetBillingParams(ctx)
		if err != nil {
			log.Error("error getting billing params", zap.Error(err))
			continue
		}

		chargeFunc := a.chargeSvc.MakeChargeFunc(provider, stripeKey, 0, user, workspace, nil)
		result, err := chargeFunc(inv.ID, int(inv.Cents), "Invoice for service")
		if err != nil {
			log.Error("retry charge failed", zap.Error(err))
			if markErr := a.invoiceRepo.MarkIncompleteCard(ctx, inv.ID); markErr != nil {
				log.Error("failed to mark invoice incomplete", zap.Error(markErr))
			}
			continue
		}

		confNumber, err := createInvoiceConfirmationNumber()
		if err != nil {
			log.Error("error generating confirmation", zap.Error(err))
			continue
		}

		if err := a.invoiceRepo.MarkComplete(ctx, inv.ID, result.Amount, confNumber); err != nil {
			log.Error("error marking invoice complete", zap.Error(err))
			continue
		}

		log.Info("invoice retry succeeded", zap.Int64("amount", result.Amount))
	}

	return nil, nil
}
