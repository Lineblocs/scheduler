package main

import (
	"encoding/json"
	amqp "github.com/rabbitmq/amqp091-go"
	"lineblocs.com/scheduler/internal/storage"
	"lineblocs.com/scheduler/models"
	"lineblocs.com/scheduler/utils"
	"log"
	"os"
)

const (
	queueName = "recording_tasks"
)

func main() {
	db, _ := utils.GetDBConnection()
	ariClient, _ := utils.CreateARIConnection()
	settings, _ := utils.GetSettingsFromAPI() // Centralized settings fetcher

	storageSvc := storage.NewRecordingService(db, ariClient, settings)

	conn, err := amqp.Dial(os.Getenv("QUEUE_URL"))
	if err != nil {
		panic(err)
	}

	ch, err := conn.Channel()
	if err != nil {
		panic(err)
	}

	// Ensure queue exists
	q, err := ch.QueueDeclare(queueName, true, false, false, false, nil)
	if err != nil {
		panic(err)
	}

	msgs, err := ch.Consume(q.Name, "", false, false, false, false, nil)
	if err != nil {
		panic(err)
	}

	log.Println("S3 Recording Worker Started...")

	for d := range msgs {
		var task models.RecordingTask
		if err := json.Unmarshal(d.Body, &task); err != nil {
			log.Printf("Error decoding task: %v", err)
			d.Ack(false) // Drop malformed messages
			continue
		}

		if err := storageSvc.ProcessRecording(task); err != nil {
			log.Printf("Worker failed to process recording %d: %v", task.ID, err)
			d.Nack(false, true) // Requeue for retry
		} else {
			d.Ack(false)
		}
	}
}
