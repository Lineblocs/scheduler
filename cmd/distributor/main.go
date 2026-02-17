package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"os"

	"lineblocs.com/crontabs/models"
	"lineblocs.com/crontabs/utils"

	_ "github.com/go-sql-driver/mysql"
	amqp "github.com/rabbitmq/amqp091-go"
)

func main() {
	db, _ := utils.GetDBConnection()
	defer db.Close()


	conn, err := amqp.Dial(os.Getenv("QUEUE_URL"))
	if err != nil { log.Fatal(err) }
	defer conn.Close()

	ch, _ := conn.Channel()
	defer ch.Close()

	q, _ := ch.QueueDeclare("billing_tasks", true, false, false, false, nil)

	rows, err := db.Query("SELECT id, creator_id, plan_term FROM workspaces")
	if err != nil { log.Fatal(err) }
	defer rows.Close()

	for rows.Next() {
		var task models.BillingTask
		var term sql.NullString
		rows.Scan(&task.WorkspaceID, &task.CreatorID, &term)

		task.BillingType = "monthly"
		if term.Valid && term.String != "" {
			task.BillingType = term.String
		}

		body, _ := json.Marshal(task)
		ch.PublishWithContext(context.Background(), "", q.Name, false, false, amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			Body:         body,
		})
	}
	log.Println("Distribution complete.")
}