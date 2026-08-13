package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"time"

	helpers "github.com/Lineblocs/go-helpers"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/sirupsen/logrus"
	"lineblocs.com/scheduler/internal/billing"
	"lineblocs.com/scheduler/models"
	"lineblocs.com/scheduler/repository"
	"lineblocs.com/scheduler/utils"
)

const (
	suspensionQueueName = "workspace_suspensions_tasks"
	upgradeQueueName    = "workspace_upgrades"
	callFraudQueueName  = "workspace_call_fraud"
)

type FraudRiskProfile struct {
	MaxTotalCalls         int
	MaxShortCalls         int
	MaxUniqueDestinations int
	AutoSuspendThreshold  int
	MinAvgMosScore        float64
	MinCallsForMosEval    int
}

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
	// ensure queue exists
	q, err := ch.QueueDeclare(suspensionQueueName, true, false, false, false, nil)
	if err != nil {
		panic(err)
	}

	msgs, _ := ch.Consume(q.Name, "", false, false, false, false, nil)

	log.Println("workspace suspensions consumer started...")

	for d := range msgs {
		var task models.SuspensionTask
		if err := json.Unmarshal(d.Body, &task); err != nil {
			log.Printf("error decoding task: %v", err)
			d.Ack(false) // drop malformed messages
			continue
		}

		now := time.Now()
		daysSinceSuspension := now.Sub(task.SuspensionInitiatedAt).Hours() / 24

		switch {
		// case 1: not a follow-up and grace period is nil - insert with suspended status
		case !task.IsFollowUp && task.GracePeriodExtension == nil:
			_, err := db.Exec("INSERT INTO workspaces_suspensions (workspace_id, suspension_initiated_at, grace_period_extension, reason, status, suspended_at, invoice_id) VALUES (?, ?, ?, ?, ?, ?, ?)",
				task.WorkspaceID, task.SuspensionInitiatedAt, task.GracePeriodExtension, task.Reason, "SUSPENDED", now, task.InvoiceID)
			if err != nil {
				log.Printf("error inserting into workspaces_suspensions: %v", err)
			}
			if err := publishWorkspaceSuspended(ch, task); err != nil {
				log.Printf("error publishing workspace suspended: %v", err)
			}
			log.Println("workspace suspension record created with suspended status")

		// case 3: not a follow-up and grace period is set - add suspension record with initiated status
		case !task.IsFollowUp && task.GracePeriodExtension != nil:
			_, err := db.Exec("INSERT INTO workspaces_suspensions (workspace_id, suspension_initiated_at, grace_period_extension, reason, status, invoice_id) VALUES (?, ?, ?, ?, ?, ?)",
				task.WorkspaceID, task.SuspensionInitiatedAt, task.GracePeriodExtension, task.Reason, "INITIATED", task.InvoiceID)
			if err != nil {
				log.Printf("error inserting into workspaces_suspensions: %v", err)
			}
			log.Println("workspace suspension record created")

		// case 2: is a follow-up and grace period has expired - suspend the workspace
		case task.IsFollowUp && (task.GracePeriodExtension == nil || daysSinceSuspension > float64(*task.GracePeriodExtension)):
			_, err := db.Exec("UPDATE workspaces_suspensions SET status = 'SUSPENDED', suspended_at = ? WHERE id = ?",
				now, task.ID)
			if err != nil {
				log.Printf("error updating workspaces_suspensions to suspended: %v", err)
			}
			if err := publishWorkspaceSuspended(ch, task); err != nil {
				log.Printf("error publishing workspace suspended: %v", err)
			}
			log.Println("workspace suspension status updated to suspended")
		}

		d.Ack(true)
		log.Println("workspace suspension initiated")
	}
}

func processWorkspaceUpgrades(db *sql.DB, ch *amqp.Channel) {
	// ensure queue exists
	q, err := ch.QueueDeclare(upgradeQueueName, true, false, false, false, nil)
	if err != nil {
		panic(err)
	}

	msgs, _ := ch.Consume(q.Name, "", false, false, false, false, nil)

	log.Println("workspace upgrades consumer started...")

	wRepo := repository.NewWorkspaceRepository(db)
	pRepo := repository.NewPaymentRepository(db)

	publisher := billing.NewGenericRabbitMQPublisher(ch)
	customizations, err := helpers.GetCustomizationKVs()
	if err != nil {
		log.Printf("warning: could not load customizations: %v", err)
	}
	svc := billing.NewBillingServiceWithPublisher(db, wRepo, pRepo, customizations, publisher)

	for d := range msgs {
		var task models.WorkspaceUpgradeTask
		if err := json.Unmarshal(d.Body, &task); err != nil {
			log.Printf("error decoding upgrade task: %v", err)
			d.Ack(false) // drop malformed messages
			continue
		}

		logger := logrus.WithField("component", "workspace-upgrades").WithField("workspace_id", task.WorkspaceID)
		if err := svc.HandleUpgrade(task, logger); err != nil {
			log.Printf("error handling workspace upgrade: %v", err)
			d.Ack(false)
			continue
		}

		d.Ack(true)
		log.Println("workspace upgrade processed")
	}
}

func processCallFraud(db *sql.DB, ch *amqp.Channel) {
	// ensure queue exists
	q, err := ch.QueueDeclare(callFraudQueueName, true, false, false, false, nil)
	if err != nil {
		panic(err)
	}

	msgs, _ := ch.Consume(q.Name, "", false, false, false, false, nil)

	log.Println("workspace call fraud consumer started...")

	// define profiles in a map
	riskProfiles := map[string]FraudRiskProfile{
		"HIGH": {
			MaxTotalCalls:         100,
			MaxShortCalls:         30,
			MaxUniqueDestinations: 25,
			AutoSuspendThreshold:  150,
			MinAvgMosScore:        3.0,
			MinCallsForMosEval:    50,
		},
		"MEDIUM": {
			MaxTotalCalls:         500,
			MaxShortCalls:         150,
			MaxUniqueDestinations: 150,
			AutoSuspendThreshold:  750,
			MinAvgMosScore:        3.0,
			MinCallsForMosEval:    250,
		},
		"LOW": {
			MaxTotalCalls:         2000,
			MaxShortCalls:         500,
			MaxUniqueDestinations: 500,
			AutoSuspendThreshold:  3000,
			MinAvgMosScore:        3.0,
			MinCallsForMosEval:    1000,
		},
	}

	// fallback profile
	defaultProfile := FraudRiskProfile{
		MaxTotalCalls:         1000,
		MaxShortCalls:         300,
		MaxUniqueDestinations: 250,
		AutoSuspendThreshold:  1500,
		MinAvgMosScore:        3.0,
		MinCallsForMosEval:    500,
	}

	for d := range msgs {
		var task models.CallFraudTask
		if err := json.Unmarshal(d.Body, &task); err != nil {
			log.Printf("error decoding call fraud task: %v", err)
			d.Ack(false) // drop malformed messages
			continue
		}

		log.Printf("processing call fraud event for workspace: %v", task.WorkspaceID)

		// query the calls table and left join call quality metrics
		query := `
			SELECT 
				COUNT(c.id) as total_calls,
				COALESCE(SUM(c.duration), 0) as total_duration,
				COUNT(DISTINCT c.` + "`to`" + `) as unique_destinations,
				COALESCE(SUM(CASE WHEN c.duration <= 5 THEN 1 ELSE 0 END), 0) as short_calls,
				COALESCE(AVG(cqm.mos_score), 5.0) as avg_mos_score
			FROM calls c
			LEFT JOIN call_quality_metrics cqm ON cqm.call_id = c.id
			WHERE c.workspace_id = ? 
			  AND c.direction = 'OUTBOUND' 
			  AND c.started_at >= ?
		`

		var totalCalls, totalDuration, uniqueDestinations, shortCalls int
		var avgMosScore float64
		err := db.QueryRow(query, task.WorkspaceID, task.StartDatetimeOfFraudCheck).
			Scan(&totalCalls, &totalDuration, &uniqueDestinations, &shortCalls, &avgMosScore)

		if err != nil {
			log.Printf("error querying call stats for workspace %d: %v", task.WorkspaceID, err)
			d.Ack(false)
			continue
		}

		// load the correct threshold profile
		profile, exists := riskProfiles[task.AccountRiskLevel]
		if !exists {
			profile = defaultProfile
		}

		// evaluate the heuristics for fraud
		isFraud := false
		fraudReason := ""
		needsSuspension := false

		if totalCalls > profile.MaxTotalCalls {
			isFraud = true
			fraudReason = "excessive outbound call volume"
			if totalCalls >= profile.AutoSuspendThreshold {
				needsSuspension = true
			}
		} else if shortCalls > profile.MaxShortCalls {
			isFraud = true
			fraudReason = "high volume of short-duration calls"
			if shortCalls >= profile.AutoSuspendThreshold {
				needsSuspension = true
			}
		} else if uniqueDestinations > profile.MaxUniqueDestinations {
			isFraud = true
			fraudReason = "excessive unique destination numbers (potential toll fraud)"
			if uniqueDestinations >= profile.AutoSuspendThreshold {
				needsSuspension = true
			}
		} else if avgMosScore < profile.MinAvgMosScore && totalCalls > profile.MinCallsForMosEval {
			isFraud = true
			fraudReason = "low average mos score combined with high call volume (potential automated dialer)"
			if totalCalls >= profile.AutoSuspendThreshold {
				needsSuspension = true
			}
		}

		// handle the result
		if isFraud {
			log.Printf("fraud detected for workspace %d: %s (total: %d, short: %d, unique dests: %d, avg mos: %.2f)",
				task.WorkspaceID, fraudReason, totalCalls, shortCalls, uniqueDestinations, avgMosScore)

			if needsSuspension {
				now := time.Now()
				suspendTask := models.SuspensionTask{
					WorkspaceID:           task.WorkspaceID,
					InvoiceID:             0,
					Status:                "INITIATED",
					Reason:                "auto-suspended due to high fraud detection: " + fraudReason,
					GracePeriodExtension:  nil,
					SuspensionInitiatedAt: now,
					IsFollowUp:            false,
				}

				body, _ := json.Marshal(suspendTask)

				err := ch.Publish(
					"",
					"workspace_suspensions_tasks",
					false,
					false,
					amqp.Publishing{
						ContentType: "application/json",
						Body:        body,
					},
				)

				if err != nil {
					log.Printf("error publishing suspension task for workspace %d: %v", task.WorkspaceID, err)
				} else {
					log.Printf("workspace %d suspension event published successfully", task.WorkspaceID)
				}
			}
		} else {
			log.Printf("workspace %d passed fraud check.", task.WorkspaceID)
		}

		d.Ack(true)
		log.Println("workspace call fraud event processed")
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

	log.Println("workspace tasks worker started...")

	// start consumers in goroutines
	go processSuspensions(db, ch)
	go processWorkspaceUpgrades(db, ch)
	go processCallFraud(db, ch)

	// keep main running
	select {}
}