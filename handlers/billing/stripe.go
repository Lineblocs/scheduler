package billing

import (
	"fmt"
	"os"

	_ "github.com/go-sql-driver/mysql"
	"github.com/stripe/stripe-go/v72"
	"github.com/stripe/stripe-go/v72/paymentintent"
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
	row := db.QueryRow(`SELECT id, stripe_payment_method_id FROM users_cards WHERE workspace_id=? AND primary =1`, workspace.Id)

	err := row.Scan(&id, &paymentMethodId)
	if err != nil {
		return err
	}

	domain := os.Getenv("DEPLOYMENT_DOMAIN")
	redirectUrl := fmt.Sprintf("https://app.%s/confirm-payment-intent", domain)
	descriptor := fmt.Sprintf("%s invoice", domain)
	customerId := user.StripeId
	// Define the parameters for creating a PaymentIntent
	params := &stripe.PaymentIntentParams{
		Amount:                  stripe.Int64(int64(invoice.Cents)),
		Currency:                stripe.String(string(stripe.CurrencyUSD)),
		AutomaticPaymentMethods: &stripe.PaymentIntentAutomaticPaymentMethodsParams{Enabled: stripe.Bool(true)},
		Customer:                stripe.String(customerId),
		PaymentMethod:           stripe.String(paymentMethodId), // Replace with the payment method ID
		ReturnURL:               stripe.String(redirectUrl),     // Replace with the redirect URL
		OffSession:              stripe.Bool(true),
		Confirm:                 stripe.Bool(true),
		StatementDescriptor:     stripe.String(descriptor), // Replace with your statement descriptor
	}

	// Create the PaymentIntent
	_, err = paymentintent.New(params)

	if err != nil {
		return err
	}

	return nil
}
