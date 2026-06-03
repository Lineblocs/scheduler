package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"time"
	"lineblocs.com/scheduler/internal/billing"
	"lineblocs.com/scheduler/models"
	"lineblocs.com/scheduler/repository"
	"lineblocs.com/scheduler/utils"
	"github.com/sirupsen/logrus"
	amqp "github.com/rabbitmq/amqp091-go"
	helpers "github.com/Lineblocs/go-helpers"
)

const (
	suspensionQueueName = "workspace_suspensions_tasks"
	upgradeQueueName    = "workspace_upgrades"
)

func publishWorkspaceSuspended(ch *amqp.Channel, task models.SuspensionTask) error {
	body, err := json.Marshal(task)
	if err != nil {
		return err
	}
	err = ch.Publish(
		"",
		"workspace_suspended",
		false,
		false,
		amqp.Publishing{
			ContentType: "application/json",
			Body:        body,
		},
	)
	return err
}

func processSuspensions(db *sql.DB, ch *amqp.Channel) {
	// Ensure queue exists
	q, err := ch.QueueDeclare(suspensionQueueName, true, false, false, false, nil)
	if err != nil {
		panic(err)
	}

	msgs, _ := ch.Consume(q.Name, "", false, false, false, false, nil)

	log.Println("Workspace suspensions consumer started...")

	for d := range msgs {
		var task models.SuspensionTask
		if err := json.Unmarshal(d.Body, &task); err != nil {
			log.Printf("Error decoding task: %v", err)
			d.Ack(false) // Drop malformed messages
			continue
		}

		now := time.Now()
		daysSinceSuspension := now.Sub(task.SuspensionInitiatedAt).Hours() / 24

		switch {
		// Case 1: Not a follow-up and grace period is nil - insert with SUSPENDED status
		case !task.IsFollowUp && task.GracePeriodExtension == nil:
			_, err := db.Exec("INSERT INTO workspaces_suspensions (workspace_id, suspension_initiated_at, grace_period_extension, reason, status, suspended_at) VALUES (?, ?, ?, ?, ?, ?)",
				task.WorkspaceID, task.SuspensionInitiatedAt, task.GracePeriodExtension, task.Reason, "SUSPENDED", now)
			if err != nil {
				log.Printf("Error inserting into workspaces_suspensions: %v", err)
			}
			if err := publishWorkspaceSuspended(ch, task); err != nil {
				log.Printf("Error publishing workspace suspended: %v", err)
			}
			log.Println("Workspace suspension record created with SUSPENDED status")

		// Case 2: Is a follow-up and grace period has expired - suspend the workspace
		case task.IsFollowUp && (task.GracePeriodExtension == nil || daysSinceSuspension > float64(*task.GracePeriodExtension)):
			_, err := db.Exec("UPDATE workspaces_suspensions SET status = 'SUSPENDED', suspended_at = ? WHERE id = ?",
				now, task.ID)
			if err != nil {
				log.Printf("Error updating workspaces_suspensions to SUSPENDED: %v", err)
			}
			if err := publishWorkspaceSuspended(ch, task); err != nil {
				log.Printf("Error publishing workspace suspended: %v", err)
			}
			log.Println("Workspace suspension status updated to SUSPENDED")

		// Case 3: Default - add suspension record with all fields
		default:
			_, err := db.Exec("INSERT INTO workspaces_suspensions (workspace_id, suspension_initiated_at, grace_period_extension, reason, status) VALUES (?, ?, ?, ?, ?)",
				task.WorkspaceID, task.SuspensionInitiatedAt, task.GracePeriodExtension, task.Reason, task.Status)
			if err != nil {
				log.Printf("Error inserting into workspaces_suspensions: %v", err)
			}
			log.Println("Workspace suspension record created")
		}

		d.Ack(true)
		log.Println("Workspace suspension initiated")
	}
}

func processWorkspaceUpgrades(db *sql.DB, ch *amqp.Channel) {
	// Ensure queue exists
	q, err := ch.QueueDeclare(upgradeQueueName, true, false, false, false, nil)
	if err != nil {
		panic(err)
	}

	msgs, _ := ch.Consume(q.Name, "", false, false, false, false, nil)

	log.Println("Workspace upgrades consumer started...")

	wRepo := repository.NewWorkspaceRepository(db)
	pRepo := repository.NewPaymentRepository(db)
	svc := billing.NewBillingService(db, wRepo, pRepo, nil)

	for d := range msgs {
		var task models.WorkspaceUpgradeTask
		if err := json.Unmarshal(d.Body, &task); err != nil {
			log.Printf("Error decoding upgrade task: %v", err)
			d.Ack(false) // Drop malformed messages
			continue
		}

		logger := logrus.WithField("component", "workspace-upgrades").WithField("workspace_id", task.WorkspaceID)
		if err := svc.HandleUpgrade(task, logger); err != nil {
			log.Printf("Error handling workspace upgrade: %v", err)
			d.Ack(false)
			continue
		}

		d.Ack(true)
		log.Println("Workspace upgrade processed")
	}
}

func main() {
	logDestination := utils.Config("LOG_DESTINATIONS")
	helpers.InitLogrus(logDestination)

	db, err := utils.GetDBConnection()
	if err != nil {
		panic(err)
	}
	//settings, _ := utils.GetSettingsFromAPI()

	conn, err := amqp.Dial(os.Getenv("QUEUE_URL"))
	if err != nil {
		panic(err)
	}

	ch, err := conn.Channel()
	if err != nil {
		panic(err)
	}

	log.Println("Workspace tasks worker started...")

	// Start consumers in goroutines
	go processSuspensions(db, ch)
	go processWorkspaceUpgrades(db, ch)


	// Keep main running
	select {}
}