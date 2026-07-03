package billing

import (
	"fmt"
	"os"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/stripe/stripe-go/v72"
	"github.com/stripe/stripe-go/v72/paymentintent"
	"github.com/sirupsen/logrus"
	models "lineblocs.com/scheduler/models"
	"database/sql"
	helpers "github.com/Lineblocs/go-helpers"
)

type StripeBillingHandler struct {
	DBConn    *sql.DB
	StripeKey string
	Billing
	RetryAttempts int
}

// CreateIdempotencyKey generates a unique key in the format:
// workspaceid_yyyymmdd_paymentamount
func createIdempotencyKey(workspaceID int, amount int64) string {
	// Go's reference date for YYYYMMDD is 20060102
	dateStr := time.Now().Format("20060102")
	
	// Returns a string like: "500_20260220_1000"
	return fmt.Sprintf("%d_%s_%d", workspaceID, dateStr, amount)
}

func NewStripeBillingHandler(dbConn *sql.DB, stripeKey string, retryAttempts int) *StripeBillingHandler {
	//rootCtx, _ := context.WithCancel(context.Background())
	item := &StripeBillingHandler{
		DBConn:        dbConn,
		StripeKey:     stripeKey,
		RetryAttempts: retryAttempts,
	}
	return item
}

func (hndl *StripeBillingHandler) ChargeCustomer(user *helpers.User, workspace *helpers.Workspace, invoice *models.UserInvoice) (*ChargeResult, error) {
    db := hndl.DBConn
    stripe.Key = hndl.StripeKey

    var id int
    var paymentMethodId string
    var cardLast4 string
    var cardBrand string
    var backupPaymentMethodId string
    var backupCardLast4 string
    var backupCardBrand string

    // Use the attributes directly from the invoice object
    if invoice.PaymentMethodId != "" {
        helpers.Log(logrus.InfoLevel, fmt.Sprintf("Payment method setup from invoice for workspace %d - PaymentMethodId: %s", workspace.Id, invoice.PaymentMethodId))
        paymentMethodId = invoice.PaymentMethodId
        cardLast4 = invoice.CardLast4
        cardBrand = invoice.CardBrand
    }

    // Removed the SQL lookup
    var row *sql.Row
    if invoice.PaymentMethodId == "" {
        helpers.Log(logrus.InfoLevel, fmt.Sprintf("No PaymentMethodId on invoice, querying for primary card in workspace: %d", workspace.Id))
        row = db.QueryRow("SELECT id, stripe_payment_method_id, last_4, issuer FROM users_cards WHERE `workspace_id`=? AND `primary` = 1", workspace.Id)
        err := row.Scan(&id, &paymentMethodId, &cardLast4, &cardBrand)
        if err != nil {
            helpers.Log(logrus.ErrorLevel, fmt.Sprintf("could not lookup default payment method for workspace %d", workspace.Id))

            return nil, fmt.Errorf("could not lookup default payment method for workspace %d", workspace.Id)
        }
        helpers.Log(logrus.InfoLevel, fmt.Sprintf("Successfully retrieved card - ID: %d, Last4: %s, Brand: %s", id, cardLast4, cardBrand))
    } else {
        helpers.Log(logrus.InfoLevel, fmt.Sprintf("Using PaymentMethodId from invoice: %s", paymentMethodId))
    }

    // Attempt to retrieve backup payment method in case primary fails
    if invoice.PaymentMethodId == "" {
        backupRow := db.QueryRow("SELECT id, stripe_payment_method_id, last_4, issuer FROM users_cards WHERE `workspace_id`=? AND `backup` = 1", workspace.Id)
        backupErr := backupRow.Scan(&id, &backupPaymentMethodId, &backupCardLast4, &backupCardBrand)
        if backupErr != nil {
            if backupErr != sql.ErrNoRows {
                helpers.Log(logrus.WarnLevel, fmt.Sprintf("Error querying backup payment method for workspace %d: %v", workspace.Id, backupErr))
            }
            // No backup method available, which is okay
            backupPaymentMethodId = ""
            helpers.Log(logrus.InfoLevel, fmt.Sprintf("No backup payment method found for workspace %d", workspace.Id))
        } else {
            helpers.Log(logrus.InfoLevel, fmt.Sprintf("Backup payment method available for workspace %d - ID: %d, Last4: %s, Brand: %s", workspace.Id, id, backupCardLast4, backupCardBrand))
        }
    }

    domain := os.Getenv("DEPLOYMENT_DOMAIN")
    redirectUrl := fmt.Sprintf("https://app.%s/confirm-payment-intent", domain)
    descriptorSuffix := fmt.Sprintf("%s invoice", domain)
    customerId := user.StripeId
    
    // Convert amount once to ensure consistency
    amountCents := int64(invoice.Cents)

    // Define the parameters for creating a PaymentIntent
    params := &stripe.PaymentIntentParams{
        Amount:                  stripe.Int64(amountCents),
        Currency:                stripe.String(string(stripe.CurrencyUSD)),
        AutomaticPaymentMethods: &stripe.PaymentIntentAutomaticPaymentMethodsParams{Enabled: stripe.Bool(true)},
        Customer:                stripe.String(customerId),
        PaymentMethod:           stripe.String(paymentMethodId),
        ReturnURL:               stripe.String(redirectUrl),
        OffSession:              stripe.Bool(true),
        Confirm:                 stripe.Bool(true),
        StatementDescriptorSuffix: stripe.String(descriptorSuffix),
    }

    // Apply the custom idempotency key
    idempotencyKey := createIdempotencyKey(workspace.Id, amountCents)
	helpers.Log(logrus.InfoLevel, fmt.Sprintf("Using idempotency key: %s for PaymentIntent creation", idempotencyKey))
	params.SetIdempotencyKey(idempotencyKey)

    // Create the PaymentIntent
	res, err := paymentintent.New(params)

    if err != nil {
        helpers.Log(logrus.ErrorLevel, fmt.Sprintf("Stripe Charge Failed: %v", err))

        // Try backup payment method if primary failed and backup exists
        if backupPaymentMethodId != "" {
            helpers.Log(logrus.InfoLevel, fmt.Sprintf("Attempting charge with backup payment method for workspace %d", workspace.Id))
            
            params.PaymentMethod = stripe.String(backupPaymentMethodId)
            idempotencyKey := createIdempotencyKey(workspace.Id, amountCents)
            params.SetIdempotencyKey(idempotencyKey)
            
            res, err = paymentintent.New(params)
            if err != nil {
                helpers.Log(logrus.ErrorLevel, fmt.Sprintf("Stripe Charge Failed with backup method: %v", err))
                return nil, err
            }
            
            paymentMethodId = backupPaymentMethodId
            cardLast4 = backupCardLast4
            cardBrand = backupCardBrand
            helpers.Log(logrus.InfoLevel, fmt.Sprintf("Backup payment method succeeded for workspace %d", workspace.Id))
        } else {
            return nil, err
        }
    }

    helpers.Log(logrus.InfoLevel, fmt.Sprintf("Stripe PaymentIntent processed. ID: %s Status: %s", res.ID, res.Status))

    chargeResult := &ChargeResult{
        PaymentGatewayID: res.ID,
        PaymentMethodID: paymentMethodId,
        Amount:          amountCents,
        Currency:        string(stripe.CurrencyUSD),
        Status:          string(res.Status),
        Created:         res.Created,
        CardBrand:       cardBrand,
        CardLast4:       cardLast4,
    }

    return chargeResult, nil
}
