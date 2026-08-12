package billing

import (
	_ "github.com/go-sql-driver/mysql"

	helpers "github.com/Lineblocs/go-helpers"
	models "lineblocs.com/scheduler/models"
)

type BillingHandler interface {
	ChargeCustomer(user *helpers.User, workspace *helpers.Workspace, invoice *models.UserInvoice) (*ChargeResult, error)
	RefundAccount(user *helpers.User, workspace *helpers.Workspace, amount int64) error
}

type Billing struct {
	RetryAttempts int
}

// ChargeResult contains the details of a successful charge
type ChargeResult struct {
	PaymentGatewayID  string
	PaymentMethodID  string
	Amount           int64
	Currency         string
	Status           string
	Created          int64
	CardBrand        string
	CardLast4        string
}
