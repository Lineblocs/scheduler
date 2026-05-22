package main

import (
	"encoding/json"
	"log"
	"os"
	"lineblocs.com/scheduler/models"
	"lineblocs.com/scheduler/utils"
	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	suspensionQueueName = "workspace_suspensions_tasks"
)



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
	
	// Ensure queue exists
	q, err := ch.QueueDeclare(suspensionQueueName, true, false, false, false, nil)
	if err != nil {
		panic(err)
	}

	msgs, _ := ch.Consume(q.Name, "", false, false, false, false, nil)

	log.Println("Workspace tasks worker started...")

	for d := range msgs {
		var task models.SuspensionTask
		if err := json.Unmarshal(d.Body, &task); err != nil {
			log.Printf("Error decoding task: %v", err)
			d.Ack(false) // Drop malformed messages
			continue
		}

		_, err := db.Exec("INSERT INTO workspaces_suspensions (workspace_id, suspended_at, grace_period_extension, reason, status) VALUES (?, ?, ?, ?, ?)",
			task.WorkspaceID, task.SuspendedAt, task.GracePeriodExtension, task.Reason, task.Status)
		if err != nil {
			log.Printf("Error inserting into workspaces_suspensions: %v", err)
		}

		body, err := json.Marshal(task)
		if err == nil {
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
			if err != nil {
				log.Printf("Error publishing to workspace_suspended: %v", err)
			}
		}

		d.Ack(true)
		log.Println("Workspace was suspended")

	}
}