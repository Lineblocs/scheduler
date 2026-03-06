package main

import (
	"encoding/json"
	"fmt"
	"os"

	"go.uber.org/zap"
	"lineblocs.com/scheduler/internal/config"
	"lineblocs.com/scheduler/internal/queue"
	"lineblocs.com/scheduler/models"
)

// CLI is a thin task producer. It publishes tasks to queues; workers do the work.
func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	log, _ := zap.NewProduction()
	defer log.Sync()

	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Println("Usage: cli <command> [args...]")
		fmt.Println("Commands: monthly_billing, annual_billing, retry_billing, cleanup, remove_logs, background_emails")
		os.Exit(1)
	}

	conn, err := queue.NewConnection(cfg)
	if err != nil {
		log.Fatal("failed to connect to RabbitMQ", zap.Error(err))
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		log.Fatal("failed to open channel", zap.Error(err))
	}
	defer ch.Close()

	publisher := queue.NewRabbitPublisher(ch)

	command := args[0]
	switch command {
	case "monthly_billing":
		task := map[string]string{"activity": "billing.monthly"}
		body, _ := json.Marshal(task)
		err = publisher.Publish(cfg.Rabbit.BillingTasksQueue, body)

	case "annual_billing":
		task := map[string]string{"activity": "billing.annual"}
		body, _ := json.Marshal(task)
		err = publisher.Publish(cfg.Rabbit.BillingTasksQueue, body)

	case "retry_billing":
		task := map[string]string{"activity": "billing.retry"}
		body, _ := json.Marshal(task)
		err = publisher.Publish(cfg.Rabbit.BillingTasksQueue, body)

	case "cleanup":
		task := map[string]string{"activity": "cleanup.stale_users"}
		body, _ := json.Marshal(task)
		err = publisher.Publish("maintenance_tasks", body)

	case "remove_logs":
		task := map[string]string{"activity": "logs.purge"}
		body, _ := json.Marshal(task)
		err = publisher.Publish("maintenance_tasks", body)

	case "background_emails":
		// Publish individual email tasks
		for _, emailActivity := range []string{"email.inactive", "email.card_expiry", "email.free_trial", "email.satisfaction", "email.usage_trigger"} {
			task := map[string]string{"activity": emailActivity}
			body, _ := json.Marshal(task)
			if pubErr := publisher.Publish("email_tasks", body); pubErr != nil {
				log.Error("failed to publish email task", zap.String("activity", emailActivity), zap.Error(pubErr))
			}
		}

	case "prorated_billing":
		if len(args) < 2 {
			fmt.Println("Usage: cli prorated_billing <json-task>")
			os.Exit(1)
		}
		var task models.BillingTask
		if err := json.Unmarshal([]byte(args[1]), &task); err != nil {
			log.Fatal("invalid task JSON", zap.Error(err))
		}
		body, _ := json.Marshal(task)
		err = publisher.Publish(cfg.Rabbit.BillingTasksQueue, body)

	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", command)
		os.Exit(1)
	}

	if err != nil {
		log.Fatal("failed to publish task", zap.String("command", command), zap.Error(err))
	}

	log.Info("task published", zap.String("command", command))
}
