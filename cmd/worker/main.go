package main

import (
	"encoding/json"
	"log"
	"os"

	"lineblocs.com/crontabs/internal/billing"
	"lineblocs.com/crontabs/models"
	"lineblocs.com/crontabs/repository"
	"lineblocs.com/crontabs/utils"

	amqp "github.com/rabbitmq/amqp091-go"
)

func main() {
	db, _ := utils.GetDBConnection()
	wRepo := repository.NewWorkspaceRepository(db)
	pRepo := repository.NewPaymentRepository(db)
	billingSvc := billing.NewBillingService(db, wRepo, pRepo)


	conn, err := amqp.Dial(os.Getenv("QUEUE_URL"))
	if err != nil {
		panic(err)
	}

	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		panic(err)
	}
	defer ch.Close()

	// Prefetch(1) ensures the worker doesn't hog all tasks if one is slow
	ch.Qos(1, 0, false)
	msgs, err := ch.Consume("billing_tasks", "", false, false, false, false, nil)
	if err != nil {
		panic(err)
	}

	log.Println("Worker ready. Waiting for tasks...")

	for d := range msgs {
		var task models.BillingTask
		json.Unmarshal(d.Body, &task)

		err := billingSvc.ProcessTask(task)
		if err != nil {
			log.Printf("Error processing workspace %d: %v", task.WorkspaceID, err)
			d.Nack(false, true) // Requeue for retry
		} else {
			d.Ack(false)
		}
	}
}