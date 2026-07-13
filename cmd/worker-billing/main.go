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
	billingTasksQueue = "billing_tasks"
)

func main() {
	logDestination := utils.Config("LOG_DESTINATIONS")
	helpers.InitLogrus(logDestination)

	// 1. INITIALIZE DATABASE
	db, err := utils.GetDBConnection()
	if err != nil {
		log.Fatalf("Critical: Could not connect to DB: %v", err)
	}
	defer db.Close()

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

	wRepo := repository.NewWorkspaceRepository(db)
	pRepo := repository.NewPaymentRepository(db)

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

	customizations, err := helpers.GetCustomizationKVs()
	if err != nil {
		log.Fatalf("Critical: Could not load customizations: %v", err)
	}

	billingSvc := billing.NewBillingServiceWithPublisher(db, wRepo, pRepo, customizations, publisher)

	q, err := ch.QueueDeclare(billingTasksQueue, true, false, false, false, nil)
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

		// STEP 0: Handle plan cancellation requests
		if task.CancelPlan {
			log.Printf("Plan cancellation requested for subscription %d (workspace %d). Updating status to CANCELLED.", task.SubscriptionID, task.WorkspaceID)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, err := db.ExecContext(ctx, `UPDATE subscriptions SET status = 'CANCELLED' WHERE id = ?`, task.SubscriptionID)
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

		// Wrap execution loop in a defined lifecycle timeout context to prevent leaks
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)

		lockKey := fmt.Sprintf("lock:billing:subscription:%d", task.SubscriptionID)

		// STEP 1: Secure distributed lock in Redis
		locked, err := rdb.SetNX(ctx, lockKey, "processing", 5*time.Minute).Result()
		if err != nil {
			log.Printf("Redis error securing lock for subscription %d: %v. Cooling down and requeuing...", task.SubscriptionID, err)
			cancel()
			time.Sleep(2 * time.Second) // Defensive cool-down against infrastructure drops
			d.Nack(false, true)
			continue
		}
		if !locked {
			log.Printf("Lock conflict: Subscription %d is currently being processed by another worker. Requeuing...", task.SubscriptionID)
			cancel()
			time.Sleep(1 * time.Second)
			d.Nack(false, true)
			continue
		}

		// STEP 2: Verify current state using non-blocking, context-aware query
		var dbNextBillingDate sql.NullTime
		err = db.QueryRowContext(ctx, `SELECT next_billing_date FROM subscriptions WHERE id = ?`, task.SubscriptionID).Scan(&dbNextBillingDate)
		if err != nil {
			log.Printf("Database connectivity error fetching sub %d: %v. Cooling down...", task.SubscriptionID, err)
			rdb.Del(ctx, lockKey)
			cancel()
			time.Sleep(2 * time.Second) // Prevent high-speed spin loop if MySQL is restarting
			d.Nack(false, true)
			continue
		}

		targetNextDate, err := time.Parse("2006-01-02", task.NextBillingDate)
		if err != nil {
			log.Printf("Hard Failure: Malformed task date configuration: %v", err)
			rdb.Del(ctx, lockKey)
			cancel()
			d.Ack(false) // Drop invalid message permanently
			continue
		}

		// Idempotency verification guard
		if dbNextBillingDate.Valid && !dbNextBillingDate.Time.Before(targetNextDate) {
			log.Printf("Idempotency Triggered: Cycle %s for subscription %d already completed. Dropping duplicate task.", task.NextBillingDate, task.SubscriptionID)
			rdb.Del(ctx, lockKey)
			cancel()
			d.Ack(false)
			continue
		}

		// STEP 3: Handle slow third-party API payment processing (Database is completely free here)
		err = billingSvc.ProcessTask(task)
		if err != nil {
			if billing.IsTransientError(err) {
				log.Printf("Transient gateway failure for workspace %d: %v. Requeuing task...", task.WorkspaceID, err)
				rdb.Del(ctx, lockKey)
				cancel()
				time.Sleep(2 * time.Second)
				d.Nack(false, true)
			} else {
				log.Printf("Definitive Decline for workspace %d: %v. Executing state transitions to unpaid status.", task.WorkspaceID, err)
				if err := recordFailedBillingCycle(db, task, targetNextDate); err != nil {
					log.Printf("CRITICAL: Failed to write fallback failure records to storage: %v", err)
				}
				rdb.Del(ctx, lockKey)
				cancel()
				d.Ack(false)
			}
			continue
		}

		// STEP 4: Success Path -> Clean, fast context-bound atomic update
		_, err = db.ExecContext(ctx, `
			UPDATE subscriptions 
			SET next_billing_date = ?, 
				current_plan_id = ?, 
				scheduled_plan_id = NULL, 
				scheduled_effective_date = NULL 
			WHERE id = ?`, targetNextDate, task.PlanToBill, task.SubscriptionID)

		if err != nil {
			log.Printf("CRITICAL FAILURE: Payment cleared but state engine failed to commit update for sub %d: %v", task.SubscriptionID, err)
			rdb.Del(ctx, lockKey)
			cancel()
			time.Sleep(2 * time.Second)
			d.Nack(false, true)
			continue
		}

		// STEP 5: Clean up contexts and distributed locks
		rdb.Del(ctx, lockKey)
		cancel()
		log.Printf("SUCCESS: Subscription cycle successfully advanced to %s for workspace %d.", task.NextBillingDate, task.WorkspaceID)
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

	// 1. Generate an unpaid record so suspensions worker hooks into it
	_, err = tx.ExecContext(ctx, `
		INSERT INTO users_invoices (workspace_id, status, due_date, created_at, updated_at) 
		VALUES (?, 'UNPAID', ?, NOW(), NOW())`, task.WorkspaceID, time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		return fmt.Errorf("failed to write unpaid fallback invoice: %v", err)
	}

	// 2. Advance date baseline to isolate and protect card gateway from midnight loop storms
	_, err = tx.ExecContext(ctx, `
		UPDATE subscriptions 
		SET next_billing_date = ?, 
			current_plan_id = ?, 
			scheduled_plan_id = NULL, 
			scheduled_effective_date = NULL 
		WHERE id = ?`, nextDate, task.PlanToBill, task.SubscriptionID)
	if err != nil {
		return fmt.Errorf("failed to move subscription forward during fallback: %v", err)
	}

	return tx.Commit()
}