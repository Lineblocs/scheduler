package billing

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
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
	customizations      *helpers.CustomizationSettingsKV
	rabbitmqPublisher   RabbitMQPublisher
}

var invoiceDueDateGracePeriod = 7 * 24 * time.Hour // 7 days by default

const failedChargeDescription = "failed to charge payment card on file"

type RabbitMQPublisher interface {
	Publish(queue string, message []byte) error
}

type GenericRabbitMQPublisher struct {
	channel *amqp.Channel
}


func NewGenericRabbitMQPublisher(channel *amqp.Channel) *GenericRabbitMQPublisher {
	return &GenericRabbitMQPublisher{channel: channel}
}

func (p *GenericRabbitMQPublisher) Publish(queue string, message []byte) error {
	return p.channel.Publish("", queue, false, false, amqp.Publishing{
		ContentType: "application/json",
		Body:        message,
	})
}

func NewBillingService(db *sql.DB, wRepo repository.WorkspaceRepository, pRepo repository.PaymentRepository, customizations *helpers.CustomizationSettingsKV) *BillingService {
	return &BillingService{
		db:                  db,
		workspaceRepository: wRepo,
		paymentRepository:   pRepo,
		customizations:      customizations,
	}
}

func NewBillingServiceWithPublisher(db *sql.DB, wRepo repository.WorkspaceRepository, pRepo repository.PaymentRepository, customizations *helpers.CustomizationSettingsKV, publisher RabbitMQPublisher) *BillingService {
	return &BillingService{
		db:                  db,
		workspaceRepository: wRepo,
		paymentRepository:   pRepo,
		customizations:      customizations,
		rabbitmqPublisher:   publisher,
	}
}

func (s *BillingService) isOverageEnabled() bool {
	var allowed bool = false

	if interfacePtr, ok := s.customizations.Pairs["allow_billing_overage"]; ok && interfacePtr != nil {
		if boolStruct, ok := (*interfacePtr).(*helpers.CustomizationBooleanValue); ok {
			result := boolStruct.Value
			allowed = result
		}
	}

	return allowed
}

// --- RABBITMQ PUBLISHERS ---


func (s *BillingService) publishWorkspaceUpgrade(task models.BillingTask, planName string, planID int64, action string, logger *logrus.Entry) error {
	if s.rabbitmqPublisher == nil {
		return fmt.Errorf("rabbitmq publisher not initialized")
	}

	if action != "SUCCESSFUL_UPGRADE" && action != "FAILED_UPGRADE" {
		return fmt.Errorf("invalid action %q: must be SUCCESSFUL_UPGRADE or FAILED_UPGRADE", action)
	}

	planIDInt := int(planID)
	timestamp := int(time.Now().Unix())
	upgradeTask := models.WorkspaceUpgradeResultTask{
		RunID:          task.RunID,
		WorkspaceID:    task.WorkspaceID,
		SubscriptionID: task.SubscriptionID,
		CreatorID:      task.CreatorID,
		PlanName:       planName,
		PlanID:         planIDInt,
		Action:         action,
		Timestamp:      timestamp,
	}

	messageBytes, err := json.Marshal(upgradeTask)
	if err != nil {
		logger.WithError(err).Error("error marshaling workspace upgrade task")
		return err
	}

	err = s.rabbitmqPublisher.Publish("workspace_upgrades", messageBytes)
	if err != nil {
		logger.WithError(err).Error("error publishing workspace upgrade event")
		return err
	}

	logger.Infof("Published workspace upgrade event for workspace %d, subscription %d", task.WorkspaceID, task.SubscriptionID)
	return nil
}

func (s *BillingService) publishFailedPayment(task models.BillingTask, reason string, paymentType string, cardLast4 string, cardBrand string, logger *logrus.Entry) error {
	if s.rabbitmqPublisher == nil {
		return fmt.Errorf("rabbitmq publisher not initialized")
	}

	failedTask := models.FailedBillingTask{
		RunID:          task.RunID,
		WorkspaceID:    task.WorkspaceID,
		SubscriptionID: task.SubscriptionID,
		CreatorID:      task.CreatorID,
		Reason:         reason,
		PaymentType:    paymentType,
		CardLast4:      cardLast4,
		CardBrand:      cardBrand,
	}

	messageBytes, err := json.Marshal(failedTask)
	if err != nil {
		logger.WithError(err).Error("error marshaling failed billing task")
		return err
	}

	err = s.rabbitmqPublisher.Publish("payment_failures", messageBytes)
	if err != nil {
		logger.WithError(err).Error("error publishing failed payment event")
		return err
	}

	logger.Infof("Published failed payment event for workspace %d, subscription %d", task.WorkspaceID, task.SubscriptionID)
	return nil
}

func (s *BillingService) publishPaymentReceipt(task models.BillingTask, paymentAmount int64, cardLast4 string, cardBrand string, logger *logrus.Entry) error {
	if s.rabbitmqPublisher == nil {
		return fmt.Errorf("rabbitmq publisher not initialized")
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
		return err
	}

	err = s.rabbitmqPublisher.Publish("payment_receipts", messageBytes)
	if err != nil {
		logger.WithError(err).Error("error publishing payment receipt event")
		return err
	}

	logger.Infof("Published payment receipt event for workspace %d, subscription %d, amount: %d cents", task.WorkspaceID, task.SubscriptionID, paymentAmount)
	return nil
}

func (s *BillingService) publishInvoiceGenerated(task models.BillingTask, invoiceID int64, logger *logrus.Entry) error {
	if s.rabbitmqPublisher == nil {
		return fmt.Errorf("rabbitmq publisher not initialized")
	}
	if invoiceID == 0 {
		return fmt.Errorf("invalid invoice ID 0 provided to publisher")
	}

	var invoiceTask interface{}
	queueName := "monthly_invoices"
	eventType := "monthly"

	if task.BillingType == "ANNUAL" {
		queueName = "annual_invoices"
		eventType = "annual"
		invoiceTask = models.AnnualInvoiceTask{
			AlreadyGenerated: true,
			RunID:            task.RunID,
			WorkspaceID:      task.WorkspaceID,
			CreatorID:        task.CreatorID,
			InvoiceId:        fmt.Sprintf("%d", invoiceID),
			TaxMetadata:      make(map[string]string),
		}
	} else {
		invoiceTask = models.MonthlyInvoiceTask{
			AlreadyGenerated: true,
			RunID:            task.RunID,
			WorkspaceID:      task.WorkspaceID,
			CreatorID:        task.CreatorID,
			InvoiceId:        fmt.Sprintf("%d", invoiceID),
			TaxMetadata:      make(map[string]string),
		}
	}

	messageBytes, err := json.Marshal(invoiceTask)
	if err != nil {
		logger.WithError(err).Errorf("error marshaling %s invoice task", eventType)
		return err
	}

	err = s.rabbitmqPublisher.Publish(queueName, messageBytes)
	if err != nil {
		logger.WithError(err).Errorf("error publishing %s invoice event", eventType)
		return err
	}

	logger.Infof("Published %s invoice event for workspace %d, subscription %d, invoice %d", eventType, task.WorkspaceID, task.SubscriptionID, invoiceID)
	return nil
}



// --- CORE ROUTING (ProcessTask) ---

func (s *BillingService) ProcessTask(task models.BillingTask) error {
	logger := logrus.WithField("component", "billing").
		WithField("workspace_id", task.WorkspaceID).
		WithField("run_id", task.RunID).
		WithField("action", task.Action)

	var err error

	// 1. Route based on Action (Immediate Signup/Upgrade vs. Regular Renewal)
	// Anniversary distributor uses "renewal" or "upgrade"
	switch {
	case task.Action == "IMMEDIATE":
		err = s.processImmediateProrated(task, logger)
	case task.Action == "SETTLE_INVOICE":
		err = s.processSettleInvoice(task, logger)
	case task.Action == "SETTLE_INVOICES":
		err = s.processSettleInvoices(task, logger)
	case task.BillingType == "ANNUAL" && (task.Action == "BILLING_RENEWAL" || task.Action == "BILLING_UPGRADE"):
		err = s.processAnnual(task, logger)
	case task.BillingType == "MONTHLY" && (task.Action == "BILLING_RENEWAL" || task.Action == "BILLING_UPGRADE"):
		// Handles MONTHLY and ANNIVERSARY flows
		err = s.processMonthly(task, logger)
	}

	if err != nil {
		return err
	}

	// 2. PAYMENT SUCCESSFUL -> Move anchor forward
	// We use the NextBillingDate provided by the Distributor for consistency.
	return s.updateSubscriptionAnchor(task, logger)
}

// --- PRORATION & ANCHOR UPDATES ---

func (s *BillingService) processImmediateProrated(task models.BillingTask, logger *logrus.Entry) error {
	billingData, err := s.loadBillingData(task, task.BillingType, logger)
	if err != nil {
		return err
	}

	costs := &BillingCosts{
		MembershipCosts: int64(task.Amount * 100),
		TotalCosts:      int64(task.Amount * 100),
		InvoiceDesc:     fmt.Sprintf("Initial prorated charge for %s plan", task.BillingType),
	}

	invoiceID, err := s.createInvoice(costs, billingData, logger)
	if err != nil {
		return err
	}

	_ = s.publishInvoiceGenerated(task, invoiceID, logger)

	return s.chargeInvoice(invoiceID, costs, billingData, task, logger)
}

func (s *BillingService) processSettleInvoice(task models.BillingTask, logger *logrus.Entry) error {
	logger.Info("Demo: processing invoice settlement")

	billingData, err := s.loadBillingData(task, task.BillingType, logger)
	if err != nil {
		return err
	}

	// For demo purposes, we create a generic settlement cost based on the task amount
	costs := &BillingCosts{
		MembershipCosts:   0,
		TotalCosts:        int64(task.Amount * 100),
		InvoiceDesc:       "Settlement for outstanding balance (Demo)",
	}

	// In a real scenario, you might retrieve an existing PENDING invoice by ID
	// Here we generate a new invoice to demonstrate the charge flow
	invoiceID, err := strconv.ParseInt(task.InvoiceID, 10, 64)
	if err != nil {
		logger.WithError(err).Error("invalid invoice id in task")
		return err
	}

	var totalCents int64
	var status string
	var source string
	var createdAt time.Time
	err = s.db.QueryRow("SELECT cents, status, source, created_at FROM users_invoices WHERE id = ?", invoiceID).Scan(&totalCents, &status, &source, &createdAt)
	
	if err != nil {
		logger.WithError(err).Error("could not find existing invoice for settlement")
		return err
	}

	logger.Infof("Loaded existing invoice %d: %d cents, status: %s, source: %s, created_at: %v", invoiceID, totalCents, status, source, createdAt)
	costs = &BillingCosts{
		TotalCosts:  totalCents,
		InvoiceDesc: fmt.Sprintf("Settlement for invoice %d", invoiceID),
	}

	chargeErr := s.chargeInvoice(invoiceID, costs, billingData, task, logger)
	if chargeErr != nil {
		logger.WithError(chargeErr).Error("failed to charge invoice")
		return chargeErr
	}

	// Update workspaces_suspensions to lift any suspensions for this invoice
	suspensionUpdateErr := s.db.QueryRow("UPDATE workspaces_suspensions SET status = 'LIFTED' WHERE invoice_id = ?", invoiceID).Err()
	if suspensionUpdateErr != nil {
		logger.WithError(suspensionUpdateErr).Warnf("failed to lift suspensions for invoice %d", invoiceID)
	} else {
		logger.Infof("Lifted suspensions for invoice %d", invoiceID)
	}

	return nil

}

func (s *BillingService) processSettleInvoices(task models.BillingTask, logger *logrus.Entry) error {
	logger.Infof("Processing settlement for user %d, workspace %d", task.CreatorID, task.WorkspaceID)

	billingData, err := s.loadBillingData(task, task.BillingType, logger)
	if err != nil {
		logger.WithError(err).Error("failed to load billing data for settlement")
		return err
	}

	// Calculate amount to pay (from invoice or task)
	amountToPay := int64(task.Amount * 100)
	if amountToPay <= 0 {
		logger.Warnf("Invalid amount to pay for settlement: %d cents", amountToPay)
		return fmt.Errorf("invalid amount to pay")
	}

	logger.Infof("Charging user %d for settlement: %d cents", task.CreatorID, amountToPay)
	costs := &BillingCosts{
		TotalCosts:  amountToPay,
		InvoiceDesc: fmt.Sprintf("Settlement for user %d", task.CreatorID),
	}
	chargeErr := s.chargeUser(costs, billingData, task, logger)
	if chargeErr != nil {
		logger.WithError(chargeErr).Error("failed to charge user for settlement")
		return chargeErr
	}

	logger.Infof("Successfully charged user %d for %d cents, recording payment", task.CreatorID, amountToPay)

	// Record the successful payment in users_invoices_payments table
	insertStmt, err := s.db.Prepare("INSERT INTO users_invoices_payments (`created_at`, `updated_at`, `user_id`, `workspace_id`, `cents`, `source`, `status`) VALUES (?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		logger.WithError(err).Error("failed to prepare payment insert statement")
		return err
	}
	defer insertStmt.Close()

	now := time.Now()
	_, err = insertStmt.Exec(
		now,
		now,
		task.CreatorID,
		task.WorkspaceID,
		amountToPay,
		"WEBUI",
		"PAID",
	)
	if err != nil {
		logger.WithError(err).Error("failed to insert payment record")
		return err
	}

	// Parse and update invoice statuses
	if task.InvoiceID != "" {
		invoiceIDs := strings.Split(task.InvoiceID, ",")
		for _, invoiceIDStr := range invoiceIDs {
			invoiceIDStr = strings.TrimSpace(invoiceIDStr)
			if invoiceIDStr == "" {
				continue
			}
			invoiceID, parseErr := strconv.ParseInt(invoiceIDStr, 10, 64)
			if parseErr != nil {
				logger.WithError(parseErr).Warnf("invalid invoice id in csv: %s", invoiceIDStr)
				continue
			}
			updateErr := s.markInvoiceChargePaid(invoiceID, "", 0, logger)
			if updateErr != nil {
				logger.WithError(updateErr).Warnf("failed to mark invoice %d as paid", invoiceID)
			} else {
				logger.Infof("Marked invoice %d as PAID", invoiceID)
			}

			// Update workspaces_suspensions to lift any suspensions for this invoice
			suspensionUpdateErr := s.db.QueryRow("UPDATE workspaces_suspensions SET status = 'LIFTED' WHERE invoice_id = ?", invoiceID).Err()
			if suspensionUpdateErr != nil {
				logger.WithError(suspensionUpdateErr).Warnf("failed to lift suspensions for invoice %d", invoiceID)
			} else {
				logger.Infof("Lifted suspensions for invoice %d", invoiceID)
			}
		}

	}

	logger.Infof("Payment recorded successfully for user %d, workspace %d: %d cents", task.CreatorID, task.WorkspaceID, amountToPay)
	return nil
}

func (s *BillingService) getSubscriptionData(subscriptionID int64, logger *logrus.Entry) (string, error) {
	var billingCycle string
	err := s.db.QueryRow("SELECT billing_cycle FROM subscriptions WHERE id = ?", subscriptionID).Scan(&billingCycle)
	if err != nil {
		logger.WithError(err).Error("error retrieving subscription data")
		return "", err
	}
	return billingCycle, nil
}

func (s *BillingService) HandleUpgrade(task models.WorkspaceUpgradeTask, logger *logrus.Entry) error {
	logger.Infof("Processing plan upgrade for workspace %d: current plan %d -> scheduled plan %d (subscription %d, effective: %s)", task.WorkspaceID, task.CurrentPlan, task.ScheduledPlan, task.SubscriptionID, task.ScheduledEffectiveDate)

	// Get billing type from subscription record using helper function
	billingCycle, err := s.getSubscriptionData(int64(task.SubscriptionID), logger)
	if err != nil {
		logger.WithError(err).Error("error retrieving billing cycle from subscription")
		return err
	}
	logger.Infof("Retrieved billing cycle '%s' for subscription %d", billingCycle, task.SubscriptionID)

	// Create billing task for loading billing data
	billingTask := models.BillingTask{
		WorkspaceID:    task.WorkspaceID,
		CreatorID:      task.CreatorID,
		SubscriptionID: task.SubscriptionID,
		BillingType:    billingCycle,
		PaymentMethodID: task.PaymentMethodID,
		CardLast4:      task.CardLast4,
		CardBrand:      task.CardBrand,
	}
	billingData, err := s.loadBillingData(billingTask, billingTask.BillingType, logger)
	if err != nil {
		return err
	}
	logger.Infof("Loaded billing data for workspace %d (creator: %d)", billingData.Workspace.Id, billingData.Workspace.CreatorId)

	// Create invoice with upgrade fee
	upgradeFeeInCents := task.UpgradeFee
	logger.Infof("Upgrade fee for workspace %d: %d cents", task.WorkspaceID, upgradeFeeInCents)

	deduplicationKey := helpers.GenerateDeduplicationKey("UPGRADE", time.Now().Year(), int(time.Now().Month()), time.Now().Day(), task.WorkspaceID, 0)
	logger.Infof("Checking deduplication key '%s' for workspace %d", deduplicationKey, task.WorkspaceID)
	var count int
	err = s.db.QueryRow("SELECT COUNT(*) FROM users_invoices WHERE deduplication_key = ?", deduplicationKey).Scan(&count)
	if err != nil {
		return err
	}
	if count > 0 {
		logger.Warnf("Duplicate upgrade invoice creation attempt detected for workspace %d (deduplication_key: %s)", task.WorkspaceID, deduplicationKey)
		return fmt.Errorf("duplicate upgrade invoice creation attempt")
	}

	insertStmt, err := s.db.Prepare("INSERT INTO users_invoices (`cents`, `cents_including_taxes`, `call_costs`, `recording_costs`, `fax_costs`, `membership_costs`, `number_costs`, `status`, `user_id`, `workspace_id`, `created_at`, `updated_at`, `source`, `tax_metadata`, `deduplication_key`, `due_date`, `source_service`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer insertStmt.Close()

	now := time.Now()

	// Get invoice due date from customizations
	invoiceDueDays := utils.GetInvoiceDueInDays(s.customizations)
	dueDate := now.AddDate(0, 0, invoiceDueDays)
	sourceService := "SCHEDULER"
	taxMetadata := ""

	result, err := insertStmt.Exec(upgradeFeeInCents, upgradeFeeInCents, 0, 0, 0, upgradeFeeInCents, 0, "PENDING", billingData.Workspace.CreatorId, billingData.Workspace.Id, now, now, "UPGRADE", taxMetadata, deduplicationKey, dueDate, sourceService)
	if err != nil {
		return err
	}

	invoiceID, err := result.LastInsertId()
	if err != nil {
		return err
	}

	// Create line item for upgrade proration
	lineItemStmt, err := s.db.Prepare("INSERT INTO users_invoices_line_items (`name`, `cents`, `invoice_id`, `key_name`, `is_recurring`, `created_at`, `updated_at`) VALUES (?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer lineItemStmt.Close()

	upgradeName := fmt.Sprintf("Plan upgrade: %d to %d", task.CurrentPlan, task.ScheduledPlan)
	logger.Infof("Creating line item '%s' (%d cents) for invoice %d, workspace %d", upgradeName, upgradeFeeInCents, invoiceID, billingData.Workspace.Id)
	_, err = lineItemStmt.Exec(upgradeName, upgradeFeeInCents, invoiceID, "plan_upgrade_proration", 0, now, now)
	if err != nil {
		logger.WithError(err).Error("error creating upgrade line item")
		return err
	}
	logger.Infof("Line item created successfully for invoice %d", invoiceID)

	logger.Infof("Created upgrade invoice %d with fee %d cents for workspace %d", invoiceID, upgradeFeeInCents, task.WorkspaceID)

	// Charge the upgrade fee first
	costs := &BillingCosts{
		MembershipCosts: int64(upgradeFeeInCents),
		TotalCosts:      int64(upgradeFeeInCents),
		InvoiceDesc:     fmt.Sprintf("Plan upgrade fee: %s", upgradeName),
	}

	logger.Infof("Charging invoice %d for workspace %d (%d cents)", invoiceID, task.WorkspaceID, costs.TotalCosts)
	err = s.chargeInvoice(invoiceID, costs, billingData, billingTask, logger)
	if err != nil {
		logger.WithError(err).Errorf("failed to charge invoice %d for workspace %d", invoiceID, task.WorkspaceID)

		_, rollbackErr := s.db.Exec(`
			UPDATE subscriptions
			SET scheduled_plan_id = NULL,
				scheduled_effective_date = NULL,
				updated_at = NOW()
			WHERE workspace_id = ?`, task.WorkspaceID)
		if rollbackErr != nil {
			logger.WithError(rollbackErr).Errorf("failed to rollback scheduled_plan_id and scheduled_effective_date for workspace %d", task.WorkspaceID)
		} else {
			logger.Infof("Rolled back scheduled_plan_id and scheduled_effective_date for workspace %d after charge failure", task.WorkspaceID)
		}

		_, deleteInvoiceErr := s.db.Exec(`DELETE FROM users_invoices WHERE id = ?`, invoiceID)
		if deleteInvoiceErr != nil {
			logger.WithError(deleteInvoiceErr).Errorf("failed to delete invoice %d after charge failure", invoiceID)
		} else {
			logger.Infof("Deleted invoice %d after charge failure for workspace %d", invoiceID, task.WorkspaceID)
		}

		if pubErr := s.publishWorkspaceUpgrade(billingTask, upgradeName, int64(task.ScheduledPlan), "FAILED_UPGRADE", logger); pubErr != nil {
			logger.WithError(pubErr).Errorf("failed to publish workspace upgrade failure event for workspace %d", task.WorkspaceID)
		}

		return err
	}
	logger.Infof("Successfully charged invoice %d for workspace %d", invoiceID, task.WorkspaceID)

	// Clean up upgrade metadata upon successful billing in a transaction
	if _, err := time.Parse("2006-01-02", task.ScheduledEffectiveDate); err != nil {
		logger.WithError(err).Errorf("critical: could not parse ScheduledEffectiveDate %s from task", task.ScheduledEffectiveDate)
		return err
	}

	// Begin transaction
	tx, err := s.db.Begin()
	if err != nil {
		logger.WithError(err).Error("failed to begin transaction")
		return err
	}

	logger.Infof("Updating subscription for workspace %d: setting current_plan_id=%d", task.WorkspaceID, task.ScheduledPlan)
	_, err = tx.Exec(`
        UPDATE subscriptions 
        SET current_plan_id = ?,
            last_billed_at = NOW(),
            updated_at = NOW() 
        WHERE workspace_id = ?`, task.ScheduledPlan, task.WorkspaceID)

	if err != nil {
		tx.Rollback()
		logger.WithError(err).Error("failed to update subscription plan")
		return err
	}

	// Commit transaction
	logger.Infof("Committing upgrade transaction for workspace %d", task.WorkspaceID)
	err = tx.Commit()
	if err != nil {
		logger.WithError(err).Error("failed to commit transaction")
		return err
	}

	logger.Infof("Subscription plan updated to %d for workspace %d (effective: %s)", task.ScheduledPlan, task.WorkspaceID, task.ScheduledEffectiveDate)

	if pubErr := s.publishWorkspaceUpgrade(billingTask, upgradeName, int64(task.ScheduledPlan), "SUCCESSFUL_UPGRADE", logger); pubErr != nil {
		logger.WithError(pubErr).Errorf("failed to publish workspace upgrade success event for workspace %d", task.WorkspaceID)
	}

	return nil
}

func (s *BillingService) updateSubscriptionAnchor(task models.BillingTask, logger *logrus.Entry) error {
	// IMPORTANT: We do not calculate the date here. 
	// We parse the string sent by the distributor to ensure the source of truth is singular.
	nextDate, err := time.Parse("2006-01-02", task.NextBillingDate)
	if err != nil {
		logger.WithError(err).Errorf("critical: could not parse NextBillingDate %s from distributor", task.NextBillingDate)
		return err
	}

	// Clean up upgrade metadata upon successful billing
	_, err = s.db.Exec(`
        UPDATE subscriptions 
        SET next_billing_date = ?, 
            current_plan_id = ?,
            scheduled_plan_id = NULL,
            scheduled_effective_date = NULL,
            last_billed_at = NOW(),
            updated_at = NOW() 
        WHERE id = ?`, nextDate, task.PlanToBill, task.SubscriptionID)

	if err != nil {
		logger.WithError(err).Error("failed to update next_billing_date anchor")
		return err
	}

	logger.Infof("Subscription %d cycle advanced to %s (Plan: %d)", task.SubscriptionID, task.NextBillingDate, task.PlanToBill)
	return nil
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

	_ = s.publishInvoiceGenerated(task, invoiceID, logger)

	return s.chargeInvoice(invoiceID, costs, billingData, task, logger)
}

func (s *BillingService) processAnnual(task models.BillingTask, logger *logrus.Entry) error {
	billingData, err := s.loadBillingData(task, "ANNUAL", logger)
	if err != nil {
		return err
	}

	costs, err := s.calculateAnnualCosts(billingData, logger)
	if err != nil {
		return err
	}

	invoiceID, err := s.createInvoice(costs, billingData, logger)
	if err != nil {
		return err
	}

	_ = s.publishInvoiceGenerated(task, invoiceID, logger)

	return s.chargeInvoice(invoiceID, costs, billingData, task, logger)
}

// --- CHARGE LOGIC ---

func (s *BillingService) chargeUser(costs *BillingCosts, data *BillingData, task models.BillingTask, logger *logrus.Entry) error {
	invoiceID := int64(0) // No invoice record for this flow, so we pass 0 and handle accordingly in chargeUser
	return s.chargeWithCard(invoiceID, costs, data, task, logger)
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
	s.markInvoiceChargeFailed(invoiceID, logger)
	return fmt.Errorf("insufficient credits")
}

func (s *BillingService) chargeCreditsOnly(invoiceID int64, totalCosts int64, data *BillingData, task models.BillingTask, logger *logrus.Entry) error {
	logger.Info("User has enough credits. Charging balance")

	confNumber, err := utils.CreateInvoiceConfirmationNumber()
	if err != nil {
		logger.WithError(err).Error("error generating confirmation number")
		return err
	}

	updateStmt, err := s.db.Prepare("UPDATE users_invoices SET status = 'PAID', source ='CREDITS', cents_collected = ?, confirmation_number = ? WHERE id = ?")
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

	return s.publishPaymentReceipt(task, totalCosts, "", "CREDITS", logger)
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
	if task.PaymentMethodID != "" {
		invoice.PaymentMethodId = &task.PaymentMethodID
		invoice.CardLast4 = task.CardLast4
		invoice.CardBrand = task.CardBrand
	}

	chargeResult, err := s.paymentRepository.ChargeCustomer(data.BillingParams.(*utils.BillingParams), data.User, data.Workspace, &invoice)
	if err != nil {
		logger.WithError(err).Error(failedChargeDescription)

		if err := s.publishFailedPayment(task, failedChargeDescription, "CARD", task.CardLast4, task.CardBrand, logger); err != nil {
			logger.WithError(err).Error("could not publish failed payment")
		}
		if err := s.markInvoiceChargeFailed(invoiceID, logger); err != nil {
		logger.WithError(err).Error("could not mark invoice charge as failed")
			return err
		}
		return err
	}

	err = s.publishPaymentReceipt(task, int64(costs.TotalCosts), chargeResult.CardLast4, chargeResult.CardBrand, logger)
	if err != nil {
		return err
	}

	logger.Infof("Payment charged successfully for invoice %d with gateway ID %s", invoiceID, chargeResult.PaymentGatewayID)
	return s.markInvoiceChargePaid(invoiceID, chargeResult.PaymentGatewayID, int64(costs.TotalCosts), logger)
}

// --- DATA LOADING & CALCULATIONS ---

func (s *BillingService) loadBillingData(task models.BillingTask, billingType string, logger *logrus.Entry) (*BillingData, error) {
	conn := utils.NewDBConn(s.db)

	subscription, err := s.paymentRepository.GetSubscription(task.SubscriptionID)
	if err != nil {
		logger.WithError(err).Error("error getting subscription")
		return nil, err
	}

	billingParams, err := conn.GetBillingParams()
	if err != nil {
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
		return nil, err
	}

	user, err := s.workspaceRepository.GetUserFromDB(task.CreatorID)
	if err != nil {
		return nil, err
	}

	plans, err := s.paymentRepository.GetServicePlans()
	if err != nil {
		return nil, err
	}

	plan := utils.GetPlanBySubscription(plans, subscription)
	if plan == nil {
		return nil, fmt.Errorf("plan not found for subscription")
	}

	billingInfo, err := s.workspaceRepository.GetWorkspaceBillingInfo(workspace)
	if err != nil {
		return nil, err
	}

	baseCosts, err := helpers.GetBaseCosts()
	if err != nil {
		return nil, err
	}

	return &BillingData{
		BillingParams:      billingParams,
		Workspace:          workspace,
		User:               user,
		Plan:               plan,
		BillingInfo:        billingInfo,
		BaseCosts:          baseCosts,
		BillingPeriodStart: billingPeriodStart,
		BillingPeriodEnd:   now,
		Now:                now,
	}, nil
}

func (s *BillingService) calculateMonthlyCosts(data *BillingData, logger *logrus.Entry) (*BillingCosts, error) {
	costs := &BillingCosts{}
	userCount := utils.GetWorkspaceUserCountInPeriod(s.db, data.Workspace.Id, data.BillingPeriodStart, data.BillingPeriodEnd)
	costs.MembershipCosts = int64(data.Plan.MonthlyCostCents * userCount)

	err := utils.CreateMonthlyNumberRentalDebit(s.db, data.Workspace.Id, data.User.Id, data.BillingPeriodStart)
	if err != nil {
		return nil, err
	}

	startStr := data.BillingPeriodStart.Format(time.DateTime)
	endStr := data.BillingPeriodEnd.Format(time.DateTime)

	if err := s.processDebits(data, costs, startStr, endStr, logger); err != nil {
		return nil, err
	}
	if err := s.processRecordings(data, costs, startStr, endStr, logger); err != nil {
		return nil, err
	}
	if err := s.processFaxes(data, costs, startStr, endStr, logger); err != nil {
		return nil, err
	}

	costs.TotalCosts = costs.MembershipCosts + costs.CallTollsCosts + costs.RecordingCosts + costs.FaxCosts + costs.NumberRentalCosts
	costs.InvoiceDesc = fmt.Sprintf("LineBlocs invoice for %s", data.BillingInfo.InvoiceDue)

	return costs, nil
}

func (s *BillingService) calculateAnnualCosts(data *BillingData, logger *logrus.Entry) (*BillingCosts, error) {
	costs := &BillingCosts{}
	userCount := utils.GetWorkspaceUserCount(s.db, data.Workspace.Id)
	costs.MembershipCosts = int64(data.Plan.AnnualCostCents * userCount)

	costs.TotalCosts = costs.MembershipCosts
	costs.InvoiceDesc = fmt.Sprintf("LineBlocs annual invoice for %s", data.BillingInfo.InvoiceDue)

	return costs, nil
}

func (s *BillingService) processDebits(data *BillingData, costs *BillingCosts, startStr, endStr string, logger *logrus.Entry) error {
	rows, err := s.db.Query("SELECT id, source, module_id, cents, created_at FROM users_debits WHERE workspace_id = ? AND created_at BETWEEN DATE(?) AND DATE(?)", data.Workspace.Id, startStr, endStr)
	if err != nil {
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
		return
	}

	callDurationMinutes := float64(call.DurationNumber / 60)
	charge, err := utils.ComputeAmountToCharge(float64(costCents), *remainingMinutes, callDurationMinutes)
	if err == nil {
		costs.CallTollsCosts += int64(charge)
		*remainingMinutes -= callDurationMinutes
	}
}

func (s *BillingService) processNumberRentalDebit(data *BillingData, costs *BillingCosts, moduleID int, logger *logrus.Entry) {
	did, err := s.workspaceRepository.GetDIDFromDB(moduleID)
	if err == nil {
		costs.NumberRentalCosts += int64(did.MonthlyCost)
	}
}

func (s *BillingService) processRecordings(data *BillingData, costs *BillingCosts, startStr, endStr string, logger *logrus.Entry) error {
	rows, err := s.db.Query("SELECT id, size, created_at FROM recordings WHERE user_id = ? AND created_at BETWEEN ? AND ?", data.Workspace.CreatorId, startStr, endStr)
	if err != sql.ErrNoRows && err != nil {
		return err
	}
	if err == sql.ErrNoRows {
		return nil
	}
	defer rows.Close()

	remainingRecordings := data.Plan.RecordingSpace
	for rows.Next() {
		var recordingID int
		var recordingSizeBytes float64
		var recordingCreatedAt time.Time
		if err := rows.Scan(&recordingID, &recordingSizeBytes, &recordingCreatedAt); err != nil {
			continue
		}

		recordingCentsPerByte := int64(math.Round(data.BaseCosts.RecordingsPerByte * recordingSizeBytes))
		charge, err := utils.ComputeAmountToCharge(float64(recordingCentsPerByte), remainingRecordings, recordingSizeBytes)
		if err == nil {
			costs.RecordingCosts += int64(charge)
			remainingRecordings -= recordingSizeBytes
		}
	}
	return nil
}

func (s *BillingService) processFaxes(data *BillingData, costs *BillingCosts, startStr, endStr string, logger *logrus.Entry) error {
	rows, err := s.db.Query("SELECT id, created_at FROM faxes WHERE workspace_id = ? AND created_at BETWEEN ? AND ?", data.Workspace.Id, startStr, endStr)
	if err != sql.ErrNoRows && err != nil {
		return err
	}
	if err == sql.ErrNoRows {
		return nil
	}
	defer rows.Close()

	remainingFaxUnits := data.Plan.Fax
	for rows.Next() {
		var faxID int
		var faxCreatedAt time.Time
		if err := rows.Scan(&faxID, &faxCreatedAt); err != nil {
			continue
		}

		planFaxLimit := float64(data.Plan.Fax)
		faxCentsPerUnit := data.BaseCosts.FaxPerUsed
		charge, err := utils.ComputeAmountToCharge(faxCentsPerUnit, float64(remainingFaxUnits), planFaxLimit)
		if err == nil {
			costs.FaxCosts += int64(charge)
			remainingFaxUnits--
		}
	}
	return nil
}

func (s *BillingService) createInvoice(costs *BillingCosts, data *BillingData, logger *logrus.Entry) (int64, error) {
	deduplicationKey := helpers.GenerateDeduplicationKey("INVOICE", data.BillingPeriodStart.Year(), int(data.BillingPeriodStart.Month()), data.BillingPeriodStart.Day(), data.Workspace.Id, 0)
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM users_invoices WHERE deduplication_key = ?", deduplicationKey).Scan(&count)
	if err != nil {
		return 0, err
	}
	if count > 0 {
		return 0, fmt.Errorf("duplicate invoice creation attempt")
	}

	insertStmt, err := s.db.Prepare("INSERT INTO users_invoices (`cents`, `cents_including_taxes`, `call_costs`, `recording_costs`, `fax_costs`, `membership_costs`, `number_costs`, `status`, `user_id`, `workspace_id`, `created_at`, `updated_at`, `source`, `tax_metadata`, `deduplication_key`, `due_date`, `source_service`) VALUES ( ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return 0, err
	}
	defer insertStmt.Close()

	taxMetadata := utils.CreateTaxMetadata(costs.CallTollsCosts, costs.RecordingCosts, costs.FaxCosts, costs.MembershipCosts, costs.NumberRentalCosts)
	dueDate := data.Now.Add(invoiceDueDateGracePeriod)
	sourceService := "SCHEDULER"
	result, err := insertStmt.Exec(costs.TotalCosts, costs.TotalCosts, costs.CallTollsCosts, costs.RecordingCosts, costs.FaxCosts, costs.MembershipCosts, costs.NumberRentalCosts, "PENDING", data.Workspace.CreatorId, data.Workspace.Id, data.Now, data.Now, "SUBSCRIPTION", taxMetadata, deduplicationKey, dueDate, sourceService)
	if err != nil {
		return 0, err
	}

	invoiceID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}

	lineItemStmt, err := s.db.Prepare("INSERT INTO users_invoices_line_items (`name`, `cents`, `invoice_id`, `key_name`, `is_recurring`, `created_at`, `updated_at`) VALUES (?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return invoiceID, err
	}
	defer lineItemStmt.Close()

	lineItems := []struct {
		name    string
		cents   int64
		keyName string
	}{
		{"Call Tolls", costs.CallTollsCosts, "call_tolls"},
		{"Recording Storage", costs.RecordingCosts, "recording_storage"},
		{"Fax Services", costs.FaxCosts, "fax_services"},
		{"DID Rental", costs.NumberRentalCosts, "did_rental"},
		{"Membership", costs.MembershipCosts, "membership"},
	}

	for _, item := range lineItems {
		isRecurring := 0
		if item.keyName == "did_rental" || item.keyName == "membership" {
			isRecurring = 1
		}
		_, err := lineItemStmt.Exec(item.name, float64(item.cents), invoiceID, item.keyName, isRecurring, data.Now, data.Now)
		if err != nil {
			logger.WithError(err).Error("error creating invoice line item")
		}
	}

	return invoiceID, nil
}

// --- STATUS UPDATES ---

func (s *BillingService) markInvoiceChargeFailed(invoiceID int64, logger *logrus.Entry) error {
	_, err := s.db.Exec("UPDATE users_invoices SET status = 'FAILED' WHERE id = ?", invoiceID)
	return err
}

func (s *BillingService) markInvoiceChargePaid(invoiceID int64, gatewayID string, totalCosts int64, logger *logrus.Entry) error {
	confirmNumber, err := utils.CreateInvoiceConfirmationNumber()
	if err != nil {
		return err
	}
	_, err = s.db.Exec("UPDATE users_invoices SET status = 'PAID', source ='CARD', cents_collected = ?, confirmation_number = ?, payment_gateway_id = ?, paid_date = ? WHERE id = ?", totalCosts, confirmNumber, gatewayID, data.Now, invoiceID)
	return err
}