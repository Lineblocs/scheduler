package billing

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math"
	"time"

	helpers "github.com/Lineblocs/go-helpers"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"lineblocs.com/scheduler/internal/db"
	"lineblocs.com/scheduler/models"
	"lineblocs.com/scheduler/repository"
)

// InvoiceService handles invoice creation and charging.
type InvoiceService struct {
	db          *sqlx.DB
	invoiceRepo *repository.InvoiceRepo
	log         *zap.Logger
}

// NewInvoiceService creates a new InvoiceService.
func NewInvoiceService(database *sqlx.DB, invRepo *repository.InvoiceRepo, log *zap.Logger) *InvoiceService {
	return &InvoiceService{
		db:          database,
		invoiceRepo: invRepo,
		log:         log,
	}
}

// CreateAndChargeInput provides data needed to create and charge an invoice.
type CreateAndChargeInput struct {
	Costs       *CostResult
	Workspace   *helpers.Workspace
	User        *helpers.User
	BillingInfo *helpers.WorkspaceBillingInfo
	Plan        *helpers.ServicePlan
	ChargeFunc  ChargeFunc
	Now         time.Time
}

// ChargeFunc is the function that charges a card.
type ChargeFunc func(invoiceID int64, cents int, desc string) (*ChargeResult, error)

// ChargeResult contains the details of a successful charge.
type ChargeResult struct {
	PaymentIntentID string
	PaymentMethodID string
	Amount          int64
	Currency        string
	Status          string
	Created         int64
	CardBrand       string
	CardLast4       string
}

// CreateAndCharge creates an invoice and charges it atomically within a transaction.
func (s *InvoiceService) CreateAndCharge(ctx context.Context, input *CreateAndChargeInput) (int64, error) {
	var invoiceID int64

	err := db.RunInTx(ctx, s.db, nil, func(tx *sqlx.Tx) error {
		txInvRepo := s.invoiceRepo.WithTx(tx)

		taxMetadata := createTaxMetadata(
			input.Costs.CallTollsCosts,
			input.Costs.RecordingCosts,
			input.Costs.FaxCosts,
			input.Costs.MembershipCosts,
			input.Costs.NumberRentalCosts,
		)

		var taxes int64
		centsIncludingTax := input.Costs.TotalCosts + taxes

		params := &models.InvoiceCreateParams{
			Cents:             input.Costs.TotalCosts,
			CentsIncludingTax: centsIncludingTax,
			CallCosts:         input.Costs.CallTollsCosts,
			RecordingCosts:    input.Costs.RecordingCosts,
			FaxCosts:          input.Costs.FaxCosts,
			MembershipCosts:   input.Costs.MembershipCosts,
			NumberCosts:       input.Costs.NumberRentalCosts,
			Status:            "INCOMPLETE",
			Source:            "SUBSCRIPTION",
			UserID:            input.User.Id,
			WorkspaceID:       input.Workspace.Id,
			TaxMetadata:       taxMetadata,
			Now:               input.Now,
		}

		var err error
		invoiceID, err = txInvRepo.Create(ctx, params)
		if err != nil {
			return fmt.Errorf("invoice_service: create invoice: %w", err)
		}

		s.log.Info("invoice created",
			zap.Int64("invoice_id", invoiceID),
			zap.Int("user_id", input.User.Id),
			zap.Int("workspace_id", input.Workspace.Id))

		// Charge based on plan type
		if input.Plan.PayAsYouGo {
			return s.chargeWithCredits(ctx, txInvRepo, invoiceID, input)
		}
		return s.chargeWithCard(ctx, txInvRepo, invoiceID, input)
	})

	if err != nil {
		return 0, err
	}

	return invoiceID, nil
}

func (s *InvoiceService) chargeWithCredits(ctx context.Context, repo *repository.InvoiceRepo, invoiceID int64, input *CreateAndChargeInput) error {
	remainingBalance := input.BillingInfo.RemainingBalanceCents

	if remainingBalance >= int64(input.Costs.TotalCosts) {
		confNumber, err := createInvoiceConfirmationNumber()
		if err != nil {
			return fmt.Errorf("invoice_service: generate confirmation: %w", err)
		}

		return repo.MarkCompleteCredits(ctx, invoiceID, input.Costs.TotalCosts, confNumber)
	}

	s.log.Warn("insufficient credits", zap.Int64("balance", remainingBalance), zap.Int64("required", input.Costs.TotalCosts))
	return repo.MarkIncomplete(ctx, invoiceID)
}

func (s *InvoiceService) chargeWithCard(ctx context.Context, repo *repository.InvoiceRepo, invoiceID int64, input *CreateAndChargeInput) error {
	cardChargeAmount := int(math.Ceil(float64(input.Costs.TotalCosts)))

	s.log.Info("charging card", zap.Int("amount_cents", cardChargeAmount))

	result, err := input.ChargeFunc(invoiceID, cardChargeAmount, input.Costs.InvoiceDesc)
	if err != nil {
		s.log.Error("card charge failed", zap.Error(err))
		if markErr := repo.MarkIncompleteCard(ctx, invoiceID); markErr != nil {
			s.log.Error("failed to mark invoice incomplete", zap.Error(markErr))
		}
		return fmt.Errorf("invoice_service: charge card: %w", err)
	}

	confNumber, err := createInvoiceConfirmationNumber()
	if err != nil {
		return fmt.Errorf("invoice_service: generate confirmation: %w", err)
	}

	_ = result // result used by caller for receipt publishing

	return repo.MarkComplete(ctx, invoiceID, input.Costs.TotalCosts, confNumber)
}

func createTaxMetadata(callTolls, recordings, fax, membership, numberRentals int64) string {
	taxMetadata := map[string]int64{
		"call_tolls_costs":    callTolls,
		"recording_costs":     recordings,
		"fax_costs":           fax,
		"membership_costs":    membership,
		"number_rental_costs": numberRentals,
	}
	b, _ := json.Marshal(taxMetadata)
	return string(b)
}

func createInvoiceConfirmationNumber() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("INV-%08X", b[:4]), nil
}
