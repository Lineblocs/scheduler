package billing

import (
	"database/sql"
	"fmt"
	"math"
	"time"

	helpers "github.com/Lineblocs/go-helpers"
	"github.com/sirupsen/logrus"
	"lineblocs.com/crontabs/models"
	"lineblocs.com/crontabs/repository"
	"lineblocs.com/crontabs/utils"
)

type BillingData struct {
	BillingParams      interface{}
	Workspace          *helpers.Workspace
	User               *helpers.User
	Plan               *helpers.ServicePlan
	BillingInfo        *helpers.WorkspaceBillingInfo
	BaseCosts          *helpers.BaseCosts
	BillingPeriodStart time.Time
	BillingPeriodEnd   time.Time
	Now                time.Time
}

type BillingCosts struct {
	MembershipCosts   float64
	CallTollsCosts    float64
	RecordingCosts    float64
	FaxCosts          float64
	NumberRentalCosts float64
	TotalCosts        float64
	InvoiceDesc       string
}

type BillingService struct {
	db                  *sql.DB
	workspaceRepository repository.WorkspaceRepository
	paymentRepository   repository.PaymentRepository
}

func NewBillingService(db *sql.DB, wRepo repository.WorkspaceRepository, pRepo repository.PaymentRepository) *BillingService {
	return &BillingService{
		db:                  db,
		workspaceRepository: wRepo,
		paymentRepository:   pRepo,
	}
}

// ProcessTask routes to the correct logic based on the task type
func (s *BillingService) ProcessTask(task models.BillingTask) error {
	if task.BillingType == "annual" {
		return s.processAnnual(task)
	}
	return s.processMonthly(task)
}

func (s *BillingService) processMonthly(task models.BillingTask) error {
	logger := logrus.WithField("component", "monthly_billing").WithField("workspace_id", task.WorkspaceID)

	billingData, err := s.loadBillingData(task, "monthly", logger)
	if err != nil {
		return err
	}

	costs, err := s.calculateMonthlyCosts(billingData, logger)
	if err != nil {
		return err
	}

	invoiceID, err := s.createInvoice(costs, billingData, logger)
	if err != nil {
		return err
	}

	return s.chargeInvoice(invoiceID, costs, billingData, logger)
}

func (s *BillingService) loadBillingData(task models.BillingTask, billingType string, logger *logrus.Entry) (*BillingData, error) {
	conn := utils.NewDBConn(s.db)
	billingParams, err := conn.GetBillingParams()
	if err != nil {
		logger.WithError(err).Error("error getting billing params")
		return nil, err
	}

	now := time.Now()
	var billingPeriodStart time.Time
	if billingType == "annual" {
		billingPeriodStart = now.AddDate(-1, 0, 0)
	} else {
		billingPeriodStart = now.AddDate(0, -1, 0)
	}

	workspace, err := s.workspaceRepository.GetWorkspaceFromDB(task.WorkspaceID)
	if err != nil {
		logger.WithError(err).Error("error getting workspace")
		return nil, err
	}

	user, err := s.workspaceRepository.GetUserFromDB(task.CreatorID)
	if err != nil {
		logger.WithError(err).Error("error getting user")
		return nil, err
	}

	plans, err := s.paymentRepository.GetServicePlans()
	if err != nil {
		logger.WithError(err).Error("error getting service plans")
		return nil, err
	}

	plan := utils.GetPlan(plans, workspace)

	billingInfo, err := s.workspaceRepository.GetWorkspaceBillingInfo(workspace)
	if err != nil {
		logger.WithError(err).Error("error getting billing info")
		return nil, err
	}

	baseCosts, err := helpers.GetBaseCosts()
	if err != nil {
		logger.WithError(err).Error("error getting base costs")
		return nil, err
	}

	return &BillingData{
		BillingParams:       billingParams,
		Workspace:           workspace,
		User:                user,
		Plan:                plan,
		BillingInfo:         billingInfo,
		BaseCosts:           baseCosts,
		BillingPeriodStart:  billingPeriodStart,
		BillingPeriodEnd:    now,
		Now:                 now,
	}, nil
}

func (s *BillingService) calculateMonthlyCosts(data *BillingData, logger *logrus.Entry) (*BillingCosts, error) {
	costs := &BillingCosts{}
	userCount := utils.GetWorkspaceUserCount(s.db, data.Workspace.Id)
	logger.Infof("Workspace total user count %d", userCount)

	costs.MembershipCosts = data.Plan.BaseCosts * float64(userCount)
	logger.Infof("Workspace total membership costs is %f", costs.MembershipCosts)

	utils.CreateMonthlyNumberRentalDebit(s.db, data.Workspace.Id, data.User.Id, data.BillingPeriodStart)

	billingPeriodStartStr := data.BillingPeriodStart.Format(time.DateTime)
	billingPeriodEndStr := data.BillingPeriodEnd.Format(time.DateTime)

	debitsErr := s.processDebits(data, costs, billingPeriodStartStr, billingPeriodEndStr, logger)
	if debitsErr != nil {
		return nil, debitsErr
	}

	recordingsErr := s.processRecordings(data, costs, billingPeriodStartStr, billingPeriodEndStr, logger)
	if recordingsErr != nil {
		return nil, recordingsErr
	}

	faxesErr := s.processFaxes(data, costs, billingPeriodStartStr, billingPeriodEndStr, logger)
	if faxesErr != nil {
		return nil, faxesErr
	}

	costs.TotalCosts = costs.MembershipCosts + costs.CallTollsCosts + costs.RecordingCosts + costs.FaxCosts + costs.NumberRentalCosts
	costs.InvoiceDesc = fmt.Sprintf("LineBlocs invoice for %s", data.BillingInfo.InvoiceDue)

	logger.Infof("Final costs are membership: %f, call tolls: %f, recordings: %f, fax: %f, did rentals: %f, total: %f (cents)",
		costs.MembershipCosts, costs.CallTollsCosts, costs.RecordingCosts, costs.FaxCosts, costs.NumberRentalCosts, costs.TotalCosts)

	return costs, nil
}

func (s *BillingService) processDebits(data *BillingData, costs *BillingCosts, startStr, endStr string, logger *logrus.Entry) error {
	rows, err := s.db.Query("SELECT id, source, module_id, cents, created_at FROM users_debits WHERE user_id = ? AND created_at BETWEEN ? AND ?", data.Workspace.CreatorId, startStr, endStr)
	if err != nil {
		logger.WithError(err).Error("error running debits query")
		return err
	}
	defer rows.Close()

	remainingMinutes := data.Plan.MinutesPerMonth

	for rows.Next() {
		var debitID int
		var debitSource string
		var debitModuleID int
		var debitCostCents float64
		var debitCreatedAt time.Time

		if err := rows.Scan(&debitID, &debitSource, &debitModuleID, &debitCostCents, &debitCreatedAt); err != nil {
			logger.WithError(err).Error("error scanning debit")
			continue
		}

		switch debitSource {
		case "CALL":
			s.processCallDebit(data, costs, debitModuleID, debitCostCents, &remainingMinutes, logger)
		case "NUMBER_RENTAL":
			s.processNumberRentalDebit(data, costs, debitModuleID, logger)
		}
	}

	return nil
}

func (s *BillingService) processCallDebit(data *BillingData, costs *BillingCosts, moduleID int, costCents float64, remainingMinutes *float64, logger *logrus.Entry) {
	call, err := s.workspaceRepository.GetCallFromDB(moduleID)
	if err != nil {
		logger.WithError(err).Error("error getting call")
		return
	}

	callDurationMinutes := float64(call.DurationNumber / 60)
	logger.Infof("processing call with duration %d seconds", call.DurationNumber)

	charge, err := utils.ComputeAmountToCharge(costCents, *remainingMinutes, callDurationMinutes)
	if err != nil {
		logger.WithError(err).Error("error computing charge")
		return
	}

	costs.CallTollsCosts += charge
	*remainingMinutes -= callDurationMinutes
}

func (s *BillingService) processNumberRentalDebit(data *BillingData, costs *BillingCosts, moduleID int, logger *logrus.Entry) {
	did, err := s.workspaceRepository.GetDIDFromDB(moduleID)
	if err != nil {
		logger.WithError(err).Error("error getting DID")
		return
	}

	logger.Infof("processing DID rental with monthly cost %d", did.MonthlyCost)
	costs.NumberRentalCosts += float64(did.MonthlyCost)
}

func (s *BillingService) processRecordings(data *BillingData, costs *BillingCosts, startStr, endStr string, logger *logrus.Entry) error {
	rows, err := s.db.Query("SELECT id, size, created_at FROM recordings WHERE user_id = ? AND created_at BETWEEN ? AND ?", data.Workspace.CreatorId, startStr, endStr)
	if err != sql.ErrNoRows && err != nil {
		logger.WithError(err).Error("error running recordings query")
		return err
	}
	defer rows.Close()

	remainingRecordings := data.Plan.RecordingSpace

	for rows.Next() {
		var recordingID int
		var recordingSizeBytes float64
		var recordingCreatedAt time.Time

		if err := rows.Scan(&recordingID, &recordingSizeBytes, &recordingCreatedAt); err != nil {
			logger.WithError(err).Error("error scanning recording")
			continue
		}

		recordingCentsPerByte := math.Round(data.BaseCosts.RecordingsPerByte * recordingSizeBytes)
		charge, err := utils.ComputeAmountToCharge(recordingCentsPerByte, remainingRecordings, recordingSizeBytes)
		if err != nil {
			logger.WithError(err).Error("error calculating recording charge")
			continue
		}

		costs.RecordingCosts += charge
		remainingRecordings -= recordingSizeBytes
	}

	return nil
}

func (s *BillingService) processFaxes(data *BillingData, costs *BillingCosts, startStr, endStr string, logger *logrus.Entry) error {
	rows, err := s.db.Query("SELECT id, created_at FROM faxes WHERE workspace_id = ? AND created_at BETWEEN ? AND ?", data.Workspace.Id, startStr, endStr)
	if err != sql.ErrNoRows && err != nil {
		logger.WithError(err).Error("error running faxes query")
		return err
	}
	defer rows.Close()

	remainingFaxUnits := data.Plan.Fax

	for rows.Next() {
		var faxID int
		var faxCreatedAt time.Time

		if err := rows.Scan(&faxID, &faxCreatedAt); err != nil {
			logger.WithError(err).Error("error scanning fax")
			continue
		}

		planFaxLimit := float64(data.Plan.Fax)
		faxCentsPerUnit := data.BaseCosts.FaxPerUsed
		charge, err := utils.ComputeAmountToCharge(faxCentsPerUnit, float64(remainingFaxUnits), planFaxLimit)
		if err != nil {
			logger.WithError(err).Error("error calculating fax charge")
			continue
		}

		costs.FaxCosts += charge
		remainingFaxUnits--
	}

	return nil
}

func (s *BillingService) createInvoice(costs *BillingCosts, data *BillingData, logger *logrus.Entry) (int64, error) {
	logger.Infof("Creating invoice for user %d, on workspace %d, plan type %s", data.User.Id, data.Workspace.Id, data.Workspace.Plan)

	insertStmt, err := s.db.Prepare("INSERT INTO users_invoices (`cents`, `call_costs`, `recording_costs`, `fax_costs`, `membership_costs`, `number_costs`, `status`, `user_id`, `workspace_id`, `created_at`, `updated_at`) VALUES ( ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		logger.WithError(err).Error("could not prepare invoice insert query")
		return 0, err
	}
	defer insertStmt.Close()

	result, err := insertStmt.Exec(costs.TotalCosts, costs.CallTollsCosts, costs.RecordingCosts, costs.FaxCosts, costs.MembershipCosts, costs.NumberRentalCosts, "INCOMPLETE", data.Workspace.CreatorId, data.Workspace.Id, data.Now, data.Now)
	if err != nil {
		logger.WithError(err).Error("error creating invoice")
		return 0, err
	}

	invoiceID, err := result.LastInsertId()
	if err != nil {
		logger.WithError(err).Error("could not get insert id")
		return 0, err
	}

	return invoiceID, nil
}

func (s *BillingService) chargeInvoice(invoiceID int64, costs *BillingCosts, data *BillingData, logger *logrus.Entry) error {
	logger.Infof("Charging user %d, on workspace %d, plan type %s", data.User.Id, data.Workspace.Id, data.Workspace.Plan)

	if data.Plan.PayAsYouGo {
		return s.chargeWithCredits(invoiceID, costs, data, logger)
	}
	return s.chargeWithCard(invoiceID, costs, data, logger)
}

func (s *BillingService) chargeWithCredits(invoiceID int64, costs *BillingCosts, data *BillingData, logger *logrus.Entry) error {
	remainingBalance := data.BillingInfo.RemainingBalanceCents

	if remainingBalance >= costs.TotalCosts {
		return s.chargeCreditsOnly(invoiceID, costs.TotalCosts, logger)
	}

	return s.markInvoiceChargeIncomplete(invoiceID, logger)
}

func (s *BillingService) chargeCreditsOnly(invoiceID int64, totalCosts float64, logger *logrus.Entry) error {
	logger.Info("User has enough credits. Charging balance")

	confNumber, err := utils.CreateInvoiceConfirmationNumber()
	if err != nil {
		logger.WithError(err).Error("error generating confirmation number")
		return err
	}

	updateStmt, err := s.db.Prepare("UPDATE users_invoices SET status = 'COMPLETE', source ='CREDITS', cents_collected = ?, confirmation_number = ? WHERE id = ?")
	if err != nil {
		logger.WithError(err).Error("could not prepare update query")
		return err
	}
	defer updateStmt.Close()

	_, err = updateStmt.Exec(totalCosts, confNumber, invoiceID)
	if err != nil {
		logger.WithError(err).Error("error updating invoice")
		return err
	}

	return nil
}



func (s *BillingService) chargeWithCard(invoiceID int64, costs *BillingCosts, data *BillingData, logger *logrus.Entry) error {
	logger.Info("Charging recurringly with card")

	cardChargeAmount := int(math.Ceil(costs.TotalCosts))
	invoice := models.UserInvoice{
		Id:          int(invoiceID),
		Cents:       cardChargeAmount,
		InvoiceDesc: costs.InvoiceDesc,
	}

	err := s.paymentRepository.ChargeCustomer(data.BillingParams.(*utils.BillingParams), data.User, data.Workspace, &invoice)
	if err != nil {
		logger.WithError(err).Error("error charging user")
		return s.markInvoiceChargeIncomplete(invoiceID, logger)
	}

	return s.markInvoiceChargeSuccess(invoiceID, costs.TotalCosts, logger)
}

func (s *BillingService) markInvoiceSuccess(invoiceID int64, totalCosts float64, now time.Time, logger *logrus.Entry) error {
	successStmt, err := s.db.Prepare("UPDATE users_invoices SET status = 'COMPLETE', source ='CARD', cents_collected = ?, last_attempted = ?, num_attempts = 1 WHERE id = ?")
	if err != nil {
		logger.WithError(err).Error("could not prepare update query")
		return err
	}
	defer successStmt.Close()

	_, err = successStmt.Exec(totalCosts, now, invoiceID)
	if err != nil {
		logger.WithError(err).Error("error updating invoice")
		return err
	}

	return nil
}

func (s *BillingService) markInvoiceFailed(invoiceID int64, now time.Time, logger *logrus.Entry) error {
	failStmt, err := s.db.Prepare("UPDATE users_invoices SET source = 'CARD', status = 'INCOMPLETE', num_attempts = 1, last_attempted = ? WHERE id = ?")
	if err != nil {
		logger.WithError(err).Error("could not prepare update query")
		return err
	}
	defer failStmt.Close()

	_, err = failStmt.Exec(now, invoiceID)
	if err != nil {
		logger.WithError(err).Error("error updating invoice")
		return err
	}

	return nil
}

func (s *BillingService) markInvoiceChargeIncomplete(invoiceID int64, logger *logrus.Entry) error {
	updateStmt, err := s.db.Prepare("UPDATE users_invoices SET status = 'INCOMPLETE', source = 'CARD', cents_collected = 0.0 WHERE id = ?")
	if err != nil {
		logger.WithError(err).Error("could not prepare update query")
		return err
	}
	defer updateStmt.Close()

	_, err = updateStmt.Exec(invoiceID)
	if err != nil {
		logger.WithError(err).Error("error updating invoice")
		return err
	}

	return nil
}

func (s *BillingService) markInvoiceChargeSuccess(invoiceID int64, totalCosts float64, logger *logrus.Entry) error {
	confirmNumber, err := utils.CreateInvoiceConfirmationNumber()
	if err != nil {
		logger.WithError(err).Error("error generating confirmation number")
		return err
	}

	finalStmt, err := s.db.Prepare("UPDATE users_invoices SET status = 'COMPLETE', source ='CARD', cents_collected = ?, confirmation_number = ? WHERE id = ?")
	if err != nil {
		logger.WithError(err).Error("could not prepare update query")
		return err
	}
	defer finalStmt.Close()

	_, err = finalStmt.Exec(totalCosts, confirmNumber, invoiceID)
	if err != nil {
		logger.WithError(err).Error("error updating invoice")
		return err
	}

	return nil
}

func (s *BillingService) processAnnual(task models.BillingTask) error {
	conn := utils.NewDBConn(s.db)
	logger := logrus.WithField("component", "annual_billing").WithField("workspace_id", task.WorkspaceID)

	billingParams, err := conn.GetBillingParams()
	if err != nil {
		logger.WithError(err).Error("error getting billing params")
		return err
	}

	now := time.Now()
	billingPeriodStart := now.AddDate(-1, 0, 0)
	billingPeriodEnd := now
	billingPeriodStartStr := billingPeriodStart.Format(time.DateTime)
	billingPeriodEndStr := billingPeriodEnd.Format(time.DateTime)

	workspace, err := s.workspaceRepository.GetWorkspaceFromDB(task.WorkspaceID)
	if err != nil {
		logger.WithError(err).Error("error getting workspace")
		return err
	}

	user, err := s.workspaceRepository.GetUserFromDB(task.CreatorID)
	if err != nil {
		logger.WithError(err).Error("error getting user")
		return err
	}

	plans, err := s.paymentRepository.GetServicePlans()
	if err != nil {
		logger.WithError(err).Error("error getting service plans")
		return err
	}

	plan := utils.GetPlan(plans, workspace)

	billingInfo, err := s.workspaceRepository.GetWorkspaceBillingInfo(workspace)
	if err != nil {
		logger.WithError(err).Error("error getting billing info")
		return err
	}

	baseCosts, err := helpers.GetBaseCosts()
	if err != nil {
		logger.WithError(err).Error("error getting base costs")
		return err
	}

	userCount := utils.GetWorkspaceUserCount(s.db, workspace.Id)
	logger.Infof("Workspace total user count %d", userCount)

	totalCosts := 0.0
	annualMembershipCosts := plan.BaseCosts * float64(userCount) * 12.0
	callTollsCosts := 0.0
	recordingCosts := 0.0
	faxCosts := 0.0
	numberRentalCosts := 0.0
	invoiceDesc := fmt.Sprintf("LineBlocs annual invoice for %s", billingInfo.InvoiceDue)

	logger.Infof("Workspace total annual membership costs is %f", annualMembershipCosts)

	debitsRows, err := s.db.Query(
		"SELECT id, source, module_id, cents, created_at FROM users_debits WHERE user_id = ? AND created_at BETWEEN ? AND ?",
		workspace.CreatorId, billingPeriodStartStr, billingPeriodEndStr,
	)
	if err != nil {
		logger.WithError(err).Error("error running debits query")
		return err
	}
	defer debitsRows.Close()

    var debitID int
    var debitSource string
    var debitModuleID int
    var debitCostCents float64
    var debitCreatedAt time.Time

    remainingAnnualMinutes := plan.MinutesPerMonth * 12
    remainingAnnualRecordings := plan.RecordingSpace * 12
    remainingAnnualFaxUnits := plan.Fax * 12

    for debitsRows.Next() {
        if err := debitsRows.Scan(&debitID, &debitSource, &debitModuleID, &debitCostCents, &debitCreatedAt); err != nil {
            logger.WithError(err).Error("error scanning debit row")
            continue
        }

        switch debitSource {
        case "CALL":
            call, err := s.workspaceRepository.GetCallFromDB(debitModuleID)
            if err != nil {
                logger.WithError(err).Error("error getting call")
                continue
            }

            callDurationMinutes := float64(call.DurationNumber) / 60.0
            charge, err := utils.ComputeAmountToCharge(debitCostCents, remainingAnnualMinutes, callDurationMinutes)
            if err != nil {
                logger.WithError(err).Error("error computing call charge")
                continue
            }

            callTollsCosts += charge
            remainingAnnualMinutes -= callDurationMinutes

        case "NUMBER_RENTAL":
            did, err := s.workspaceRepository.GetDIDFromDB(debitModuleID)
            if err != nil {
                logger.WithError(err).Error("error getting DID")
                continue
            }
            numberRentalCosts += float64(did.MonthlyCost)
        }
    }

	recordingsRows, err := s.db.Query(
		"SELECT id, size, created_at FROM recordings WHERE user_id = ? AND created_at BETWEEN ? AND ?",
		workspace.CreatorId, billingPeriodStartStr, billingPeriodEndStr,
	)
	if err != sql.ErrNoRows && err != nil {
		logger.WithError(err).Error("error running recordings query")
		return err
	}
	defer recordingsRows.Close()

    var recordingID int
    var recordingSizeBytes float64
    var recordingCreatedAt time.Time

    for recordingsRows.Next() {
        if err := recordingsRows.Scan(&recordingID, &recordingSizeBytes, &recordingCreatedAt); err != nil {
            logger.WithError(err).Error("error scanning recording row")
            continue
        }

        recordingCentsPerByte := math.Round(baseCosts.RecordingsPerByte * recordingSizeBytes)
        charge, err := utils.ComputeAmountToCharge(recordingCentsPerByte, remainingAnnualRecordings, recordingSizeBytes)
        if err != nil {
            logger.WithError(err).Error("error calculating recording charge")
            continue
        }

        recordingCosts += charge
        remainingAnnualRecordings -= recordingSizeBytes
    }

	faxesRows, err := s.db.Query(
		"SELECT id, created_at FROM faxes WHERE workspace_id = ? AND created_at BETWEEN ? AND ?",
		workspace.Id, billingPeriodStartStr, billingPeriodEndStr,
	)
	if err != sql.ErrNoRows && err != nil {
		logger.WithError(err).Error("error running faxes query")
		return err
	}
	defer faxesRows.Close()

    var faxID int
    var faxCreatedAt time.Time

    for faxesRows.Next() {
        if err := faxesRows.Scan(&faxID, &faxCreatedAt); err != nil {
            logger.WithError(err).Error("error scanning fax row")
            continue
        }

        faxCentsPerUnit := baseCosts.FaxPerUsed
        charge, err := utils.ComputeAmountToCharge(faxCentsPerUnit, float64(remainingAnnualFaxUnits), float64(plan.Fax*12))
        if err != nil {
            logger.WithError(err).Error("error calculating fax charge")
            continue
        }

        faxCosts += charge
        remainingAnnualFaxUnits--
    }

    totalCosts = annualMembershipCosts + callTollsCosts + recordingCosts + faxCosts + numberRentalCosts

    logger.Infof(
        "Final annual costs are membership: %f, call tolls: %f, recordings: %f, fax: %f, did rentals: %f, total: %f (cents)",
        annualMembershipCosts, callTollsCosts, recordingCosts, faxCosts, numberRentalCosts, totalCosts,
    )

    insertStmt, err := s.db.Prepare("INSERT INTO users_invoices (`cents`, `call_costs`, `recording_costs`, `fax_costs`, `membership_costs`, `number_costs`, `status`, `user_id`, `workspace_id`, `created_at`, `updated_at`) VALUES ( ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
    if err != nil {
        logger.WithError(err).Error("could not prepare invoice insert query")
        return err
    }
    defer insertStmt.Close()

    result, err := insertStmt.Exec(totalCosts, callTollsCosts, recordingCosts, faxCosts, annualMembershipCosts, numberRentalCosts, "INCOMPLETE", workspace.CreatorId, workspace.Id, now, now)
    if err != nil {
        logger.WithError(err).Error("error creating invoice")
        return err
    }

    invoiceID, err := result.LastInsertId()
    if err != nil {
        logger.WithError(err).Error("could not get insert id")
        return err
    }

    if plan.PayAsYouGo {
        remainingBalance := billingInfo.RemainingBalanceCents
        balanceAfterCharge := remainingBalance - totalCosts
        chargeAmount, err := utils.ComputeAmountToCharge(totalCosts, remainingBalance, balanceAfterCharge)
        if err != nil {
            logger.WithError(err).Error("error calculating charge amount")
            return err
        }

        if remainingBalance >= totalCosts {
            confNumber, err := utils.CreateInvoiceConfirmationNumber()
            if err != nil {
                logger.WithError(err).Error("error generating confirmation number")
                return err
            }

            updateStmt, err := s.db.Prepare("UPDATE users_invoices SET status = 'COMPLETE', source ='CREDITS', cents_collected = ?, confirmation_number = ? WHERE id = ?")
            if err != nil {
                logger.WithError(err).Error("could not prepare update query")
                return err
            }
            defer updateStmt.Close()
            _, err = updateStmt.Exec(totalCosts, confNumber, invoiceID)
            if err != nil {
                logger.WithError(err).Error("error updating invoice")
                return err
            }
        } else {
            updateStmt, err := s.db.Prepare("UPDATE users_invoices SET status = 'INCOMPLETE', source ='CREDITS', cents_collected = ? WHERE id = ?")
            if err != nil {
                logger.WithError(err).Error("could not prepare update query")
                return err
            }
            defer updateStmt.Close()
            _, err = updateStmt.Exec(chargeAmount, invoiceID)
            if err != nil {
                logger.WithError(err).Error("error updating invoice")
                return err
            }

            cardChargeAmount := int(math.Ceil(chargeAmount))
            invoice := models.UserInvoice{
                Id:          int(invoiceID),
                Cents:       cardChargeAmount,
                InvoiceDesc: invoiceDesc,
            }

            err = s.paymentRepository.ChargeCustomer(billingParams, user, workspace, &invoice)
            if err != nil {
                logger.WithError(err).Error("error charging customer card")
                failStmt, err := s.db.Prepare("UPDATE users_invoices SET source = 'CARD', status = 'INCOMPLETE', num_attempts = 1, last_attempted = ? WHERE id = ?")
                if err != nil {
                    logger.WithError(err).Error("could not prepare update query")
                    return err
                }
                defer failStmt.Close()
                _, err = failStmt.Exec(now, invoiceID)
                if err != nil {
                    logger.WithError(err).Error("error updating invoice")
                    return err
                }
                return err
            }

            successStmt, err := s.db.Prepare("UPDATE users_invoices SET status = 'COMPLETE', source ='CARD', cents_collected = ?, last_attempted = ?, num_attempts = 1 WHERE id = ?")
            if err != nil {
                logger.WithError(err).Error("could not prepare update query")
                return err
            }
            defer successStmt.Close()
            _, err = successStmt.Exec(totalCosts, now, invoiceID)
            if err != nil {
                logger.WithError(err).Error("error updating invoice")
                return err
            }
        }
    } else {
        cardChargeAmount := int(math.Ceil(totalCosts))
        invoice := models.UserInvoice{
            Id:          int(invoiceID),
            Cents:       cardChargeAmount,
            InvoiceDesc: invoiceDesc,
        }

        err := s.paymentRepository.ChargeCustomer(billingParams, user, workspace, &invoice)
        if err != nil {
            logger.WithError(err).Error("error charging user")
            updateStmt, err := s.db.Prepare("UPDATE users_invoices SET status = 'INCOMPLETE', source = 'CARD', cents_collected = 0.0 WHERE id = ?")
            if err != nil {
                logger.WithError(err).Error("could not prepare update query")
                return err
            }
            defer updateStmt.Close()
            _, err = updateStmt.Exec(invoiceID)
            if err != nil {
                logger.WithError(err).Error("error updating invoice")
                return err
            }
            return err
        }

        confirmNumber, err := utils.CreateInvoiceConfirmationNumber()
        if err != nil {
            logger.WithError(err).Error("error generating confirmation number")
            return err
        }

        finalStmt, err := s.db.Prepare("UPDATE users_invoices SET status = 'COMPLETE', source ='CARD', cents_collected = ?, confirmation_number = ? WHERE id = ?")
        if err != nil {
            logger.WithError(err).Error("could not prepare update query")
            return err
        }
        defer finalStmt.Close()
        _, err = finalStmt.Exec(totalCosts, confirmNumber, invoiceID)
        if err != nil {
            logger.WithError(err).Error("error updating invoice")
            return err
        }
    }

    return nil
}