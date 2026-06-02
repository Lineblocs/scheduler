package main

import (
	"encoding/json"
	"log"
	"os"
	"time"
	"lineblocs.com/scheduler/models"
	"lineblocs.com/scheduler/utils"
	amqp "github.com/rabbitmq/amqp091-go"
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

func processSuspensions(db interface{}, ch *amqp.Channel) {
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
			_, err := db.(*interface{}).Exec("INSERT INTO workspaces_suspensions (workspace_id, suspension_initiated_at, grace_period_extension, reason, status, suspended_at) VALUES (?, ?, ?, ?, ?, ?)",
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
			_, err := db.(*interface{}).Exec("UPDATE workspaces_suspensions SET status = 'SUSPENDED', suspended_at = ? WHERE id = ?",
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
			_, err := db.(*interface{}).Exec("INSERT INTO workspaces_suspensions (workspace_id, suspension_initiated_at, grace_period_extension, reason, status) VALUES (?, ?, ?, ?, ?)",
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

func processWorkspaceUpgrades(db interface{}, ch *amqp.Channel) {
	// Ensure queue exists
	q, err := ch.QueueDeclare(upgradeQueueName, true, false, false, false, nil)
	if err != nil {
		panic(err)
	}

	msgs, _ := ch.Consume(q.Name, "", false, false, false, false, nil)

	log.Println("Workspace upgrades consumer started...")

	for d := range msgs {
		var task models.WorkspaceUpgradeTask
		if err := json.Unmarshal(d.Body, &task); err != nil {
			log.Printf("Error decoding upgrade task: %v", err)
			d.Ack(false) // Drop malformed messages
			continue
		}

		now := time.Now()

		// Create proration invoice line item if proration amount > 0
		if task.UpgradeFee > 0 {
			_, err := db.(*interface{}).Exec(
				"INSERT INTO user_invoice_line_items (created_at, updated_at, is_recurring, name, cents, invoice_id, workspace_id, key_name) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
				now, now, 0, "Upgrade Adjustment: "+task.OldPlanName+" to "+task.NewPlanName, task.UpgradeFee, nil, task.WorkspaceID, "plan_upgrade_proration",
			)
			if err != nil {
				log.Printf("Error creating invoice line item: %v", err)
			}
		}

		// Update subscription with new plan details
		_, err := db.(*interface{}).Exec(
			"UPDATE workspace_subscriptions SET scheduled_plan_id = ?, scheduled_effective_date = ?, updated_at = ? WHERE workspace_id = ?",
			task.NewPlanID, task.ScheduledEffectiveDate, now, task.WorkspaceID,
		)
		if err != nil {
			log.Printf("Error updating workspace subscription: %v", err)
		}

		// End current plan usage period
		_, err = db.(*interface{}).Exec(
			"UPDATE plan_usage_periods SET ended_at = ? WHERE workspace_id = ? AND ended_at IS NULL",
			now, task.WorkspaceID,
		)
		if err != nil {
			log.Printf("Error ending plan usage period: %v", err)
		}

		// Create new plan usage period
		_, err = db.(*interface{}).Exec(
			"INSERT INTO plan_usage_periods (workspace_id, started_at, plan) VALUES (?, ?, ?)",
			task.WorkspaceID, now, task.PlanKey,
		)
		if err != nil {
			log.Printf("Error creating new plan usage period: %v", err)
		}

		log.Printf("Processing workspace upgrade for workspace_id: %v", task.WorkspaceID)


		d.Ack(true)
		log.Println("Workspace upgrade processed")
	}
}

func main() {
	db, _ := utils.GetDBConnection()
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