package main

import (
	"os"

	helpers "github.com/Lineblocs/go-helpers"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/mailgun/mailgun-go/v4"
	"github.com/sirupsen/logrus"
	cmd "lineblocs.com/scheduler/cmd"
	"lineblocs.com/scheduler/repository"
	"lineblocs.com/scheduler/utils"
	//now "github.com/jinzhu/now"
)

func main() {
	var err error

	logDestination := utils.Config("LOG_DESTINATIONS")
	helpers.InitLogrus(logDestination)

	args := os.Args[1:]
	if len(args) == 0 {
		helpers.Log(logrus.InfoLevel, "Please provide command")
		return
	}
	command := args[0]
	switch command {
	case "cleanup":
		helpers.Log(logrus.InfoLevel, "App cleanup started...")
		err = cmd.CleanupApp()
		if err != nil {
			helpers.Log(logrus.ErrorLevel, err.Error())
		}
	case "background_emails":
		helpers.Log(logrus.InfoLevel, "sending background emails")
		err = cmd.SendBackgroundEmails()
		if err != nil {
			helpers.Log(logrus.ErrorLevel, err.Error())
		}
	case "monthly_billing":
		helpers.Log(logrus.InfoLevel, "running monthly billing routines")

		db, _ := helpers.CreateDBConn()
		ws := repository.NewWorkspaceService()
		ps := repository.NewPaymentService(db)
		job := cmd.NewMonthlyBillingJob(db, ws, ps)

		err = job.MonthlyBilling()
		if err != nil {
			helpers.Log(logrus.ErrorLevel, err.Error())
		}
	case "annual_billing":
		helpers.Log(logrus.InfoLevel, "running annual billing routines")

		db, _ := helpers.CreateDBConn()
		ws := repository.NewWorkspaceService()
		ps := repository.NewPaymentService(db)

		job := cmd.NewAnnualBillingJob(db, ws, ps)

		err = job.AnnualBilling()
		if err != nil {
			helpers.Log(logrus.ErrorLevel, err.Error())
		}
	case "retry_failed_billing_attempts":
		helpers.Log(logrus.InfoLevel, "reattempting to bill unpaid invoices")
		err = cmd.RetryFailedBillingAttempts()
		if err != nil {
			helpers.Log(logrus.ErrorLevel, err.Error())
		}
	case "remove_logs":
		helpers.Log(logrus.InfoLevel, "removing old logs")
		err = cmd.RemoveLogs()
		if err != nil {
			helpers.Log(logrus.ErrorLevel, err.Error())
		}
	}
}
