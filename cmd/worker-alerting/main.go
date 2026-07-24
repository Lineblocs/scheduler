package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	helpers "github.com/Lineblocs/go-helpers"
	"lineblocs.com/scheduler/internal/billing"
	"lineblocs.com/scheduler/models"
	"lineblocs.com/scheduler/repository"
	"lineblocs.com/scheduler/utils"

	_ "github.com/go-sql-driver/mysql"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
)

const (
	alertingTasksQueue = "alerting_tasks"
)

// AlertHandler defines the contract for any alert evaluation strategy.
type AlertHandler interface {
	Handle(ctx context.Context, task AlertTask) error
}

// BalanceCheckAlertTask holds data for low balance alerting
type BalanceCheckAlertTask struct {
	WorkspaceID int
}

// BalanceCheckStruct represents another alert struct for balance checks
type BalanceCheckStruct struct {
	WorkspaceID int
	Source      string
	CreatedAt   time.Time
}

// UsageLimitAlertTask holds data for usage limit alerting
type UsageLimitAlertTask struct {
	WorkspaceID int
	LimitType   string
	Usage       float64
}

// AlertTask encapsulates the different types of alerts
type AlertTask struct {
	Action     string
	WorkspaceID int
	BalanceCheck *BalanceCheckAlertTask
	UsageLimit *UsageLimitAlertTask
}

// AlertRegistry maps task actions to their respective handlers.
type AlertRegistry struct {
	handlers map[string]AlertHandler
}

func NewAlertRegistry() *AlertRegistry {
	return &AlertRegistry{
		handlers: make(map[string]AlertHandler),
	}
}

func (r *AlertRegistry) Register(action string, handler AlertHandler) {
	r.handlers[action] = handler
}

func (r *AlertRegistry) Dispatch(ctx context.Context, task AlertTask) (bool, error) {
	handler, exists := r.handlers[task.Action]
	if !exists {
		return false, nil // Not an alert task handled by registry
	}
	err := handler.Handle(ctx, task)
	return true, err
}

// --- STRATEGY 1: Low Balance Alert Handler ---

type BalanceCheckAlertHandler struct {
	db        *sql.DB
	rdb       *redis.Client
	publisher *billing.GenericRabbitMQPublisher
}

func NewBalanceCheckAlertHandler(db *sql.DB, rdb *redis.Client, pub *billing.GenericRabbitMQPublisher) *BalanceCheckAlertHandler {
	return &BalanceCheckAlertHandler{db: db, rdb: rdb, publisher: pub}
}

func (h *BalanceCheckAlertHandler) Handle(ctx context.Context, task AlertTask) error {

	workspace, err := helpers.GetWorkspaceFromDB(task.WorkspaceID)
	if err != nil {
		log.Printf("[Alerts] Workspace %d not found for low balance check. Skipping.", task.WorkspaceID)
		return nil
	}

	sub, err := helpers.GetSubscription(task.WorkspaceID)
	if err != nil {
		return fmt.Errorf("failed fetching subscription: %w", err)
	}

	billingInfo, err := helpers.GetWorkspaceBillingInfo(workspace)
	if err != nil {
		return fmt.Errorf("failed fetching billing info: %w", err)
	}
	balance := float64(billingInfo.RemainingBalanceCents) / 100.0

	alertThreshold := float64(sub.AutoTopupThreshold)
	enableAlerts := sub.AutoTopupEnabled

	alertKey := fmt.Sprintf("alert:low_balance:%d", task.WorkspaceID)

	// Clear flag if balance recovers above threshold
	if balance > alertThreshold {
		if err := h.rdb.Del(ctx, alertKey).Err(); err != nil {
			log.Printf("[Alerts] Warning: failed clearing low balance key for workspace %d: %v", task.WorkspaceID, err)
		}
		return nil
	}

	if !enableAlerts {
		return nil
	}

	// Atomic debouncing: Ensure only 1 alert fires per low-balance state cycle
	set, err := h.rdb.SetNX(ctx, alertKey, "1", 0).Result()
	if err != nil {
		return fmt.Errorf("redis failure during threshold lock check: %w", err)
	}

	if !set {
		log.Printf("[Alerts] Debounced: Low balance notification already dispatched for workspace %d.", task.WorkspaceID)
		return nil
	}

	// 1. Send Email Alert
	alertPayload := map[string]interface{}{
		"action":       "SEND_LOW_BALANCE_NOTIFICATION",
		"workspace_id": task.WorkspaceID,
		"balance":      balance,
		"threshold":    alertThreshold,
		"timestamp":    time.Now().Unix(),
	}

	payloadBytes, err := json.Marshal(alertPayload)
	if err != nil {
		_ = h.rdb.Del(ctx, alertKey).Err()
		return fmt.Errorf("failed marshaling email alert payload: %w", err)
	}

	if err := h.publisher.Publish("email_alerts", payloadBytes); err != nil {
		_ = h.rdb.Del(ctx, alertKey).Err()
		return fmt.Errorf("failed publishing email alert event to RabbitMQ: %w", err)
	}

	// 2. Dispatch Billing Task for Top-Up
	billingPayload := map[string]interface{}{
		"action":       "RELOAD_CREDITS",
		"workspace_id": task.WorkspaceID,
	}
	billingBytes, err := json.Marshal(billingPayload)
	if err != nil {
		_ = h.rdb.Del(ctx, alertKey).Err()
		return fmt.Errorf("failed marshaling billing payload: %w", err)
	}

	if err := h.publisher.Publish("billing_tasks", billingBytes); err != nil {
		_ = h.rdb.Del(ctx, alertKey).Err()
		return fmt.Errorf("failed publishing billing event to RabbitMQ: %w", err)
	}

	log.Printf("[Alerts] SUCCESS: Low balance notification triggered for workspace %d (Current: %.2f, Threshold: %.2f).",
		task.WorkspaceID, balance, alertThreshold)

	return nil
}

// --- STRATEGY 2: Example Template for Future Alerts ---
/*
type UsageLimitAlertHandler struct {
	db        *sql.DB
	rdb       *redis.Client
	publisher *billing.GenericRabbitMQPublisher
}

func (h *UsageLimitAlertHandler) Handle(ctx context.Context, task models.BillingTask) error {
	// Implement usage limit checks here
	return nil
}
*/

// --- MAIN WORKER ---

func main() {
	logDestination := utils.Config("LOG_DESTINATIONS")
	helpers.InitLogrus(logDestination)

	db, err := utils.GetDBConnection()
	if err != nil {
		log.Fatalf("Critical: Could not connect to DB: %v", err)
	}
	defer db.Close()

	redisURL := os.Getenv("REDIS_URL")
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Fatalf("Critical: Failed to parse REDIS_URL: %v", err)
	}
	rdb := redis.NewClient(opt)
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("Critical: Consumer could not connect to Redis: %v", err)
	}
	defer rdb.Close()

	wRepo := repository.NewWorkspaceRepository(db)
	pRepo := repository.NewPaymentRepository(db)

	conn, err := amqp.Dial(os.Getenv("QUEUE_URL"))
	if err != nil {
		log.Fatalf("Critical: Could not connect to RabbitMQ: %v", err)
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		log.Fatalf("Critical: Could not open channel: %v", err)
	}
	defer ch.Close()

	publisher := billing.NewGenericRabbitMQPublisher(ch)

	customizations, err := helpers.GetCustomizationKVs()
	if err != nil {
		log.Fatalf("Critical: Could not load customizations: %v", err)
	}

	billingSvc := billing.NewBillingServiceWithPublisher(db, wRepo, pRepo, customizations, publisher)

	// Initialize and register all alert strategies
	alertRegistry := NewAlertRegistry()
	alertRegistry.Register("CHECK_LOW_BALANCE", NewBalanceCheckAlertHandler(db, rdb, publisher))
	// alertRegistry.Register("CHECK_USAGE_LIMIT", NewUsageLimitAlertHandler(db, rdb, publisher))

	q, err := ch.QueueDeclare(alertingTasksQueue, true, false, false, false, nil)
	if err != nil {
		log.Fatalf("Critical: Could not declare queue: %v", err)
	}

	ch.Qos(1, 0, false)
	msgs, err := ch.Consume(q.Name, "", false, false, false, false, nil)
	if err != nil {
		log.Fatalf("Critical: Could not start consumer: %v", err)
	}

	log.Println("Worker ready. Waiting for tasks...")

	for d := range msgs {
		var task models.BillingTask
		if err := json.Unmarshal(d.Body, &task); err != nil {
			log.Printf("Error unmarshaling task: %v", err)
			d.Ack(false)
			continue
		}

		taskJSON, _ := json.MarshalIndent(task, "", "  ")
		log.Printf("Received task: %s", string(taskJSON))

		// STEP 0-A: Handle plan cancellation requests
		if task.CancelPlan {
			log.Printf("Plan cancellation requested for subscription %d (workspace %d).", task.SubscriptionID, task.WorkspaceID)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, err := db.ExecContext(ctx, `UPDATE subscriptions SET status = 'CANCELLED', cancel_at_period_end = 0 WHERE id = ?`, task.SubscriptionID)
			cancel()
			if err != nil {
				log.Printf("Error cancelling subscription %d: %v", task.SubscriptionID, err)
				d.Nack(false, true)
				continue
			}
			log.Printf("SUCCESS: Subscription %d cancelled for workspace %d.", task.SubscriptionID, task.WorkspaceID)
			d.Ack(false)
			continue
		}

		// STEP 0-B: Extensible Alert Handler Dispatch
		ctxAlert, cancelAlert := context.WithTimeout(context.Background(), 15*time.Second)
		handled, alertErr := alertRegistry.Dispatch(ctxAlert, AlertTask{
			Action: task.Action,
			WorkspaceID: task.WorkspaceID,
		})
		cancelAlert()

		if handled {
			if alertErr != nil {
				log.Printf("Error processing alert [%s] for workspace %d: %v", task.Action, task.WorkspaceID, alertErr)
				d.Nack(false, true)
				continue
			}
			d.Ack(false)
			continue
		}

		// --- Standard Billing Flow ---
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)

		lockKey := fmt.Sprintf("lock:billing:subscription:%d", task.SubscriptionID)

		// STEP 1: Secure distributed lock in Redis
		locked, err := rdb.SetNX(ctx, lockKey, "processing", 5*time.Minute).Result()
		if err != nil {
			log.Printf("Redis error securing lock for subscription %d: %v. Requeuing...", task.SubscriptionID, err)
			cancel()
			time.Sleep(2 * time.Second)
			d.Nack(false, true)
			continue
		}
		if !locked {
			log.Printf("Lock conflict: Subscription %d is being processed. Requeuing...", task.SubscriptionID)
			cancel()
			time.Sleep(1 * time.Second)
			d.Nack(false, true)
			continue
		}

		// STEP 2: Verify current state
		var dbNextBillingDate sql.NullTime
		err = db.QueryRowContext(ctx, `SELECT next_billing_date FROM subscriptions WHERE id = ?`, task.SubscriptionID).Scan(&dbNextBillingDate)
		if err != nil {
			log.Printf("Database error fetching sub %d: %v. Cooling down...", task.SubscriptionID, err)
			rdb.Del(ctx, lockKey)
			cancel()
			time.Sleep(2 * time.Second)
			d.Nack(false, true)
			continue
		}

		var targetNextDate time.Time
		if task.Action != "ADD_CREDITS" {
			var err error
			targetNextDate, err = time.Parse("2006-01-02", task.NextBillingDate)
			if err != nil {
				log.Printf("Hard Failure: Malformed task date configuration: %v", err)
				rdb.Del(ctx, lockKey)
				cancel()
				d.Ack(false)
				continue
			}

			if dbNextBillingDate.Valid && !dbNextBillingDate.Time.Before(targetNextDate) {
				log.Printf("Idempotency Triggered: Cycle %s for sub %d already completed.", task.NextBillingDate, task.SubscriptionID)
				rdb.Del(ctx, lockKey)
				cancel()
				d.Ack(false)
				continue
			}
		}

		// STEP 3: Process Task
		err = billingSvc.ProcessTask(task)
		if err != nil {
			if billing.IsTransientError(err) {
				log.Printf("Transient gateway failure for workspace %d: %v. Requeuing...", task.WorkspaceID, err)
				rdb.Del(ctx, lockKey)
				cancel()
				time.Sleep(2 * time.Second)
				d.Nack(false, true)
			} else {
				log.Printf("Definitive Decline for workspace %d: %v. Marking unpaid.", task.WorkspaceID, err)
				if err := recordFailedBillingCycle(db, task, targetNextDate); err != nil {
					log.Printf("CRITICAL: Failed writing fallback record: %v", err)
				}
				rdb.Del(ctx, lockKey)
				cancel()
				d.Ack(false)
			}
			continue
		}

		// STEP 4: Success Path Update
		excludedActions := []string{"ADD_CREDITS"}
		shouldSkipUpdate := false
		for _, action := range excludedActions {
			if task.Action == action {
				shouldSkipUpdate = true
				break
			}
		}

		if !shouldSkipUpdate {
			_, err = db.ExecContext(ctx, `
                UPDATE subscriptions 
                SET next_billing_date = ?, 
                    current_plan_id = ?, 
                    scheduled_plan_id = NULL, 
                    scheduled_effective_date = NULL 
                WHERE id = ?`, targetNextDate, task.PlanToBill, task.SubscriptionID)

			if err != nil {
				log.Printf("CRITICAL FAILURE: State engine failed to commit update for sub %d: %v", task.SubscriptionID, err)
				rdb.Del(ctx, lockKey)
				cancel()
				time.Sleep(2 * time.Second)
				d.Nack(false, true)
				continue
			}
		}

		// Clear low balance lock on credit addition
		if task.Action == "ADD_CREDITS" {
			alertKey := fmt.Sprintf("alert:low_balance:%d", task.WorkspaceID)
			_ = rdb.Del(ctx, alertKey).Err()
			log.Printf("[Alerts] Cleared low balance state key for workspace %d after credit top-up.", task.WorkspaceID)
		}

		// STEP 5: Cleanup
		rdb.Del(ctx, lockKey)
		cancel()
		log.Printf("SUCCESS: Subscription cycle advanced to %s for workspace %d.", task.NextBillingDate, task.WorkspaceID)
		d.Ack(false)
	}
}

func recordFailedBillingCycle(db *sql.DB, task models.BillingTask, nextDate time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
        INSERT INTO users_invoices (workspace_id, status, due_date, created_at, updated_at) 
        VALUES (?, 'UNPAID', ?, NOW(), NOW())`, task.WorkspaceID, time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		return fmt.Errorf("failed writing unpaid invoice: %v", err)
	}

	_, err = tx.ExecContext(ctx, `
        UPDATE subscriptions 
        SET next_billing_date = ?, 
            current_plan_id = ?, 
            scheduled_plan_id = NULL, 
            scheduled_effective_date = NULL 
        WHERE id = ?`, nextDate, task.PlanToBill, task.SubscriptionID)
	if err != nil {
		return fmt.Errorf("failed advancing subscription date: %v", err)
	}

	return tx.Commit()
}