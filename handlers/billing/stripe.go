package billing

import (
	"fmt"
	"os"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/stripe/stripe-go/v72"
	"github.com/stripe/stripe-go/v72/paymentintent"
	"github.com/sirupsen/logrus"
	models "lineblocs.com/crontabs/models"
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

func (hndl *StripeBillingHandler) ChargeCustomer(user *helpers.User, workspace *helpers.Workspace, invoice *models.UserInvoice) error {
    db := hndl.DBConn
    stripe.Key = hndl.StripeKey

    var id int
    var paymentMethodId string
    row := db.QueryRow("SELECT id, stripe_payment_method_id FROM users_cards WHERE `workspace_id`=? AND `primary` = 1", workspace.Id)

    err := row.Scan(&id, &paymentMethodId)
    if err != nil {
        return err
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
        return err
    }

    helpers.Log(logrus.InfoLevel, fmt.Sprintf("Stripe PaymentIntent processed. ID: %s Status: %s", res.ID, res.Status))

    return nil
}
