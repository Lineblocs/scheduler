package cmd

import (
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"time"

	helpers "github.com/Lineblocs/go-helpers"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/mailgun/mailgun-go/v4"
	"github.com/sirupsen/logrus"
	models "lineblocs.com/crontabs/models"
	"lineblocs.com/crontabs/repository"
	utils "lineblocs.com/crontabs/utils"
)

type AnnualBillingJob struct {
	workspaceRepository repository.WorkspaceRepository
	paymentRepository   repository.PaymentRepository
	db                  *sql.DB
}

func NewAnnualBillingJob(db *sql.DB, worskpaceRepository repository.WorkspaceRepository, paymentRepository repository.PaymentRepository) *AnnualBillingJob {
	return &AnnualBillingJob{
		db:                  db,
		workspaceRepository: worskpaceRepository,
		paymentRepository:   paymentRepository,
	}
}

// cron tab to run annual billing
func (ab *AnnualBillingJob) AnnualBilling() error {
	var id int
	var creatorId int

	conn := utils.NewDBConn(ab.db)

	billingParams, err := conn.GetBillingParams()
	if err != nil {
		return err
	}

	// get any workspaces that have annual pricing enabled
	results, err := ab.db.Query("SELECT id, creator_id FROM workspaces WHERE plan_term = 'annual'")
	if err != nil {
		helpers.Log(logrus.ErrorLevel, "error running query..\r\n")
		helpers.Log(logrus.ErrorLevel, err.Error())
		return err
	}

	plans, err := ab.paymentRepository.GetServicePlans()
	if err != nil {
		helpers.Log(logrus.ErrorLevel, "error getting service plans\r\n")
		helpers.Log(logrus.ErrorLevel, err.Error())
		return err
	}
	// time for all annual invoices will be the same
	// TODO: look into possibly changing this to ensure times are in sync with database records
	currentTime := time.Now()

	defer results.Close()
	for results.Next() {

		_ = results.Scan(&id, &creatorId)
		workspace, err := ab.workspaceRepository.GetWorkspaceFromDB(id)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "error getting workspace ID: "+strconv.Itoa(id)+"\r\n")
			continue
		}
		user, err := ab.workspaceRepository.GetUserFromDB(creatorId)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "error getting user ID: "+strconv.Itoa(id)+"\r\n")
			continue
		}

		plan := utils.GetPlan(plans, workspace)

		invoiceDesc := "LineBlocs annual invoice"

		userCount := utils.GetWorkspaceUserCount(ab.db, workspace.Id)
		helpers.Log(logrus.InfoLevel, fmt.Sprintf("Workspace total user count %d", userCount))

		membershipCosts := float64(plan.AnnualCostCents) * float64(userCount)
		totalCostsCents := int(math.Ceil(membershipCosts))
		// any regular costs are accured towards monthly billing, no need to charge anything here
		regularCostsCents := 0
		stmt, err := ab.db.Prepare("INSERT INTO users_invoices (`cents`, `membership_costs`, `status`, `user_id`, `workspace_id`, `created_at`, `updated_at`) VALUES ( ?, ?, ?, ?, ?, ?, ?)")
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "could not prepare query..\r\n")
			helpers.Log(logrus.ErrorLevel, err.Error())
			continue
		}
		defer stmt.Close()

		res, err := stmt.Exec(regularCostsCents, totalCostsCents, "INCOMPLETE", workspace.CreatorId, workspace.Id, currentTime, currentTime)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "error creating invoice..\r\n")
			helpers.Log(logrus.ErrorLevel, err.Error())
			continue
		}

		invoiceId, err := res.LastInsertId()
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "could not get insert id..\r\n")
			helpers.Log(logrus.ErrorLevel, err.Error())
			continue
		}

		helpers.Log(logrus.InfoLevel, "Charging recurringly with card..\r\n")
		invoice := models.UserInvoice{
			Id:          int(invoiceId),
			Cents:       totalCostsCents,
			InvoiceDesc: invoiceDesc,
		}

		err = ab.paymentRepository.ChargeCustomer(billingParams, user, workspace, &invoice)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "error charging user..\r\n")
			helpers.Log(logrus.ErrorLevel, err.Error())

			stmt, err := ab.db.Prepare("UPDATE users_invoices SET status = 'INCOMPLETE', source = 'CARD', cents_collected = 0.0 WHERE id = ?")
			if err != nil {
				helpers.Log(logrus.ErrorLevel, "could not prepare query..\r\n")
				continue
			}

			_, err = stmt.Exec(invoiceId)
			if err != nil {
				helpers.Log(logrus.ErrorLevel, "error updating invoice....\r\n")
				helpers.Log(logrus.ErrorLevel, err.Error())
				continue
			}
			// TODO send email when any billing attempts fail
			continue
		}

		confNumber, err := utils.CreateInvoiceConfirmationNumber()
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "error while generating confirmation number: "+err.Error())
			continue
		}

		stmt, err = ab.db.Prepare("UPDATE users_invoices SET status = 'COMPLETE', source ='CARD', cents_collected = ?, confirmation_number = ? WHERE id = ?")
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "could not prepare query..\r\n")
			continue
		}

		_, err = stmt.Exec(totalCostsCents, confNumber, invoiceId)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "error updating debit..\r\n")
			helpers.Log(logrus.ErrorLevel, err.Error())
			continue
		}
	}

	return nil
}
