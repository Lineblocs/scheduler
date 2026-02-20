package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	helpers "github.com/Lineblocs/go-helpers"
	"lineblocs.com/crontabs/models"
	"lineblocs.com/crontabs/utils"

	_ "github.com/go-sql-driver/mysql"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"github.com/robfig/cron/v3"
)

var rdb *redis.Client

func main() {

	logDestination := utils.Config("LOG_DESTINATIONS")
	helpers.InitLogrus(logDestination)

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
		runBillingDistributor("MONTHLY")
	})

	// PRODUCTION: Yearly Billing (Midnight on Jan 1st)
	_, _ = c.AddFunc("0 0 1 1 *", func() {
		log.Println("[PROD] Triggering Yearly Billing...")
		runBillingDistributor("ANNUAL")
	})

	// DEBUG: Every Minute (only if DISTRIBUTOR_DEBUG is set to 1)
	if os.Getenv("DISTRIBUTOR_DEBUG") == "1" {
		_, _ = c.AddFunc("* * * * *", func() {
			log.Println("[DEBUG] Running per-minute test trigger...")
			runBillingDistributor("MONTHLY_DEBUG")
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

	if scheduleType == "MONTHLY_DEBUG" {
		lockKeySuffix = time.Now().Format("2006-01-02-15:04") // Unique per minute
		lockTTL = 50 * time.Second                           // Expire just before next minute
	} else if scheduleType == "ANNUAL" {
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
	db, err := utils.GetDBConnection()
	if err != nil {
		log.Printf("[%s] Database connection failed: %v", scheduleType, err)
		return
	}
	// Note: Assuming utils.GetDBConnection handles its own pooling. If it returns a new connection, uncomment defer db.Close()
	// defer db.Close()

	conn, err := amqp.Dial(os.Getenv("QUEUE_URL"))
	if err != nil {
		log.Printf("[%s] RabbitMQ connection failed: %v", scheduleType, err)
		return
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		log.Printf("[%s] RabbitMQ channel creation failed: %v", scheduleType, err)
		return
	}
	defer ch.Close()

	// Put channel in Confirm Mode to ensure messages aren't lost
	if err := ch.Confirm(false); err != nil {
		log.Printf("[%s] Could not enable RabbitMQ confirms: %v", scheduleType, err)
		return
	}
	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))

	q, err := ch.QueueDeclare("billing_tasks", true, false, false, false, nil)
	if err != nil {
		log.Printf("[%s] RabbitMQ queue declaration failed: %v", scheduleType, err)
		return
	}

	// --- DATABASE QUERY ---
	// Map debug value to actual billing cycle for querying
	queryTerm := scheduleType
	if scheduleType == "MONTHLY_DEBUG" {
		queryTerm = "MONTHLY"
	}

	// JOIN workspaces to maintain the creator_id requirement for your workers
	query := `
		SELECT 
			s.id, 
			s.workspace_id, 
			w.creator_id, 
			s.current_plan_id, 
			s.scheduled_plan_id, 
			s.scheduled_effective_date, 
			s.provider_subscription_id
		FROM subscriptions s
		JOIN workspaces w ON s.workspace_id = w.id
		WHERE s.status = 'ACTIVE' AND s.billing_cycle = ?
	`

	rows, err := db.QueryContext(ctx, query, queryTerm)
	if err != nil {
		log.Printf("[%s] DB Query Error: %v", scheduleType, err)
		return
	}
	defer rows.Close()

	// --- DISTRIBUTION LOOP ---
	count := 0
	for rows.Next() {
		var subID, workspaceID, creatorID, currentPlanID int
		var scheduledPlanID sql.NullInt64
		var scheduledDate sql.NullTime
		var providerSubID sql.NullString

		// Scan the row using Go's safe Null handlers
		err := rows.Scan(
			&subID,
			&workspaceID,
			&creatorID,
			&currentPlanID,
			&scheduledPlanID,
			&scheduledDate,
			&providerSubID,
		)
		if err != nil {
			log.Printf("Row scan error: %v", err)
			continue
		}

		// DEDUPLICATION: Ensures no workspace is queued twice in the same cycle
		dedupeKey := fmt.Sprintf("queued:%s:%d:%s", scheduleType, workspaceID, lockKeySuffix)
		isNew, err := rdb.SetNX(ctx, dedupeKey, "true", 31*24*time.Hour).Result()
		if err != nil || !isNew {
			continue // Already queued, skip
		}

		// --- GRACEFUL UPGRADE LOGIC ---
		action := "renewal"
		planToBill := currentPlanID

		// Check if there is an upgrade scheduled AND the date has arrived
		if scheduledPlanID.Valid && scheduledDate.Valid {
			if !time.Now().Before(scheduledDate.Time) { // If Now >= Scheduled Date
				action = "upgrade"
				planToBill = int(scheduledPlanID.Int64)
			}
		}

		// --- BUILD PAYLOAD ---
		task := models.BillingTask{
			RunID:                  globalLockKey,
			BillingType:            queryTerm,
			WorkspaceID:            workspaceID,
			CreatorID:              creatorID,
			SubscriptionID:         subID,
			Action:                 action,
			PlanToBill:             planToBill,
			ProviderSubscriptionID: providerSubID.String, // Converts NullString to string (empty if null)
		}

		body, _ := json.Marshal(task)

		// --- PUBLISH TO QUEUE ---
		err = ch.PublishWithContext(ctx, "", q.Name, false, false, amqp.Publishing{
			DeliveryMode: amqp.Persistent, // Ensure messages survive RabbitMQ restarts
			ContentType:  "application/json",
			Body:         body,
		})

		if err != nil {
			rdb.Del(ctx, dedupeKey) // Failed to publish, delete dedupe key to allow retry
			log.Printf("Publish error for workspace %d: %v", workspaceID, err)
			continue
		}

		// Confirm receipt by RabbitMQ
		select {
		case confirmed := <-confirms:
			if !confirmed.Ack {
				rdb.Del(ctx, dedupeKey)
				log.Printf("RabbitMQ NACK for workspace %d", workspaceID)
			} else {
				count++
			}
		case <-time.After(5 * time.Second):
			rdb.Del(ctx, dedupeKey)
			log.Printf("Timeout waiting for RabbitMQ ACK for workspace %d", workspaceID)
		}
	}

	log.Printf("[%s] Distribution Finished. Total Queued: %d", scheduleType, count)
}