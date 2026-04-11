package main

import (
	"database/sql"
	"encoding/json"
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
		err := billingSvc.ProcessTask(task)
		if err != nil {
			log.Printf("Payment processing failed for Workspace %d: %v", task.WorkspaceID, err)
			// Decide here if you want to retry or Ack and log failure.
			// Usually, for billing, we Ack and mark the subscription as 'past_due' in the DB inside ProcessTask.
			d.Ack(false)
			continue
		}

		// 2. SUCCESS -> UPDATE THE SUBSCRIPTION CYCLE
		if err := finishBillingCycle(db, task); err != nil {
			log.Printf("CRITICAL: Payment succeeded but DB update failed for Workspace %d: %v", task.WorkspaceID, err)
		} else {
			log.Printf("SUCCESS: Workspace %d billed and cycle advanced.", task.WorkspaceID)
		}

		d.Ack(false)
	}
}

func finishBillingCycle(db *sql.DB, task models.BillingTask) error {
	var cycle string
	var anchor int
	
	// Fetch necessary metadata
	err := db.QueryRow("SELECT billing_cycle, billing_anchor_day FROM subscriptions WHERE id = ?", task.SubscriptionID).Scan(&cycle, &anchor)
	if err != nil {
		return err
	}

	// Logic to calculate the next date (handling end-of-month and annual cycles)
	//nextDate := utils.CalculateNextDate(time.Now().UTC(), cycle, anchor)
	nextDate, err := time.Parse("2006-01-02", task.NextBillingDate)
	if err != nil {
		return err
	}
	// Update DB: Move date forward and clean up any processed scheduled plans (upgrades)
	_, err = db.Exec(`
		UPDATE subscriptions 
		SET next_billing_date = ?, 
		    current_plan_id = ?, 
		    scheduled_plan_id = NULL, 
		    scheduled_effective_date = NULL 
		WHERE id = ?`, nextDate, task.PlanToBill, task.SubscriptionID)

	return err
}