package main

import (
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

type RabbitMQPublisher struct {
	channel *amqp.Channel
}

func (p *RabbitMQPublisher) Publish(queue string, message []byte) error {
	return p.channel.Publish("", queue, false, false, amqp.Publishing{
		ContentType: "application/json",
		Body:        message,
	})
}

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

	publisher := &RabbitMQPublisher{channel: ch}

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

		// 1. ATTEMPT PAYMENT PROCESSING
		// Inside ProcessTask, your logic should record the transaction and handle card failures.
		err := billingSvc.ProcessTask(task)
		if err != nil {
			log.Printf("Payment processing failed for Workspace %d: %v", task.WorkspaceID, err)
			// Acknowledge the message to remove from queue, but log the failure.
			d.Ack(false)
			continue
		}

		// 2. SUCCESS -> UPDATE THE SUBSCRIPTION RECORD
		// We only move the date forward if the payment call above succeeded.
		if err := finishBillingCycle(db, task); err != nil {
			log.Printf("CRITICAL: Payment succeeded but DB update failed for Workspace %d: %v", task.WorkspaceID, err)
		} else {
			log.Printf("SUCCESS: Workspace %d billed and cycle advanced to %s.", task.WorkspaceID, task.NextBillingDate)
		}

		d.Ack(false)
	}
}

func finishBillingCycle(db *sql.DB, task models.BillingTask) error {
	// Parse the date defined by the Distributor
	nextDate, err := time.Parse("2006-01-02", task.NextBillingDate)
	if err != nil {
		return fmt.Errorf("invalid date format in task: %v", err)
	}

	// Update the database to advance the cycle.
	// We also clear out any scheduled plans because they have now been applied (Action: "upgrade").
	_, err = db.Exec(`
		UPDATE subscriptions 
		SET next_billing_date = ?, 
		    current_plan_id = ?, 
		    scheduled_plan_id = NULL, 
		    scheduled_effective_date = NULL 
		WHERE id = ?`, nextDate, task.PlanToBill, task.SubscriptionID)

	return err
}