package cmd

import (
	"context"
	"fmt"
	"time"

	"database/sql"
	"math"
	"strconv"

	helpers "github.com/Lineblocs/go-helpers"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/mailgun/mailgun-go/v4"
	"github.com/sirupsen/logrus"
	models "lineblocs.com/crontabs/models"
	"lineblocs.com/crontabs/repository"
	utils "lineblocs.com/crontabs/utils"
)

type MonthlyBillingJob struct {
	workspaceRepository repository.WorkspaceRepository
	paymentRepository   repository.PaymentRepository
	db                  *sql.DB
	logger              *logrus.Entry
}

type BillingMetrics struct {
	MembershipCosts    float64
	CallTolls          float64
	RecordingCosts     float64
	FaxCosts           float64
	MonthlyNumberRents float64
	TotalCosts         float64
}

type BillingContext struct {
	WorkspaceID    int
	UserID         int
	StartTime      time.Time
	EndTime        time.Time
	CorrelationID  string
	Logger         *logrus.Entry
}

func NewMonthlyBillingJob(db *sql.DB, workspaceRepository repository.WorkspaceRepository, paymentRepository repository.PaymentRepository) *MonthlyBillingJob {
	return &MonthlyBillingJob{
		db:                  db,
		workspaceRepository: workspaceRepository,
		paymentRepository:   paymentRepository,
		logger:              logrus.WithField("component", "monthly_billing"),
	}
}

// cron tab to run monthly billing
func (mb *MonthlyBillingJob) MonthlyBilling() error {
	var id int
	var creatorId int

	conn := utils.NewDBConn(mb.db)

	billingParams, err := conn.GetBillingParams()
	if err != nil {
		return err
	}

	start := time.Now()
	start = start.AddDate(0, -1, 0)
	end := time.Now()
	currentTime := time.Now()
	startFormatted := start.Format(time.DateTime)
	endFormatted := end.Format(time.DateTime)
	results, err := mb.db.Query("SELECT id, creator_id FROM workspaces")
	if err != nil {
		helpers.Log(logrus.ErrorLevel, "error running query..\r\n")
		helpers.Log(logrus.ErrorLevel, err.Error())
		return err
	}
	plans, err := mb.paymentRepository.GetServicePlans()
	if err != nil {
		helpers.Log(logrus.ErrorLevel, "error getting service plans\r\n")
		helpers.Log(logrus.ErrorLevel, err.Error())
		return err
	}

	defer results.Close()
	for results.Next() {
		_ = results.Scan(&id, &creatorId)
		workspace, err := mb.workspaceRepository.GetWorkspaceFromDB(id)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "error getting workspace ID: "+strconv.Itoa(id)+"\r\n")
			continue
		}
		user, err := mb.workspaceRepository.GetUserFromDB(creatorId)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "error getting user ID: "+strconv.Itoa(id)+"\r\n")
			continue
		}

		plan := utils.GetPlan(plans, workspace)

		billingInfo, err := mb.workspaceRepository.GetWorkspaceBillingInfo(workspace)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "Could not get billing info..\r\n")
			helpers.Log(logrus.ErrorLevel, err.Error())
			continue
		}

		utils.CreateMonthlyNumberRentalDebit(mb.db, workspace.Id, user.Id, start)

		baseCosts, err := helpers.GetBaseCosts()
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "error getting base costs..\r\n")
			helpers.Log(logrus.ErrorLevel, err.Error())
			continue
		}

		userCount := utils.GetWorkspaceUserCount(mb.db, workspace.Id)
		helpers.Log(logrus.InfoLevel, fmt.Sprintf("Workspace total user count %d", userCount))

		totalCosts := 0.0
		membershipCosts := plan.BaseCosts * float64(userCount)
		callTolls := 0.0
		recordingCosts := 0.0
		faxCosts := 0.0
		monthlyNumberRentals := 0.0
		invoiceDesc := fmt.Sprintf("LineBlocs invoice for %s", billingInfo.InvoiceDue)

		helpers.Log(logrus.InfoLevel, fmt.Sprintf("Workspace total membership costs is %f", membershipCosts))

		results2, err := mb.db.Query("SELECT id, source, module_id, cents, created_at FROM users_debits WHERE user_id = ? AND created_at BETWEEN ? AND ?", workspace.CreatorId, startFormatted, endFormatted)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "error running query..\r\n")
			helpers.Log(logrus.ErrorLevel, err.Error())
			return err
		}
		defer results2.Close()
		var id int
		var source string
		var moduleId int
		var cents float64
		var created time.Time
		usedMonthlyMinutes := plan.MinutesPerMonth
		usedMonthlyRecordings := plan.RecordingSpace
		usedMonthlyFax := plan.Fax
		for results2.Next() {
			results2.Scan(&id, &source, &moduleId, &cents, &created)
			helpers.Log(logrus.InfoLevel, fmt.Sprintf("scanning in debit source %s\r\n", source))
			switch source {
			case "CALL":
				helpers.Log(logrus.InfoLevel, fmt.Sprintf("getting call %d\r\n", moduleId))
				call, err := mb.workspaceRepository.GetCallFromDB(moduleId)
				if err != nil {
					helpers.Log(logrus.ErrorLevel, "error running query..\r\n")
					helpers.Log(logrus.ErrorLevel, err.Error())
					return err
				}
				duration := call.DurationNumber
				helpers.Log(logrus.InfoLevel, fmt.Sprintf("call duration is %d\r\n", duration))
				minutes := float64(duration / 60)
				charge, err := utils.ComputeAmountToCharge(cents, usedMonthlyMinutes, minutes)
				if err != nil {
					helpers.Log(logrus.ErrorLevel, "error getting charge..\r\n")
					helpers.Log(logrus.ErrorLevel, err.Error())
					continue
				}
				callTolls = callTolls + charge
				usedMonthlyMinutes = usedMonthlyMinutes - minutes

			case "NUMBER_RENTAL":
				helpers.Log(logrus.InfoLevel, fmt.Sprintf("getting DID %d\r\n", moduleId))
				did, err := mb.workspaceRepository.GetDIDFromDB(moduleId)
				if err != nil {
					helpers.Log(logrus.ErrorLevel, "error running query..\r\n")
					helpers.Log(logrus.ErrorLevel, err.Error())
					continue
				}

				monthlyNumberRentals += float64(did.MonthlyCost)
			}
		}
		results3, err := mb.db.Query("SELECT id, size, created_at FROM recordings WHERE user_id = ? AND created_at BETWEEN ? AND ?", workspace.CreatorId, startFormatted, endFormatted)
		if err != sql.ErrNoRows && err != nil {
			helpers.Log(logrus.ErrorLevel, "error running query..\r\n")
			helpers.Log(logrus.ErrorLevel, err.Error())
			return err
		}
		defer results3.Close()
		var recId int
		var size float64
		var createdAt time.Time
		for results3.Next() {
			results3.Scan(&recId, &size, &createdAt)
			cents := math.Round(baseCosts.RecordingsPerByte * float64(size))
			charge, err := utils.ComputeAmountToCharge(cents, usedMonthlyRecordings, size)
			if err != nil {
				helpers.Log(logrus.ErrorLevel, "error calculating charge amount\r\n")
				helpers.Log(logrus.ErrorLevel, err.Error())
				continue
			}
			recordingCosts += charge
			usedMonthlyRecordings -= size
		}

		results4, err := mb.db.Query("SELECT id, created_at FROM faxes WHERE workspace_id = ? AND created_at BETWEEN ? AND ?", workspace.Id, startFormatted, endFormatted)
		if err != sql.ErrNoRows && err != nil {
			helpers.Log(logrus.ErrorLevel, "error running query..\r\n")
			helpers.Log(logrus.ErrorLevel, err.Error())
			return err
		}
		defer results4.Close()
		var faxId int
		for results4.Next() {
			results4.Scan(&faxId, &createdAt)
			totalFax := float64(plan.Fax)
			centsForFax := baseCosts.FaxPerUsed
			charge, err := utils.ComputeAmountToCharge(centsForFax, float64(usedMonthlyFax), totalFax)
			if err != nil {
				helpers.Log(logrus.ErrorLevel, "error calculating charge amount\r\n")
				helpers.Log(logrus.ErrorLevel, err.Error())
				continue
			}
			faxCosts += charge
			usedMonthlyFax -= 1
		}

		totalCosts += membershipCosts
		totalCosts += callTolls
		totalCosts += recordingCosts
		totalCosts += faxCosts
		totalCosts += monthlyNumberRentals

		helpers.Log(logrus.InfoLevel, fmt.Sprintf("Final costs are membership: %f, call tolls: %f, recordings: %f, fax: %f, did rentals: %f, total: %f (cents)\r\n",
			membershipCosts,
			callTolls,
			recordingCosts,
			faxCosts,
			monthlyNumberRentals,
			totalCosts))

		helpers.Log(logrus.InfoLevel, fmt.Sprintf("Creating invoice for user %d, on workspace %d, plan type %s\r\n", user.Id, workspace.Id, workspace.Plan))
		stmt, err := mb.db.Prepare("INSERT INTO users_invoices (`cents`, `call_costs`, `recording_costs`, `fax_costs`, `membership_costs`, `number_costs`, `status`, `user_id`, `workspace_id`, `created_at`, `updated_at`) VALUES ( ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "could not prepare query..\r\n")
			helpers.Log(logrus.ErrorLevel, err.Error())
			continue
		}
		defer stmt.Close()
		res, err := stmt.Exec(cents, callTolls, recordingCosts, faxCosts, membershipCosts, monthlyNumberRentals, "INCOMPLETE", workspace.CreatorId, workspace.Id, currentTime, currentTime)
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
		helpers.Log(logrus.InfoLevel, fmt.Sprintf("Charging user %d, on workspace %d, plan type %s\r\n", user.Id, workspace.Id, workspace.Plan))

		// try to charge the debit
		if plan.PayAsYouGo {
			remainingBalance := billingInfo.RemainingBalanceCents
			minRemaining := remainingBalance - totalCosts
			charge, err := utils.ComputeAmountToCharge(totalCosts, remainingBalance, minRemaining)
			if err != nil {
				helpers.Log(logrus.ErrorLevel, "error calculating charge amount\r\n")
				helpers.Log(logrus.ErrorLevel, err.Error())

				continue
			}
			if remainingBalance >= totalCosts { //user has enough credits
				helpers.Log(logrus.InfoLevel, "User has enough credits. Charging balance\r\n")

				confNumber, err := utils.CreateInvoiceConfirmationNumber()
				if err != nil {
					helpers.Log(logrus.ErrorLevel, "error while generating confirmation number: "+err.Error())
					continue
				}

				stmt, err := mb.db.Prepare("UPDATE users_invoices SET status = 'COMPLETE', source ='CREDITS', cents_collected = ?, confirmation_number = ? WHERE id = ?")
				if err != nil {
					helpers.Log(logrus.ErrorLevel, "could not prepare query..\r\n")
					continue
				}
				_, err = stmt.Exec(totalCosts, confNumber, invoiceId)
				if err != nil {
					helpers.Log(logrus.ErrorLevel, "error updating debit..\r\n")
					helpers.Log(logrus.ErrorLevel, err.Error())
					continue
				}
			} else {
				helpers.Log(logrus.InfoLevel, "User does not have enough credits. Charging any payment sources\r\n")
				// update debit to reflect exactly how much we can charge
				stmt, err := mb.db.Prepare("UPDATE users_invoices SET status = 'INCOMPLETE', source ='CREDITS', cents_collected = ? WHERE id = ?")
				if err != nil {
					helpers.Log(logrus.ErrorLevel, "could not prepare query..\r\n")
					continue
				}
				_, err = stmt.Exec(charge, invoiceId)
				if err != nil {
					helpers.Log(logrus.ErrorLevel, "error updating debit..\r\n")
					helpers.Log(logrus.ErrorLevel, err.Error())
					continue
				}
				// try to charge the rest using a card
				helpers.Log(logrus.InfoLevel, "Charging remainder with card..\r\n")

				cents := int(math.Ceil(charge))
				invoice := models.UserInvoice{
					Id:          int(invoiceId),
					Cents:       cents,
					InvoiceDesc: invoiceDesc}
				err = mb.paymentRepository.ChargeCustomer(billingParams, user, workspace, &invoice)
				if err != nil {
					// could not charge card.
					// update invoice record and mark as outstanding
					stmt, err = mb.db.Prepare("UPDATE users_invoices SET source = 'CARD', status = 'INCOMPLETE', num_attempts = 1, last_attempted = ? WHERE id = ?")
					if err != nil {
						helpers.Log(logrus.ErrorLevel, "could not prepare query..\r\n")
						continue
					}
					_, err = stmt.Exec(currentTime, invoiceId)
					if err != nil {
						helpers.Log(logrus.ErrorLevel, "error updating debit..\r\n")
						helpers.Log(logrus.ErrorLevel, err.Error())
						continue
					}
					continue
				}
				stmt, err = mb.db.Prepare("UPDATE users_invoices SET status = 'COMPLETE', source ='CARD', cents_collected = ?, last_attempted = ?, num_attempts = 1 WHERE id = ?")
				if err != nil {
					helpers.Log(logrus.ErrorLevel, "could not prepare query..\r\n")
					continue
				}
				_, err = stmt.Exec(totalCosts, currentTime, invoiceId)
				if err != nil {
					helpers.Log(logrus.ErrorLevel, "error updating debit..\r\n")
					helpers.Log(logrus.ErrorLevel, err.Error())
					continue
				}
			}
		} else {
			// regular membership charge. only try to charge a card
			helpers.Log(logrus.InfoLevel, "Charging recurringly with card..\r\n")
			cents := int(math.Ceil(totalCosts))
			invoice := models.UserInvoice{
				Id:          int(invoiceId),
				Cents:       cents,
				InvoiceDesc: invoiceDesc}
			err := mb.paymentRepository.ChargeCustomer(billingParams, user, workspace, &invoice)
			if err != nil {
				helpers.Log(logrus.ErrorLevel, "error charging user..\r\n")
				helpers.Log(logrus.ErrorLevel, err.Error())
				stmt, err := mb.db.Prepare("UPDATE users_invoices SET status = 'INCOMPLETE', source = 'CARD', cents_collected = 0.0 WHERE id = ?")
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
				// TODO send email when any biliing attempts fail
				continue
			}

			confNumber, err := utils.CreateInvoiceConfirmationNumber()
			if err != nil {
				helpers.Log(logrus.ErrorLevel, "error while generating confirmation number: "+err.Error())
				continue
			}

			stmt, err := mb.db.Prepare("UPDATE users_invoices SET status = 'COMPLETE', source ='CARD', cents_collected = ?, confirmation_number = ? WHERE id = ?")
			if err != nil {
				helpers.Log(logrus.ErrorLevel, "could not prepare query..\r\n")
				continue
			}
			_, err = stmt.Exec(totalCosts, confNumber, invoiceId)
			if err != nil {
				helpers.Log(logrus.ErrorLevel, "error updating debit..\r\n")
				helpers.Log(logrus.ErrorLevel, err.Error())
				continue
			}
		}
	}
	return nil
}
