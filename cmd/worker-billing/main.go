package main

import (
	"encoding/json"
	"log"
	"os"

	helpers "github.com/Lineblocs/go-helpers"
	"lineblocs.com/scheduler/internal/billing"
	"lineblocs.com/scheduler/models"
	"lineblocs.com/scheduler/repository"
	"lineblocs.com/scheduler/utils"

	amqp "github.com/rabbitmq/amqp091-go"
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

	db, _ := utils.GetDBConnection()
	wRepo := repository.NewWorkspaceRepository(db)
	pRepo := repository.NewPaymentRepository(db)

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

	publisher := &RabbitMQPublisher{channel: ch}
	billingSvc := billing.NewBillingServiceWithPublisher(db, wRepo, pRepo, publisher)

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