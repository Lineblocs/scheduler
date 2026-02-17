package repository

import (
	"database/sql"
	"fmt"
	"strconv"

	helpers "github.com/Lineblocs/go-helpers"
	"github.com/sirupsen/logrus"
	"lineblocs.com/crontabs/handlers/billing"
	"lineblocs.com/crontabs/models"
	"lineblocs.com/crontabs/utils"
)

type PaymentService struct {
	db *sql.DB
}

type PaymentRepository interface {
	ChargeCustomer(billingParams *utils.BillingParams, user *helpers.User, workspace *helpers.Workspace, invoice *models.UserInvoice) error
	GetServicePlans() ([]helpers.ServicePlan, error)
}

func NewPaymentRepository(db *sql.DB) PaymentRepository {
	return NewPaymentService(db)
}

func NewPaymentService(db *sql.DB) *PaymentService {
	return &PaymentService{
		db: db,
	}
}

func (ps *PaymentService) GetServicePlans() ([]helpers.ServicePlan, error) {
	//TODO: In the future replace by GetServicePlans2() call
	return helpers.GetServicePlans()
}

func (ps *PaymentService) ChargeCustomer(billingParams *utils.BillingParams, user *helpers.User, workspace *helpers.Workspace, invoice *models.UserInvoice) error {
	var err error
	var hndl billing.BillingHandler
	retryAttempts := getRetryAttempts(billingParams.Data["retry_attempts"])

	switch billingParams.Provider {
	case "stripe":
		key := billingParams.Data["stripe_key"]
		hndl = billing.NewStripeBillingHandler(ps.db, key, retryAttempts)
		err = hndl.ChargeCustomer(user, workspace, invoice)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "error charging user..\r\n")
			helpers.Log(logrus.ErrorLevel, err.Error())
		}
	case "braintree":
		key := billingParams.Data["braintree_api_key"]
		hndl = billing.NewBraintreeBillingHandler(ps.db, key, retryAttempts)
		err = hndl.ChargeCustomer(user, workspace, invoice)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "error charging user..\r\n")
			helpers.Log(logrus.ErrorLevel, err.Error())
		}
	}

	return err
}

func getRetryAttempts(s string) int {
	retryAttempts, err := strconv.Atoi(s)
	if err != nil {
		helpers.Log(logrus.InfoLevel, fmt.Sprintf("variable retryAttempts is setup incorrectly. Please ensure that it is set to an integer. retryAttempts=%s setting value to 0", s))
		retryAttempts = 0
	}

	return retryAttempts
}
