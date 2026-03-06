package billing

import (
	"fmt"

	helpers "github.com/Lineblocs/go-helpers"
	"go.uber.org/zap"
	handlerbilling "lineblocs.com/scheduler/handlers/billing"
	"lineblocs.com/scheduler/models"
)

// ChargeService routes charges to the correct payment handler.
type ChargeService struct {
	log *zap.Logger
}

// NewChargeService creates a new ChargeService.
func NewChargeService(log *zap.Logger) *ChargeService {
	return &ChargeService{log: log}
}

// MakeChargeFunc creates a ChargeFunc for the given provider and workspace.
func (s *ChargeService) MakeChargeFunc(
	provider string,
	stripeKey string,
	retryAttempts int,
	user *helpers.User,
	workspace *helpers.Workspace,
	dbConn interface{ },
) ChargeFunc {
	return func(invoiceID int64, cents int, desc string) (*ChargeResult, error) {
		invoice := &models.UserInvoice{
			Id:          int(invoiceID),
			Cents:       cents,
			InvoiceDesc: desc,
		}

		var handler handlerbilling.BillingHandler
		switch provider {
		case "stripe":
			// We need a *sql.DB for the handler; extract from the interface
			handler = handlerbilling.NewStripeBillingHandler(nil, stripeKey, retryAttempts)
		case "braintree":
			handler = handlerbilling.NewBraintreeBillingHandler(nil, "", retryAttempts)
		default:
			return nil, fmt.Errorf("charge_service: unknown provider %q", provider)
		}

		result, err := handler.ChargeCustomer(user, workspace, invoice)
		if err != nil {
			return nil, fmt.Errorf("charge_service: %s charge failed: %w", provider, err)
		}

		return &ChargeResult{
			PaymentIntentID: result.PaymentIntentID,
			PaymentMethodID: result.PaymentMethodID,
			Amount:          result.Amount,
			Currency:        result.Currency,
			Status:          result.Status,
			Created:         result.Created,
			CardBrand:       result.CardBrand,
			CardLast4:       result.CardLast4,
		}, nil
	}
}
