package billing

import (
	"context"
	"fmt"
	"math"
	"time"

	helpers "github.com/Lineblocs/go-helpers"
	"go.uber.org/zap"
	"lineblocs.com/scheduler/repository"
)

// CostResult holds the calculated billing costs.
type CostResult struct {
	MembershipCosts   int64
	CallTollsCosts    int64
	RecordingCosts    int64
	FaxCosts          int64
	NumberRentalCosts int64
	TotalCosts        int64
	InvoiceDesc       string
}

// CostCalculator computes billing costs for a workspace.
// It handles both monthly and annual billing via PeriodMultiplier.
type CostCalculator struct {
	workspaceRepo *repository.WorkspaceRepo
	debitRepo     *repository.DebitRepo
	recordingRepo *repository.RecordingRepo
	paymentRepo   *repository.PaymentRepo
	log           *zap.Logger
}

// NewCostCalculator creates a new CostCalculator.
func NewCostCalculator(
	wsRepo *repository.WorkspaceRepo,
	debitRepo *repository.DebitRepo,
	recRepo *repository.RecordingRepo,
	payRepo *repository.PaymentRepo,
	log *zap.Logger,
) *CostCalculator {
	return &CostCalculator{
		workspaceRepo: wsRepo,
		debitRepo:     debitRepo,
		recordingRepo: recRepo,
		paymentRepo:   payRepo,
		log:           log,
	}
}

// CostInput provides the data needed to calculate costs.
type CostInput struct {
	WorkspaceID      int
	CreatorID        int
	Plan             *helpers.ServicePlan
	BaseCosts        *helpers.BaseCosts
	BillingInfo      *helpers.WorkspaceBillingInfo
	PeriodMultiplier int // 1 for monthly, 12 for annual
	PeriodStart      time.Time
	PeriodEnd        time.Time
}

// Calculate computes total costs for a billing period.
// PeriodMultiplier=1 for monthly, PeriodMultiplier=12 for annual.
func (c *CostCalculator) Calculate(ctx context.Context, input *CostInput) (*CostResult, error) {
	result := &CostResult{}

	userCount, err := c.workspaceRepo.GetUserCount(ctx, input.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("cost_calculator: get user count: %w", err)
	}

	c.log.Info("workspace user count", zap.Int("count", userCount), zap.Int("workspace_id", input.WorkspaceID))

	// Membership costs: base cost * user count * period multiplier
	result.MembershipCosts = int64(input.Plan.BaseCosts * float64(userCount) * float64(input.PeriodMultiplier))

	// Create number rental debits (only for monthly - annual accumulates from monthly)
	if input.PeriodMultiplier == 1 {
		if err := c.debitRepo.CreateNumberRentalDebit(ctx, input.WorkspaceID, input.CreatorID, input.PeriodStart.Format(time.DateTime)); err != nil {
			c.log.Warn("failed to create number rental debits", zap.Error(err))
		}
	}

	startStr := input.PeriodStart.Format(time.DateTime)
	endStr := input.PeriodEnd.Format(time.DateTime)

	// Process debits (calls + number rentals)
	if err := c.processDebits(ctx, input, result, startStr, endStr); err != nil {
		return nil, err
	}

	// Process recordings
	if err := c.processRecordings(ctx, input, result, startStr, endStr); err != nil {
		return nil, err
	}

	// Process faxes
	if err := c.processFaxes(ctx, input, result, startStr, endStr); err != nil {
		return nil, err
	}

	result.TotalCosts = result.MembershipCosts + result.CallTollsCosts + result.RecordingCosts + result.FaxCosts + result.NumberRentalCosts

	periodLabel := "monthly"
	if input.PeriodMultiplier > 1 {
		periodLabel = "annual"
	}
	result.InvoiceDesc = fmt.Sprintf("LineBlocs %s invoice for %s", periodLabel, input.BillingInfo.InvoiceDue)

	c.log.Info("billing costs calculated",
		zap.Int64("membership", result.MembershipCosts),
		zap.Int64("call_tolls", result.CallTollsCosts),
		zap.Int64("recordings", result.RecordingCosts),
		zap.Int64("fax", result.FaxCosts),
		zap.Int64("did_rentals", result.NumberRentalCosts),
		zap.Int64("total", result.TotalCosts))

	return result, nil
}

func (c *CostCalculator) processDebits(ctx context.Context, input *CostInput, result *CostResult, startStr, endStr string) error {
	debits, err := c.debitRepo.GetForPeriod(ctx, input.CreatorID, startStr, endStr)
	if err != nil {
		return fmt.Errorf("cost_calculator: get debits: %w", err)
	}

	remainingMinutes := input.Plan.MinutesPerMonth * float64(input.PeriodMultiplier)

	for _, debit := range debits {
		switch debit.Source {
		case "CALL":
			call, err := c.workspaceRepo.GetCall(ctx, debit.ModuleID)
			if err != nil {
				c.log.Warn("error getting call", zap.Int("module_id", debit.ModuleID), zap.Error(err))
				continue
			}
			callMinutes := float64(call.DurationNumber) / 60.0
			charge, err := computeAmountToCharge(float64(debit.Cents), remainingMinutes, callMinutes)
			if err != nil {
				c.log.Warn("error computing call charge", zap.Error(err))
				continue
			}
			result.CallTollsCosts += int64(charge)
			remainingMinutes -= callMinutes

		case "NUMBER_RENTAL":
			did, err := c.workspaceRepo.GetDID(ctx, debit.ModuleID)
			if err != nil {
				c.log.Warn("error getting DID", zap.Int("module_id", debit.ModuleID), zap.Error(err))
				continue
			}
			result.NumberRentalCosts += int64(did.MonthlyCost)
		}
	}

	return nil
}

func (c *CostCalculator) processRecordings(ctx context.Context, input *CostInput, result *CostResult, startStr, endStr string) error {
	recordings, err := c.recordingRepo.GetForPeriod(ctx, input.CreatorID, startStr, endStr)
	if err != nil {
		return fmt.Errorf("cost_calculator: get recordings: %w", err)
	}

	remainingRecordings := input.Plan.RecordingSpace * float64(input.PeriodMultiplier)

	for _, rec := range recordings {
		centsPerByte := int64(math.Round(input.BaseCosts.RecordingsPerByte * rec.Size))
		charge, err := computeAmountToCharge(float64(centsPerByte), remainingRecordings, rec.Size)
		if err != nil {
			c.log.Warn("error computing recording charge", zap.Error(err))
			continue
		}
		result.RecordingCosts += int64(charge)
		remainingRecordings -= rec.Size
	}

	return nil
}

func (c *CostCalculator) processFaxes(ctx context.Context, input *CostInput, result *CostResult, startStr, endStr string) error {
	faxes, err := c.paymentRepo.GetFaxesForPeriod(ctx, input.WorkspaceID, startStr, endStr)
	if err != nil {
		return fmt.Errorf("cost_calculator: get faxes: %w", err)
	}

	remainingFaxUnits := input.Plan.Fax * input.PeriodMultiplier

	for range faxes {
		charge, err := computeAmountToCharge(input.BaseCosts.FaxPerUsed, float64(remainingFaxUnits), float64(input.Plan.Fax*input.PeriodMultiplier))
		if err != nil {
			c.log.Warn("error computing fax charge", zap.Error(err))
			continue
		}
		result.FaxCosts += int64(charge)
		remainingFaxUnits--
	}

	return nil
}

// computeAmountToCharge determines the charge based on available resources.
// Moved from utils to billing package for cohesion.
func computeAmountToCharge(fullCentsToCharge float64, available float64, used float64) (float64, error) {
	remaining := available - used
	if available > 0 && remaining < 0 && available <= used {
		ratio := (used - available) / used
		centsToCharge := math.Abs(fullCentsToCharge * ratio)
		return math.Max(1, centsToCharge), nil
	} else if available >= used {
		return 0, nil
	} else if available <= 0 {
		return fullCentsToCharge, nil
	}
	return 0, fmt.Errorf("billing: computeAmountToCharge logic failure (avail=%f, used=%f)", available, used)
}
