package billing

import (
	_ "github.com/go-sql-driver/mysql"

	"database/sql"
	"errors"

	helpers "github.com/Lineblocs/go-helpers"
	models "lineblocs.com/scheduler/models"
)

type BraintreeBillingHandler struct {
	DBConn       *sql.DB
	BraintreeKey string
	Billing
	RetryAttempts int
}

func NewBraintreeBillingHandler(dbConn *sql.DB, BraintreeKey string, retryAttempts int) *BraintreeBillingHandler {
	//rootCtx, _ := context.WithCancel(context.Background())
	item := BraintreeBillingHandler{
		DBConn:        dbConn,
		BraintreeKey:  BraintreeKey,
		RetryAttempts: retryAttempts}
	return &item
}

func (hndl *BraintreeBillingHandler) ChargeCustomer(user *helpers.User, workspace *helpers.Workspace, invoice *models.UserInvoice) error {
	//_ := hndl.DBConn
	// todo: implement handler
	return errors.New("not implemented yet")
}
