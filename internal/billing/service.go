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
/*
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

func (s *BillingService) ProcessTask(task models.BillingTask) error {
    // ... logger setup ...
    var err error

    // Added logic: route to proration if the task action is 'immediate'
    if task.Action == models.ActionImmediate {
        err = s.processImmediateProrated(task, logger)
    } else if task.BillingType == "ANNUAL" {
        err = s.processAnnual(task, logger)
    } else {
        err = s.processMonthly(task, logger)
    }
    // ... error handling ...

    // Added logic: update the anchor date so the distributor doesn't double-bill
    return s.updateSubscriptionAnchor(task, logger)
}
*/

// --- CORE ROUTING (ProcessTask) ---

func (s *BillingService) ProcessTask(task models.BillingTask) error {
	logger := logrus.WithField("component", "billing").
		WithField("workspace_id", task.WorkspaceID).
		WithField("run_id", task.RunID).
		WithField("action", task.Action)

	var err error
	customizations, err := helpers.GetCustomizationKVs()
	if err != nil {
		logger.WithError(err).Error("error getting customization KVs")
		return err
	}

	// 1. Route based on Action (Immediate Signup/Upgrade vs. Regular Renewal)
	if task.Action == "immediate" {
		err = s.processImmediateProrated(task, customizations, logger)
	} else if task.BillingType == "ANNUAL" {
		err = s.processAnnual(task, customizations, logger)
	} else {
		err = s.processMonthly(task, customizations, logger)
	}

	if err != nil {
		s.publishFailedPayment(task, err.Error(), logger)
		return err
	}

	// 2. IMPORTANT: Move the 'next_billing_date' forward to the 1st of the next period
	// This prevents the Distributor from double-billing the user.
	return s.updateSubscriptionAnchor(task, logger)
}

// --- PRORATION & ANCHOR UPDATES ---
func (s *BillingService) processImmediateProrated(task models.BillingTask, customizations *helpers.CustomizationSettingsKV, logger *logrus.Entry) error {
	billingData, err := s.loadBillingData(task, task.BillingType, logger)
	if err != nil {
		return err
	}

	// Use the pre-calculated amount passed in the task for new signups
	costs := &BillingCosts{
		MembershipCosts: int64(task.Amount * 100),
		TotalCosts:      int64(task.Amount * 100),
		InvoiceDesc:     fmt.Sprintf("Initial prorated charge for %s plan", task.BillingType),
	}

	invoiceID, err := s.createInvoice(costs, billingData, logger)
	if err != nil {
		return err
	}

	return s.chargeInvoice(invoiceID, costs, billingData, task, logger)
}


func (s *BillingService) updateSubscriptionAnchor(task models.BillingTask, logger *logrus.Entry) error {
	var nextDate time.Time
	now := time.Now()

	// Calculate the next "Global 1st" anchor
	if task.BillingType == "ANNUAL" {
		nextDate = time.Date(now.Year()+1, 1, 1, 0, 0, 0, 0, now.Location())
	} else {
		nextDate = time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, now.Location())
	}

	_, err := s.db.Exec(`
		UPDATE subscriptions 
		SET next_billing_date = ?, 
		    last_billed_at = NOW(),
		    updated_at = NOW() 
		WHERE id = ?`, nextDate, task.SubscriptionID)

	if err != nil {
		logger.WithError(err).Error("failed to update next_billing_date anchor")
		return err
	}

	logger.Infof("Subscription %d anchor pushed to %s", task.SubscriptionID, nextDate.Format("2006-01-02"))
	return nil
}

func (s *BillingService) processMonthly(task models.BillingTask, customizations *helpers.CustomizationSettingsKV, logger *logrus.Entry) error {
	billingData, err := s.loadBillingData(task, "MONTHLY", logger)
	if err != nil {
		return err
	}

	//_ := (*customizations.Pairs["allow_billing_overage"]).(helpers.CustomizationBooleanValue).Value

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
	costs.MembershipCosts = int64(data.Plan.MonthlyCostCents * userCount)
	logger.Infof("Plan monthly cost per user: %d cents", data.Plan.MonthlyCostCents)
	logger.Infof("Workspace total membership costs is %d cents ($%.2f)", costs.MembershipCosts, float64(costs.MembershipCosts)/100.0)

	err := utils.CreateMonthlyNumberRentalDebit(s.db, data.Workspace.Id, data.User.Id, data.BillingPeriodStart)
	if err != nil {
		logger.WithError(err).Error("error creating monthly number rental debit")
		return nil, err
	}

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

	logger.Infof("Final monthly costs total in dollars: %.2f", float64(costs.TotalCosts)/100.0)
	logger.Infof("Final costs are membership: %d, call tolls: %d, recordings: %d, fax: %d, did rentals: %d, total: %d (cents)",
		costs.MembershipCosts, costs.CallTollsCosts, costs.RecordingCosts, costs.FaxCosts, costs.NumberRentalCosts, costs.TotalCosts)

	return costs, nil
}

func (s *BillingService) calculateAnnualCosts(data *BillingData, logger *logrus.Entry) (*BillingCosts, error) {
	costs := &BillingCosts{}
	userCount := utils.GetWorkspaceUserCount(s.db, data.Workspace.Id)
	logger.Infof("Workspace total user count %d", userCount)

	costs.MembershipCosts = int64(data.Plan.AnnualCostCents * userCount)
	logger.Infof("Plan annual cost per user: %d cents", data.Plan.AnnualCostCents)
	logger.Infof("Workspace total annual membership costs is %d cents ($%.2f)", costs.MembershipCosts, float64(costs.MembershipCosts)/100.0)
	// Annual billing does not charge for number rentals

	costs.CallTollsCosts = 0
	costs.RecordingCosts = 0
	costs.FaxCosts = 0
	costs.NumberRentalCosts = 0

	costs.TotalCosts = costs.MembershipCosts + costs.CallTollsCosts + costs.RecordingCosts + costs.FaxCosts + costs.NumberRentalCosts
	costs.InvoiceDesc = fmt.Sprintf("LineBlocs annual invoice for %s", data.BillingInfo.InvoiceDue)

	logger.Infof("Final annual costs are membership: %d, call tolls: %d, recordings: %d, fax: %d, did rentals: %d, total: %d (cents)", costs.MembershipCosts, costs.CallTollsCosts, costs.RecordingCosts, costs.FaxCosts, costs.NumberRentalCosts, costs.TotalCosts)
	logger.Infof("Final annual costs total in dollars: %.2f", float64(costs.TotalCosts)/100.0)	
	return costs, nil
}

func (s *BillingService) processDebits(data *BillingData, costs *BillingCosts, startStr, endStr string, logger *logrus.Entry) error {
	rows, err := s.db.Query("SELECT id, source, module_id, cents, created_at FROM users_debits WHERE workspace_id = ? AND created_at BETWEEN DATE(?) AND DATE(?)", data.Workspace.Id, startStr, endStr)
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
	logger.Infof("processing call from %s to %s, direction %s, duration %d seconds", call.From, call.To, call.Direction, call.DurationNumber)

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

	logger.Infof("processing DID rental with monthly cost %d cents for DID number %s", did.MonthlyCost, did.Number)
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
	deduplicationKey := utils.GenerateDeduplicationKey("INVOICE", data.BillingPeriodStart.Year(), int(data.BillingPeriodStart.Month()), data.BillingPeriodStart.Day(), data.Workspace.Id, 0)
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM users_invoices WHERE deduplication_key = ?", deduplicationKey).Scan(&count)
	if err != nil {
		return 0, err
	}
	if count > 0 {
		logger.Infof("Deduplication key %s already exists, skipping invoice creation.", deduplicationKey)
		return 0, fmt.Errorf("duplicate invoice creation attempt")
	}

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
	logger.Infof("Creating invoice line items for invoice %d", invoiceID)

	lineItems := []struct {
		name   string
		cents  float64
		keyName string
	}{
		{"Call Tolls", float64(costs.CallTollsCosts), "call_tolls"},
		{"Recording Storage", float64(costs.RecordingCosts), "recording_storage"},
		{"Fax Services", float64(costs.FaxCosts), "fax_services"},
		{"DID Rental", float64(costs.NumberRentalCosts), "did_rental"},
		{"Membership", float64(costs.MembershipCosts), "membership"},
	}

	lineItemStmt, err := s.db.Prepare("INSERT INTO users_invoices_line_items (`name`, `cents`, `invoice_id`, `key_name`, `is_recurring`, `created_at`, `updated_at`) VALUES (?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		logger.WithError(err).Error("could not prepare line items insert query")
		return 0, err
	}
	defer lineItemStmt.Close()

	recurringItems := []string{"did_rental", "membership"}
	for _, item := range lineItems {
		isRecurring := 0
		for _, recurringItem := range recurringItems {
			if item.keyName == recurringItem {
				isRecurring = 1
				break
			}
		}
		_, err := lineItemStmt.Exec(item.name, item.cents, invoiceID, item.keyName, isRecurring, data.Now, data.Now)
		if err != nil {
			logger.WithError(err).Error("error creating invoice line item", "key_name", item.keyName)
			return 0, err
		}
	}

	logger.Infof("Successfully created %d line items for invoice %d", len(lineItems), invoiceID)

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
	logger.Infof("Payment charged successfully for invoice %d with gateway ID %s", invoiceID, chargeResult.PaymentGatewayID)
	return s.markInvoiceChargeSuccess(invoiceID, chargeResult.PaymentGatewayID, int64(costs.TotalCosts), logger)
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

func (s *BillingService) markInvoiceChargeSuccess(invoiceID int64, gatewayID string, totalCosts int64, logger *logrus.Entry) error {
	confirmNumber, err := utils.CreateInvoiceConfirmationNumber()
	if err != nil {
		logger.WithError(err).Error("error generating confirmation number")
		return err
	}
	logger.Infof("Marking invoice %d as charged with gateway ID %s", invoiceID, gatewayID)
	finalStmt, err := s.db.Prepare("UPDATE users_invoices SET status = 'COMPLETE', source ='CARD', cents_collected = ?, confirmation_number = ?, payment_gateway_id = ? WHERE id = ?")
	defer finalStmt.Close()
	if err != nil {
		logger.WithError(err).Error("could not prepare update query")
		return err
	}

	_, err = finalStmt.Exec(totalCosts, confirmNumber, gatewayID, invoiceID)
	if err != nil {
		logger.WithError(err).Error("error updating invoice")
		return err
	}

	return nil
}

func (s *BillingService) processAnnual(task models.BillingTask, customizations *helpers.CustomizationSettingsKV, logger *logrus.Entry) error {
	billingData, err := s.loadBillingData(task, "ANNUAL", logger)
	if err != nil {
		return err
	}

	//_ := (*customizations.Pairs["allow_billing_overage"]).(helpers.CustomizationBooleanValue).Value

	costs, err := s.calculateAnnualCosts(billingData, logger)
	if err != nil {
		return err
	}

	invoiceID, err := s.createInvoice(costs, billingData, logger)
	if err != nil {
		return err
	}

	return s.chargeInvoice(invoiceID, costs, billingData, task, logger)
}
