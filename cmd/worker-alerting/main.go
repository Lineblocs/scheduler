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
	log.Printf("[Alerts] Starting BalanceCheckAlertHandler for workspace %d", task.WorkspaceID)

	throttleDurStr := os.Getenv("BALANCE_CHECK_THROTTLE_DUR")
	throttleDur := 5 * time.Minute
	if throttleDurStr != "" {
		if parsedDur, err := time.ParseDuration(throttleDurStr); err == nil {
			throttleDur = parsedDur
		}
	}

	throttleKey := fmt.Sprintf("alert:throttle:balance_check:%d", task.WorkspaceID)
	throttleSet, err := h.rdb.SetNX(ctx, throttleKey, "1", throttleDur).Result()
	if err != nil {
		return fmt.Errorf("redis failure during throttle lock check: %w", err)
	}
	if !throttleSet {
		ttl, err := h.rdb.TTL(ctx, throttleKey).Result()
		if err != nil {
			return fmt.Errorf("redis failure getting ttl: %w", err)
		}
		if ttl > 0 {
			log.Printf("[Alerts] Throttled: Balance check already performed recently for workspace %d. Waiting %v for timeout.", task.WorkspaceID, ttl)
			time.Sleep(ttl)
		}
		h.rdb.Set(ctx, throttleKey, "1", throttleDur)
	}

	workspace, err := helpers.GetWorkspaceFromDB(task.WorkspaceID)
	if err != nil {
		log.Printf("[Alerts] Workspace %d not found for low balance check. Skipping. Error: %v", task.WorkspaceID, err)
		return nil
	}
	log.Printf("[Alerts] Workspace %d fetched successfully", task.WorkspaceID)

	sub, err := helpers.GetSubscription(task.WorkspaceID)
	if err != nil {
		return fmt.Errorf("failed fetching subscription: %w", err)
	}
	log.Printf("[Alerts] Subscription for Workspace %d fetched successfully", task.WorkspaceID)

	billingInfo, err := helpers.GetWorkspaceBillingInfo(workspace)
	if err != nil {
		return fmt.Errorf("failed fetching billing info: %w", err)
	}
	balance := float64(billingInfo.RemainingBalanceCents) / 100.0
	log.Printf("[Alerts] Billing info for Workspace %d fetched successfully. Balance: %f", task.WorkspaceID, balance)

	alertThreshold := float64(sub.AutoTopupThreshold)
	enableAlerts := sub.AutoTopupEnabled
	log.Printf("[Alerts] Workspace %d alert threshold: %f, enableAlerts: %t", task.WorkspaceID, alertThreshold, enableAlerts)

	// Examples of time.Now().Truncate(10 * time.Minute):
	// 14:05:30 -> 14:00:00
	// 14:19:59 -> 14:10:00
	// 14:31:01 -> 14:30:00
	truncatedTime := time.Now().Truncate(10 * time.Minute)
	alertKey := fmt.Sprintf("alert:low_balance:%d:%d", task.WorkspaceID, truncatedTime.Unix())
	log.Printf("[Alerts] Alert key: %s", alertKey)

	// Clear flag if balance recovers above threshold
	if balance > alertThreshold {
		log.Printf("[Alerts] Workspace %d balance (%f) > threshold (%f). Clearing alert key.", task.WorkspaceID, balance, alertThreshold)
		if err := h.rdb.Del(ctx, alertKey).Err(); err != nil {
			log.Printf("[Alerts] Warning: failed clearing low balance key for workspace %d: %v", task.WorkspaceID, err)
		}
		return nil
	}

	log.Printf("[Alerts] Workspace %d checking enableAlerts flag", task.WorkspaceID)
	if !enableAlerts {
		log.Printf("[Alerts] Workspace %d alerts disabled, skipping", task.WorkspaceID)
		return nil
	}

	log.Printf("[Alerts] Setting NX for alert key %s", alertKey)
	// Atomic debouncing: Ensure only 1 alert fires per low-balance state cycle
	set, err := h.rdb.SetNX(ctx, alertKey, "1", 0).Result()
	if err != nil {
		return fmt.Errorf("redis failure during threshold lock check: %w", err)
	}

	if !set {
		log.Printf("[Alerts] Debounced: Low balance notification already dispatched for workspace %d.", task.WorkspaceID)
		return nil
	}
	log.Printf("[Alerts] Lock acquired to send low balance notification for workspace %d", task.WorkspaceID)

	// 1. Send Email Alert
	alertPayload := map[string]interface{}{
		"action":       "SEND_LOW_BALANCE_NOTIFICATION",
		"workspace_id": task.WorkspaceID,
		"balance":      balance,
		"threshold":    alertThreshold,
		"timestamp":    time.Now().Unix(),
	}
	log.Printf("[Alerts] Marshaling alert payload for workspace %d", task.WorkspaceID)

	payloadBytes, err := json.Marshal(alertPayload)
	if err != nil {
		_ = h.rdb.Del(ctx, alertKey).Err()
		return fmt.Errorf("failed marshaling email alert payload: %w", err)
	}

	log.Printf("[Alerts] Publishing email alert for workspace %d", task.WorkspaceID)
	if err := h.publisher.Publish("email_alerts", payloadBytes); err != nil {
		_ = h.rdb.Del(ctx, alertKey).Err()
		return fmt.Errorf("failed publishing email alert event to RabbitMQ: %w", err)
	}
	log.Printf("[Alerts] Email alert published successfully for workspace %d", task.WorkspaceID)

	// 2. Dispatch Billing Task for Top-Up
	log.Printf("[Alerts] Constructing billing payload for top-up, workspace %d", task.WorkspaceID)
	billingPayload := map[string]interface{}{
		"run_id":            fmt.Sprintf("reload_credits_%d_%d", workspace.CreatorId, time.Now().Unix()),
		"billing_type":      "MONTHLY",
		"workspace_id":      task.WorkspaceID,
		"subscription_id":   sub.Id,
		"creator_id":        workspace.CreatorId,
		"action":            "RELOAD_CREDITS",
		"amount":            0, // Top-up amount
		"plan_to_bill":      sub.CurrentPlanId,
		"next_billing_date": nil,
	}
	billingBytes, err := json.Marshal(billingPayload)
	if err != nil {
		_ = h.rdb.Del(ctx, alertKey).Err()
		return fmt.Errorf("failed marshaling billing payload: %w", err)
	}

	log.Printf("[Alerts] Publishing billing task for workspace %d", task.WorkspaceID)
	if err := h.publisher.Publish("billing_tasks", billingBytes); err != nil {
		_ = h.rdb.Del(ctx, alertKey).Err()
		return fmt.Errorf("failed publishing billing event to RabbitMQ: %w", err)
	}
	log.Printf("[Alerts] Billing task published successfully for workspace %d", task.WorkspaceID)

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

	// 1. INITIALIZE DATABASE
	db, err := utils.GetDBConnection()
	if err != nil {
		log.Fatalf("Critical: Could not connect to DB: %v", err)
	}
	defer db.Close()

	// Verify database connection
	if err := db.Ping(); err != nil {
		log.Fatalf("Critical: Database ping failed: %v", err)
	}

	// 2. INITIALIZE REDIS
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

	// 3. INITIALIZE RABBITMQ
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

	_, err = helpers.GetCustomizationKVs()
	if err != nil {
		log.Fatalf("Critical: Could not load customizations: %v", err)
	}

	// Initialize and register all alert strategies
	alertRegistry := NewAlertRegistry()
	alertRegistry.Register("CHECK_BALANCE", NewBalanceCheckAlertHandler(db, rdb, publisher))
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

	log.Println("Server started. Worker ready. Waiting for tasks...")

	for d := range msgs {
		log.Println("Message received.")
		var task models.AlertMessageTask
		if err := json.Unmarshal(d.Body, &task); err != nil {
			log.Printf("Error unmarshaling task: %v", err)
			d.Ack(false)
			continue
		}

		taskJSON, _ := json.MarshalIndent(task, "", "  ")
		log.Printf("Received task: %s", string(taskJSON))

		if task.Action != "CHECK_BALANCE" {
			log.Printf("Ignoring unsupported task action: %s", task.Action)
			d.Ack(false)
			continue
		}

		if task.WorkspaceID == 0 {
			log.Println("Error: mandatory fields missing in task")
			d.Nack(false, false)
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

		log.Printf("Alert action not handled: %s", task.Action)
		d.Nack(false, false)
	}
}