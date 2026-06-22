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
)

const (
	billingTasksQueue = "billing_tasks"
)

func main() {
	logDestination := utils.Config("LOG_DESTINATIONS")
	helpers.InitLogrus(logDestination)

	db, err := utils.GetDBConnection()
	if err != nil {
		log.Fatalf("Critical: Could not connect to DB: %v", err)
	}
	defer db.Close()

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

		// UPDATED: 1. Idempotency Check & SELECT FOR UPDATE Guard via Tx
		tx, err := db.BeginTx(context.Background(), nil)
		if err != nil {
			log.Printf("Transient DB Error starting tx for Workspace %d: %v", task.WorkspaceID, err)
			d.Nack(false, true) // Requeue task
			continue
		}

		var dbNextBillingDate sql.NullTime
		err = tx.QueryRow(`SELECT next_billing_date FROM subscriptions WHERE id = ? FOR UPDATE`, task.SubscriptionID).Scan(&dbNextBillingDate)
		if err != nil {
			tx.Rollback()
			log.Printf("Transient DB lock error on subscription %d: %v", task.SubscriptionID, err)
			d.Nack(false, true) // Requeue task safely
			continue
		}

		targetNextDate, err := time.Parse("2006-01-02", task.NextBillingDate)
		if err != nil {
			tx.Rollback()
			log.Printf("Hard Error: Invalid date format in task: %v", err)
			d.Ack(false) // Drop malformed task
			continue
		}

		// If next_billing_date is already advanced or caught up, this task is an accidental duplicate. Skip processing!
		if dbNextBillingDate.Valid && !dbNextBillingDate.Time.Before(targetNextDate) {
			tx.Rollback()
			log.Printf("Idempotency Triggered: Workspace %d already advanced for this cycle. Skipping payment gateway.", task.WorkspaceID)
			d.Ack(false)
			continue
		}

		// UPDATED: 2. ATTEMPT PAYMENT PROCESSING
		err = billingSvc.ProcessTask(task)
		if err != nil {
			tx.Rollback()

			// Check error types from your internal billing service wrapper
			if billing.IsTransientError(err) {
				log.Printf("Transient payment network/API failure for Workspace %d: %v. Requeuing...", task.WorkspaceID, err)
				d.Nack(false, true) // Requeue to try again later
			} else {
				log.Printf("PERMANENT CARD DECLINE for Workspace %d: %v. Breaking loop, advancing cycle to unpaid status.", task.WorkspaceID, err)

				// UPDATED: Handle hard failures to prevent midnight loops.
				// Advance cycle anyway so they are not hit tomorrow, but save an unpaid invoice record.
				if err := recordFailedBillingCycle(db, task, targetNextDate); err != nil {
					log.Printf("CRITICAL: Failed to transition subscription state to failed/unpaid for Workspace %d: %v", task.WorkspaceID, err)
				}
				d.Ack(false)
			}
			continue
		}

		// UPDATED: 3. SUCCESSFUL PATH -> UPDATE THE SUBSCRIPTION RECORD AND COMMIT TX
		_, err = tx.Exec(`
            UPDATE subscriptions 
            SET next_billing_date = ?, 
                current_plan_id = ?, 
                scheduled_plan_id = NULL, 
                scheduled_effective_date = NULL 
            WHERE id = ?`, targetNextDate, task.PlanToBill, task.SubscriptionID)

		if err != nil {
			tx.Rollback() // If DB fails to commit, roll back state. We choose to retry (possible double charge but avoids orphan states)
			log.Printf("CRITICAL: Payment succeeded but DB subscription update failed for Workspace %d: %v", task.WorkspaceID, err)
			d.Nack(false, true)
			continue
		}

		if err := tx.Commit(); err != nil {
			log.Printf("CRITICAL: Failed to commit transaction for Workspace %d: %v", task.WorkspaceID, err)
			d.Nack(false, true)
			continue
		}

		log.Printf("SUCCESS: Workspace %d billed successfully. Cycle advanced to %s.", task.WorkspaceID, task.NextBillingDate)
		d.Ack(false)
	}
}

// UPDATED: Breaking infinite decline loops by recording unpaid states and safely advancing next date
func recordFailedBillingCycle(db *sql.DB, task models.BillingTask, nextDate time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. Create an outstanding, unpaid entry into users_invoices so your suspension task picks it up
	_, err = tx.ExecContext(ctx, `
        INSERT INTO users_invoices (workspace_id, status, due_date, created_at, updated_at) 
        VALUES (?, 'UNPAID', ?, NOW(), NOW())`, task.WorkspaceID, time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		return fmt.Errorf("failed to write unpaid fallback invoice: %v", err)
	}

	// 2. Safely push the date forward so the engine does not trigger another attempt at midnight
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