package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"lineblocs.com/crontabs/models"
	"lineblocs.com/crontabs/utils"

	_ "github.com/go-sql-driver/mysql"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"github.com/robfig/cron/v3"
)

var rdb *redis.Client

func main() {
	// 1. INITIALIZE REDIS
	redisURL := os.Getenv("REDIS_URL")
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Fatalf("Critical: Failed to parse REDIS_URL: %v", err)
	}
	rdb = redis.NewClient(opt)

	// Test Redis Connection
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("Critical: Could not connect to Redis: %v", err)
	}

	// 2. SETUP SCHEDULER
	c := cron.New()

	// PRODUCTION: Monthly Billing (Midnight on the 1st)
	_, _ = c.AddFunc("0 0 1 * *", func() {
		log.Println("[PROD] Triggering Monthly Billing...")
		runBillingDistributor("monthly")
	})

	// PRODUCTION: Yearly Billing (Midnight on Jan 1st)
	_, _ = c.AddFunc("0 0 1 1 *", func() {
		log.Println("[PROD] Triggering Yearly Billing...")
		runBillingDistributor("yearly")
	})

	// DEBUG: Every Minute (only if DISTRIBUTOR_DEBUG is set to 1)
	if os.Getenv("DISTRIBUTOR_DEBUG") == "1" {
		_, _ = c.AddFunc("* * * * *", func() {
			log.Println("[DEBUG] Running per-minute test trigger...")
			runBillingDistributor("monthly-debug")
		})
	}

	log.Printf("Billing Task Distributor started. Connected to Redis at: %s", opt.Addr)
	c.Start()

	// Keep the app running
	select {}
}

func runBillingDistributor(scheduleType string) {
	// 2-hour safety timeout for the entire process
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	// --- GLOBAL LOCK LOGIC ---
	var lockKeySuffix string
	var lockTTL time.Duration

	if scheduleType == "monthly-debug" {
		lockKeySuffix = time.Now().Format("2006-01-02-15:04") // Unique per minute
		lockTTL = 50 * time.Second                           // Expire just before next minute
	} else if scheduleType == "yearly" {
		lockKeySuffix = time.Now().Format("2006")
		lockTTL = 23 * time.Hour
	} else {
		lockKeySuffix = time.Now().Format("2006-01")
		lockTTL = 23 * time.Hour
	}

	globalLockKey := fmt.Sprintf("billing_run_lock:%s:%s", scheduleType, lockKeySuffix)

	// SET NX: Only one instance/replica will succeed here
	locked, err := rdb.SetNX(ctx, globalLockKey, "running", lockTTL).Result()
	if err != nil || !locked {
		log.Printf("[%s] Skip: Lock %s held by another instance.", scheduleType, globalLockKey)
		return
	}

	log.Printf("[%s] Lock Acquired. Processing distribution...", scheduleType)

	// --- CONNECTIONS ---
	db, _ := utils.GetDBConnection()
	//defer db.Close()

	conn, err := amqp.Dial(os.Getenv("QUEUE_URL"))
	if err != nil {
		log.Printf("[%s] RabbitMQ connection failed: %v", scheduleType, err)
		return
	}
	defer conn.Close()

	ch, _ := conn.Channel()
	defer ch.Close()

	// Put channel in Confirm Mode
	if err := ch.Confirm(false); err != nil {
		log.Printf("[%s] Could not enable RabbitMQ confirms: %v", scheduleType, err)
		return
	}
	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))

	q, _ := ch.QueueDeclare("billing_tasks", true, false, false, false, nil)

	// --- DATABASE QUERY ---
	queryTerm := scheduleType
	if scheduleType == "monthly-debug" {
		queryTerm = "monthly" // Simulate monthly data during debug
	}

	rows, err := db.Query("SELECT id, creator_id, plan_term FROM workspaces WHERE plan_term = ?", queryTerm)
	if err != nil {
		log.Printf("[%s] DB Query Error: %v", scheduleType, err)
		return
	}
	defer rows.Close()

	// --- DISTRIBUTION LOOP ---
	count := 0
	for rows.Next() {
		var task models.BillingTask
		var term sql.NullString
		rows.Scan(&task.WorkspaceID, &task.CreatorID, &term)

		// DEDUPLICATION: Ensures no workspace is queued twice in the same cycle
		dedupeKey := fmt.Sprintf("queued:%s:%d:%s", scheduleType, task.WorkspaceID, lockKeySuffix)
		isNew, _ := rdb.SetNX(ctx, dedupeKey, "true", 31*24*time.Hour).Result()
		if !isNew {
			continue 
		}

		task.BillingType = queryTerm
		task.RunID = globalLockKey 
		body, _ := json.Marshal(task)

		err = ch.PublishWithContext(ctx, "", q.Name, false, false, amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			Body:         body,
		})

		if err != nil {
			rdb.Del(ctx, dedupeKey) // Failed to publish, allow retry
			log.Printf("Publish error for workspace %d: %v", task.WorkspaceID, err)
			continue
		}

		// Confirm receipt by RabbitMQ
		select {
		case confirmed := <-confirms:
			if !confirmed.Ack {
				rdb.Del(ctx, dedupeKey)
				log.Printf("RabbitMQ NACK for %d", task.WorkspaceID)
			} else {
				count++
			}
		case <-time.After(5 * time.Second):
			rdb.Del(ctx, dedupeKey)
			log.Printf("Timeout waiting for RabbitMQ ACK for %d", task.WorkspaceID)
		}
	}

	log.Printf("[%s] Distribution Finished. Total Queued: %d", scheduleType, count)
}