package billing

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"time"

	helpers "github.com/Lineblocs/go-helpers"
	"github.com/sirupsen/logrus"
	"lineblocs.com/scheduler/models"
	"lineblocs.com/scheduler/repository"
	"lineblocs.com/scheduler/utils"
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
	MembershipCosts   int64
	CallTollsCosts    int64
	RecordingCosts    int64
	FaxCosts          int64
	NumberRentalCosts int64
	TotalCosts        int64
	InvoiceDesc       string
}

type BillingService struct {
	db                  *sql.DB
	workspaceRepository repository.WorkspaceRepository
	paymentRepository   repository.PaymentRepository
	rabbitmqPublisher   RabbitMQPublisher
}

type RabbitMQPublisher interface {
	Publish(queue string, message []byte) error
}


func NewBillingService(db *sql.DB, wRepo repository.WorkspaceRepository, pRepo repository.PaymentRepository) *BillingService {
	return &BillingService{
		db:                  db,
		workspaceRepository: wRepo,
		paymentRepository:   pRepo,
	}
}

func NewBillingServiceWithPublisher(db *sql.DB, wRepo repository.WorkspaceRepository, pRepo repository.PaymentRepository, publisher RabbitMQPublisher) *BillingService {
	return &BillingService{
		db:                  db,
		workspaceRepository: wRepo,
		paymentRepository:   pRepo,
		rabbitmqPublisher:   publisher,
	}
}

func (s *BillingService) publishFailedPayment(task models.BillingTask, reason string, logger *logrus.Entry) {
	if s.rabbitmqPublisher == nil {
		return
	}

	failedTask := models.FailedBillingTask{
		RunID:          task.RunID,
		WorkspaceID:    task.WorkspaceID,
		SubscriptionID: task.SubscriptionID,
		CreatorID:      task.CreatorID,
		Reason:         reason,
	}

	messageBytes, err := json.Marshal(failedTask)
	if err != nil {
		logger.WithError(err).Error("error marshaling failed billing task")
		return
	}

	err = s.rabbitmqPublisher.Publish("failed_payments", messageBytes)
	if err != nil {
		logger.WithError(err).Error("error publishing failed payment event")
		return
	}

	logger.Infof("Published failed payment event for workspace %d, subscription %d", task.WorkspaceID, task.SubscriptionID)
}

func (s *BillingService) publishPaymentReceipt(task models.BillingTask, paymentAmount int64, cardLast4 string, cardBrand string, logger *logrus.Entry) {
	if s.rabbitmqPublisher == nil {
		return
	}

	receiptTask := models.PaymentReceiptTask{
		RunID:          task.RunID,
		WorkspaceID:    task.WorkspaceID,
		SubscriptionID: task.SubscriptionID,
		CreatorID:      task.CreatorID,
		CardLast4:      cardLast4,
		CardBrand:      cardBrand,
		PaymentAmount:  float64(paymentAmount) / 100.0,
		Timestamp:      time.Now().Unix(),
	}

	messageBytes, err := json.Marshal(receiptTask)
	if err != nil {
		logger.WithError(err).Error("error marshaling payment receipt task")
		return
	}

	err = s.rabbitmqPublisher.Publish("payment_receipts", messageBytes)
	if err != nil {
		logger.WithError(err).Error("error publishing payment receipt event")
		return
	}

	logger.Infof("Published payment receipt event for workspace %d, subscription %d, amount: %d cents", task.WorkspaceID, task.SubscriptionID, paymentAmount)
}

// ProcessTask routes to the correct logic based on the task type
func (s *BillingService) ProcessTask(task models.BillingTask) error {
	logger := logrus.WithField("component", "billing").WithField("workspace_id", task.WorkspaceID).WithField("run_id", task.RunID)
	if task.BillingType == "annual" {
		err := s.processAnnual(task, logger)
		if err != nil {
			s.publishFailedPayment(task, err.Error(), logger)
		}
		return err
	}
	err := s.processMonthly(task, logger)
	if err != nil {
		s.publishFailedPayment(task, err.Error(), logger)
	}
	return err
}

func (s *BillingService) processMonthly(task models.BillingTask, logger *logrus.Entry) error {
	billingData, err := s.loadBillingData(task, "MONTHLY", logger)
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

	return s.chargeInvoice(invoiceID, costs, billingData, task, logger)
}


func (s *BillingService) loadBillingData(task models.BillingTask, billingType string, logger *logrus.Entry) (*BillingData, error) {
	conn := utils.NewDBConn(s.db)

	subscription, err := s.paymentRepository.GetSubscription(task.SubscriptionID)
	if err != nil {
		logger.WithError(err).Error("error getting subscription")
		return nil, err
	}
	logger.Infof("Loaded subscription %d for billing task", subscription.Id)

	billingParams, err := conn.GetBillingParams()
	if err != nil {
		logger.WithError(err).Error("error getting billing params")
		return nil, err
	}

	now := time.Now()
	var billingPeriodStart time.Time
	if billingType == "ANNUAL" {
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

	plan := utils.GetPlanBySubscription(plans, subscription)
	if plan == nil {
		logger.Error("plan is nil")
		return nil, fmt.Errorf("plan not found for subscription")
	}

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

	costs.MembershipCosts = int64(data.Plan.BaseCosts * float64(userCount))
	logger.Infof("Workspace total membership costs is %d", costs.MembershipCosts)

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

	logger.Infof("Final costs are membership: %d, call tolls: %d, recordings: %d, fax: %d, did rentals: %d, total: %d (cents)",
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
		var debitCostCents int64
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

func (s *BillingService) processCallDebit(data *BillingData, costs *BillingCosts, moduleID int, costCents int64, remainingMinutes *float64, logger *logrus.Entry) {
	call, err := s.workspaceRepository.GetCallFromDB(moduleID)
	if err != nil {
		logger.WithError(err).Error("error getting call")
		return
	}

	callDurationMinutes := float64(call.DurationNumber / 60)
	logger.Infof("processing call with duration %d seconds", call.DurationNumber)

	charge, err := utils.ComputeAmountToCharge(float64(costCents), *remainingMinutes, callDurationMinutes)
	if err != nil {
		logger.WithError(err).Error("error computing charge")
		return
	}

	costs.CallTollsCosts += int64(charge)
	*remainingMinutes -= callDurationMinutes
}

func (s *BillingService) processNumberRentalDebit(data *BillingData, costs *BillingCosts, moduleID int, logger *logrus.Entry) {
	did, err := s.workspaceRepository.GetDIDFromDB(moduleID)
	if err != nil {
		logger.WithError(err).Error("error getting DID")
		return
	}

	logger.Infof("processing DID rental with monthly cost %d", did.MonthlyCost)
	costs.NumberRentalCosts += int64(did.MonthlyCost)
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

		recordingCentsPerByte := int64(math.Round(data.BaseCosts.RecordingsPerByte * recordingSizeBytes))
		charge, err := utils.ComputeAmountToCharge(float64(recordingCentsPerByte), remainingRecordings, recordingSizeBytes)
		if err != nil {
			logger.WithError(err).Error("error calculating recording charge")
			continue
		}

		costs.RecordingCosts += int64(charge)
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

		costs.FaxCosts += int64(charge)
		remainingFaxUnits--
	}

	return nil
}

func (s *BillingService) createInvoice(costs *BillingCosts, data *BillingData, logger *logrus.Entry) (int64, error) {
	logger.Infof("Creating invoice for user %d, on workspace %d, plan type %s", data.User.Id, data.Workspace.Id, data.Workspace.Plan)

	insertStmt, err := s.db.Prepare("INSERT INTO users_invoices (`cents`, `cents_including_taxes`, `call_costs`, `recording_costs`, `fax_costs`, `membership_costs`, `number_costs`, `status`, `user_id`, `workspace_id`, `created_at`, `updated_at`, `source`, `tax_metadata`) VALUES ( ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		logger.WithError(err).Error("could not prepare invoice insert query")
		return 0, err
	}
	defer insertStmt.Close()

	source := "SUBSCRIPTION"
	taxMetadata := utils.CreateTaxMetadata(costs.CallTollsCosts, costs.RecordingCosts, costs.FaxCosts, costs.MembershipCosts, costs.NumberRentalCosts)
	helpers.Log(logrus.InfoLevel, fmt.Sprintf("Tax metadata for invoice: %s", taxMetadata))

	// implement code to calculate taxes here and add to cents_including_taxes when we have tax logic in place
	var centsIncludingTaxes int64
	var taxes int64
	taxes = 0
	centsIncludingTaxes = costs.TotalCosts + taxes
	result, err := insertStmt.Exec(costs.TotalCosts, centsIncludingTaxes, costs.CallTollsCosts, costs.RecordingCosts, costs.FaxCosts, costs.MembershipCosts, costs.NumberRentalCosts, "INCOMPLETE", data.Workspace.CreatorId, data.Workspace.Id, data.Now, data.Now, source, taxMetadata)
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

func (s *BillingService) chargeInvoice(invoiceID int64, costs *BillingCosts, data *BillingData, task models.BillingTask, logger *logrus.Entry) error {
	logger.Infof("Charging user %d, on workspace %d, plan type %s", data.User.Id, data.Workspace.Id, data.Workspace.Plan)

	if data.Plan.PayAsYouGo {
		return s.chargeWithCredits(invoiceID, costs, data, task, logger)
	}
	return s.chargeWithCard(invoiceID, costs, data, task, logger)
}

func (s *BillingService) chargeWithCredits(invoiceID int64, costs *BillingCosts, data *BillingData, task models.BillingTask, logger *logrus.Entry) error {
	remainingBalance := int64(data.BillingInfo.RemainingBalanceCents)

	if remainingBalance >= int64(costs.TotalCosts) {
		return s.chargeCreditsOnly(invoiceID, int64(costs.TotalCosts), data, task, logger)
	}

	logger.Warn("Insufficient credits for payment")
	return s.markInvoiceChargeIncomplete(invoiceID, logger)
}

func (s *BillingService) chargeCreditsOnly(invoiceID int64, totalCosts int64, data *BillingData, task models.BillingTask, logger *logrus.Entry) error {
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

	s.publishPaymentReceipt(task, totalCosts, "", "CREDITS", logger)

	return nil
}



func (s *BillingService) chargeWithCard(invoiceID int64, costs *BillingCosts, data *BillingData, task models.BillingTask, logger *logrus.Entry) error {
	logger.Info("Charging recurringly with card")

	cardChargeAmount := int(math.Ceil(float64(costs.TotalCosts)))
	cardChargeAmount = 200
	logger.Info(fmt.Sprintf("Total costs to charge on card is %d cents", cardChargeAmount))

	invoice := models.UserInvoice{
		Id:          int(invoiceID),
		Cents:       cardChargeAmount,
		InvoiceDesc: costs.InvoiceDesc,
	}

	chargeResult, err := s.paymentRepository.ChargeCustomer(data.BillingParams.(*utils.BillingParams), data.User, data.Workspace, &invoice)
	if err != nil {
		logger.WithError(err).Error("error charging user")
		s.markInvoiceChargeIncomplete(invoiceID, logger)
		return err
	}

	s.publishPaymentReceipt(task, int64(costs.TotalCosts), chargeResult.CardLast4, chargeResult.CardBrand, logger)

	return s.markInvoiceChargeSuccess(invoiceID, int64(costs.TotalCosts), logger)
}

func (s *BillingService) markInvoiceSuccess(invoiceID int64, totalCosts int64, now time.Time, logger *logrus.Entry) error {
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
	updateStmt, err := s.db.Prepare("UPDATE users_invoices SET status = 'INCOMPLETE' WHERE id = ?")
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

func (s *BillingService) markInvoiceChargeSuccess(invoiceID int64, totalCosts int64, logger *logrus.Entry) error {
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

func (s *BillingService) processAnnual(task models.BillingTask, logger *logrus.Entry) error {
	conn := utils.NewDBConn(s.db)

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

	subscription, err := s.paymentRepository.GetSubscription(task.SubscriptionID)
	if err != nil {
		logger.WithError(err).Error("error getting user")
		return err
	}

	plan := utils.GetPlanBySubscription(plans, subscription)
	if plan == nil {
		logger.Error("plan is nil")
		return fmt.Errorf("plan not found for subscription")
	}

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

	totalCosts := int64(0)
	annualMembershipCosts := int64(plan.BaseCosts * float64(userCount) * 12.0)
	callTollsCosts := int64(0)
	recordingCosts := int64(0)
	faxCosts := int64(0)
	numberRentalCosts := int64(0)
	invoiceDesc := fmt.Sprintf("LineBlocs annual invoice for %s", billingInfo.InvoiceDue)

	logger.Infof("Workspace total annual membership costs is %d", annualMembershipCosts)

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
    var debitCostCents int64
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
            charge, err := utils.ComputeAmountToCharge(float64(debitCostCents), remainingAnnualMinutes, callDurationMinutes)
            if err != nil {
                logger.WithError(err).Error("error computing call charge")
                continue
            }

            callTollsCosts += int64(charge)
            remainingAnnualMinutes -= callDurationMinutes

        case "NUMBER_RENTAL":
            did, err := s.workspaceRepository.GetDIDFromDB(debitModuleID)
            if err != nil {
                logger.WithError(err).Error("error getting DID")
                continue
            }
            numberRentalCosts += int64(did.MonthlyCost)
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

        recordingCentsPerByte := int64(math.Round(baseCosts.RecordingsPerByte * recordingSizeBytes))
        charge, err := utils.ComputeAmountToCharge(float64(recordingCentsPerByte), remainingAnnualRecordings, recordingSizeBytes)
        if err != nil {
            logger.WithError(err).Error("error calculating recording charge")
            continue
        }

        recordingCosts += int64(charge)
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

        faxCosts += int64(charge)
        remainingAnnualFaxUnits--
    }

    totalCosts = annualMembershipCosts + callTollsCosts + recordingCosts + faxCosts + numberRentalCosts

    logger.Infof(
        "Final annual costs are membership: %d, call tolls: %d, recordings: %d, fax: %d, did rentals: %d, total: %d (cents)",
        annualMembershipCosts, callTollsCosts, recordingCosts, faxCosts, numberRentalCosts, totalCosts,
    )

    annualCosts := &BillingCosts{
        MembershipCosts:   annualMembershipCosts,
        CallTollsCosts:    callTollsCosts,
        RecordingCosts:    recordingCosts,
        FaxCosts:          faxCosts,
        NumberRentalCosts: numberRentalCosts,
        TotalCosts:        totalCosts,
        InvoiceDesc:       invoiceDesc,
    }

    annualBillingData := &BillingData{
        Workspace:         workspace,
        User:              user,
        BillingInfo:       billingInfo,
        Now:               now,
    }

    invoiceID, err := s.createInvoice(annualCosts, annualBillingData, logger)
    if err != nil {
        return err
    }

    if plan.PayAsYouGo {
        remainingBalance := billingInfo.RemainingBalanceCents
        balanceAfterCharge := remainingBalance - int64(totalCosts)
        chargeAmount, err := utils.ComputeAmountToCharge(float64(totalCosts), float64(remainingBalance), float64(balanceAfterCharge))
        if err != nil {
            logger.WithError(err).Error("error calculating charge amount")
            return err
        }

        if remainingBalance >= int64(totalCosts) {
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
            _, err = updateStmt.Exec(int64(totalCosts), confNumber, invoiceID)
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
            _, err = updateStmt.Exec(int64(chargeAmount), invoiceID)
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

            chargeResult, err := s.paymentRepository.ChargeCustomer(billingParams, user, workspace, &invoice)
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

            s.publishPaymentReceipt(task, int64(totalCosts), chargeResult.CardLast4, chargeResult.CardBrand, logger)

            successStmt, err := s.db.Prepare("UPDATE users_invoices SET status = 'COMPLETE', source ='CARD', cents_collected = ?, last_attempted = ?, num_attempts = 1 WHERE id = ?")
            if err != nil {
                logger.WithError(err).Error("could not prepare update query")
                return err
            }
            defer successStmt.Close()
            _, err = successStmt.Exec(int64(totalCosts), now, invoiceID)
            if err != nil {
                logger.WithError(err).Error("error updating invoice")
                return err
            }
        }
    } else {
        cardChargeAmount := int(math.Ceil(float64(totalCosts)))
        invoice := models.UserInvoice{
            Id:          int(invoiceID),
            Cents:       cardChargeAmount,
            InvoiceDesc: invoiceDesc,
        }

        chargeResult, err := s.paymentRepository.ChargeCustomer(billingParams, user, workspace, &invoice)
        if err != nil {
            logger.WithError(err).Error("error charging user")
            updateStmt, err := s.db.Prepare("UPDATE users_invoices SET status = 'INCOMPLETE', source = 'CARD', cents_collected = 0 WHERE id = ?")
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

        s.publishPaymentReceipt(task, int64(totalCosts), chargeResult.CardLast4, chargeResult.CardBrand, logger)

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
        _, err = finalStmt.Exec(int64(totalCosts), confirmNumber, invoiceID)
        if err != nil {
            logger.WithError(err).Error("error updating invoice")
            return err
        }
    }

    return nil
}