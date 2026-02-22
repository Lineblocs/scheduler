package billing

import (
	_ "github.com/go-sql-driver/mysql"

	helpers "github.com/Lineblocs/go-helpers"
	models "lineblocs.com/scheduler/models"
)

type BillingHandler interface {
	ChargeCustomer(user *helpers.User, workspace *helpers.Workspace, invoice *models.UserInvoice) error
}

type Billing struct {
	RetryAttempts int
}
