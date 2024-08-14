package cmd

import (
	"strconv"
	"time"

	helpers "github.com/Lineblocs/go-helpers"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/mailgun/mailgun-go/v4"
	"github.com/sirupsen/logrus"
	models "lineblocs.com/crontabs/models"
	utils "lineblocs.com/crontabs/utils"
)

// cron tab to remove unset password users
func RetryFailedBillingAttempts() error {

	db := utils.NewDBConn(nil)

	billingParams, err := db.GetBillingParams()
	if err != nil {
		return err
	}
	results, err := db.Conn.Query(`SELECT users_invoices.id, users_invoices.workspace_id, workspaces.creator_id, users_invoices.cents
	INNER JOIN workspaces ON workspaces.id = users_invoices.workspace_id
	FROM users_invoices WHERE users_invoices.status = 'INCOMPLETE'`)
	if err != nil {
		return err
	}
	defer results.Close()
	var invoiceId int
	var workspaceId int
	var userId int
	var cents int
	for results.Next() {
		err = results.Scan(&invoiceId, &workspaceId, &userId, &cents)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "error scanning for db result "+err.Error())
			continue
		}
		workspace, err := helpers.GetWorkspaceFromDB(workspaceId)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "error getting workspace ID: "+strconv.Itoa(workspaceId)+"\r\n")
			continue
		}
		user, err := helpers.GetUserFromDB(userId)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "error getting user ID: "+strconv.Itoa(userId)+"\r\n")
			continue
		}
		// try to charge the user again.
		invoiceDesc := "Invoice for service"
		invoice := models.UserInvoice{
			Id:          invoiceId,
			Cents:       cents,
			InvoiceDesc: invoiceDesc}
		err = utils.ChargeCustomer(db.Conn, billingParams, user, workspace, &invoice)
		currentTime := time.Now()
		if err != nil { // failed again
			stmt, err := db.Conn.Prepare("UPDATE users_invoices SET status = 'INCOMPLETE', source = 'CARD', last_attempted = ? WHERE id = ?")
			if err != nil {
				helpers.Log(logrus.ErrorLevel, "could not prepare query..\r\n")
				continue
			}
			_, err = stmt.Exec(currentTime, invoiceId)
			if err != nil {
				helpers.Log(logrus.ErrorLevel, "error updating invoice....\r\n")
				helpers.Log(logrus.ErrorLevel, err.Error())
				continue
			}
			continue
		}
		confNumber, err := utils.CreateInvoiceConfirmationNumber()
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "error while generating confirmation number: "+err.Error())
			continue
		}

		// mark as paid
		stmt, err := db.Conn.Prepare("UPDATE users_invoices SET status = 'COMPLETE', source ='CREDITS', cents_collected = ?, last_attempted = ?, num_attempts = num_attempts + 1, confirmation_number = ? WHERE id = ?")
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "could not prepare query..\r\n")
			continue
		}

		_, err = stmt.Exec(cents, currentTime, confNumber, invoiceId)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "error updating debit..\r\n")
			helpers.Log(logrus.ErrorLevel, err.Error())
			continue
		}
	}
	return nil
}
