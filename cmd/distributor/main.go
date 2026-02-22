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
	"lineblocs.com/scheduler/models"
	"lineblocs.com/scheduler/utils"

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

	// PRODUCTION: Recordings Distribution (Every 5 minutes)
	_, _ = c.AddFunc("*/5 * * * *", func() {
		log.Println("[PROD] Triggering Recordings Distribution...")
		runRecordingsDistributor()
	})

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



	log.Printf("[%s] Distribution Finished. Total Billing Queued: %d", scheduleType, count)
}

func runRecordingsDistributor() {
	// 1-hour safety timeout for the entire process
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Hour)
	defer cancel()

	// --- GLOBAL LOCK LOGIC ---
	lockKeySuffix := time.Now().Format("2006-01-02-15:04") // Unique per minute
	lockTTL := 4 * time.Minute                            // Expire before next 5-minute interval
	globalLockKey := fmt.Sprintf("recordings_run_lock:%s", lockKeySuffix)

	// SET NX: Only one instance/replica will succeed here
	locked, err := rdb.SetNX(ctx, globalLockKey, "running", lockTTL).Result()
	if err != nil || !locked {
		log.Printf("[RECORDINGS] Skip: Lock %s held by another instance.", globalLockKey)
		return
	}

	log.Printf("[RECORDINGS] Lock Acquired. Processing recordings distribution...")

	// --- CONNECTIONS ---
	db, err := utils.GetDBConnection()
	if err != nil {
		log.Printf("[RECORDINGS] Database connection failed: %v", err)
		return
	}

	conn, err := amqp.Dial(os.Getenv("QUEUE_URL"))
	if err != nil {
		log.Printf("[RECORDINGS] RabbitMQ connection failed: %v", err)
		return
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		log.Printf("[RECORDINGS] RabbitMQ channel creation failed: %v", err)
		return
	}
	defer ch.Close()

	// Put channel in Confirm Mode to ensure messages aren't lost
	if err := ch.Confirm(false); err != nil {
		log.Printf("[RECORDINGS] Could not enable RabbitMQ confirms: %v", err)
		return
	}
	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))

	qRecordings, err := ch.QueueDeclare("recordings_tasks", true, false, false, false, nil)
	if err != nil {
		log.Printf("[RECORDINGS] RabbitMQ recordings queue declaration failed: %v", err)
		return
	}

	// --- DATABASE QUERY ---
	status := "completed"
	recordingsResults, err := db.QueryContext(ctx, "SELECT id, status, storage_id, storage_server_ip, trim FROM recordings WHERE status = ?", status)
	if err != nil {
		log.Printf("[RECORDINGS] DB Query Error: %v", err)
		return
	}
	defer recordingsResults.Close()

	// --- DISTRIBUTION LOOP ---
	recordingsCount := 0
	for recordingsResults.Next() {
		var recordingID, storageID int
		var recordingStatus, storageServerIP string
		var trim sql.NullString

		err := recordingsResults.Scan(
			&recordingID,
			&recordingStatus,
			&storageID,
			&storageServerIP,
			&trim,
		)
		if err != nil {
			log.Printf("[RECORDINGS] Row scan error: %v", err)
			continue
		}

		// DEDUPLICATION: Ensures no recording is queued twice
		recordingsDedupeKey := fmt.Sprintf("queued:recording:%d:%s", recordingID, lockKeySuffix)
		isNew, err := rdb.SetNX(ctx, recordingsDedupeKey, "true", 31*24*time.Hour).Result()
		if err != nil || !isNew {
			continue // Already queued, skip
		}

		// --- BUILD RECORDINGS PAYLOAD ---
		recordingTask := models.RecordingTask{
			ID:              recordingID,
			Status:          recordingStatus,
			StorageID:       storageID,
			StorageServerIP: storageServerIP,
			Trim:            trim.String,
		}

		body, _ := json.Marshal(recordingTask)

		// --- PUBLISH TO RECORDINGS QUEUE ---
		err = ch.PublishWithContext(ctx, "", qRecordings.Name, false, false, amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			Body:         body,
		})

		if err != nil {
			rdb.Del(ctx, recordingsDedupeKey)
			log.Printf("[RECORDINGS] Publish error for ID %d: %v", recordingID, err)
			continue
		}

		// Confirm receipt by RabbitMQ
		select {
		case confirmed := <-confirms:
			if !confirmed.Ack {
				rdb.Del(ctx, recordingsDedupeKey)
				log.Printf("[RECORDINGS] RabbitMQ NACK for recording %d", recordingID)
			} else {
				recordingsCount++
			}
		case <-time.After(5 * time.Second):
			rdb.Del(ctx, recordingsDedupeKey)
			log.Printf("[RECORDINGS] Timeout waiting for RabbitMQ ACK for recording %d", recordingID)
		}
	}

	log.Printf("[RECORDINGS] Distribution Finished. Total Recordings Queued: %d", recordingsCount)
}