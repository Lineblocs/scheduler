package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
	"strconv"

	helpers "github.com/Lineblocs/go-helpers"
	"lineblocs.com/scheduler/models"
	"lineblocs.com/scheduler/utils"

	_ "github.com/go-sql-driver/mysql"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"github.com/robfig/cron/v3"
)

// Global variables shared across goroutines
var (
	rdb             *redis.Client
	db              *sql.DB
	customizations *helpers.CustomizationSettingsKV
)

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

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("Critical: Could not connect to Redis: %v", err)
	}

	// 2. INITIALIZE DATABASE (Shared Pool)
	db, err = utils.GetDBConnection()
	if err != nil {
		log.Fatalf("Critical: Database connection failed: %v", err)
	}

	var err2 error
	customizations, err2 = helpers.GetCustomizationKVs()
	if err2 != nil {
		log.Fatalf("Critical: Could not load customizations: %v", err2)
	}

	billingFlow := utils.GetBillingFlow(customizations)
	c := cron.New()

	// 3. CONFIGURE CRON TASKS
	if billingFlow == "ANNIVERSARY" {
		log.Println("[INIT] Starting Distributor in ANNIVERSARY mode...")
		c.AddFunc("0 0 * * *", func() {
			log.Println("[PROD] Triggering Anniversary Billing Check...")
			runAnniversaryBillingDistributor("ANNIVERSARY")
		})
	} else {
		log.Println("[INIT] Starting Distributor in ANNUAL/MONTHLY mode...")
		c.AddFunc("0 0 1 * *", func() {
			log.Println("[PROD] Triggering Monthly Billing...")
			runBillingDistributor("MONTHLY")
		})

		c.AddFunc("0 0 1 1 *", func() {
			log.Println("[PROD] Triggering Yearly Billing...")
			runBillingDistributor("ANNUAL")
		})
	}



	c.AddFunc("0 * * * *", func() {
		log.Println("[PROD] Triggering Workspace Suspension Distributor...")
		runWorkspaceSuspensionsDistributor()
	})

	c.AddFunc("* * * * *", func() {
		log.Println("[PROD] Triggering Recordings Distribution...")
		runRecordingsDistributor()
	})

	c.AddFunc("0 0 * * *", func() {
		log.Println("[PROD] Triggering Plan Migrations Distributor...")
		runPlanMigrationsDistributor()
	})

	log.Printf("Scheduler started. Redis: %s", opt.Addr)
	c.Start()

	// 4. GRACEFUL SHUTDOWN LOGIC
	// Create a channel to listen for OS signals
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Wait for a signal (Blocks main goroutine)
	sig := <-stop
	log.Printf("Received signal: %v. Shutting down gracefully...", sig)

	// Cleanup resources
	c.Stop() // Stops the cron scheduler from starting new jobs
	if db != nil {
		log.Println("Closing Database pool...")
		db.Close()
	}
	if rdb != nil {
		log.Println("Closing Redis connection...")
		rdb.Close()
	}

	log.Println("Process exited.")
}

func runAnniversaryBillingDistributor(scheduleType string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	now := time.Now().UTC()
	lockKeySuffix := now.Format("2006-01-02")
	globalLockKey := fmt.Sprintf("anniversary_run_lock:%s", lockKeySuffix)

	locked, err := rdb.SetNX(ctx, globalLockKey, "running", 23*time.Hour).Result()
	if err != nil || !locked {
		log.Printf("[%s] Skip: Lock %s held by another instance.", scheduleType, globalLockKey)
		return
	}

	conn, err := amqp.Dial(os.Getenv("QUEUE_URL"))
	if err != nil {
		log.Printf("[%s] RabbitMQ connection failed: %v", scheduleType, err)
		return
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return
	}
	defer ch.Close()

	_ = ch.Confirm(false)
	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))
	q, _ := ch.QueueDeclare("billing_tasks", true, false, false, false, nil)

	query := `
        SELECT 
            s.id, s.workspace_id, w.creator_id, s.current_plan_id, 
            s.scheduled_plan_id, s.scheduled_effective_date, s.provider_subscription_id,
            s.billing_anchor_day, s.billing_cycle
        FROM subscriptions s
        JOIN workspaces w ON s.workspace_id = w.id
        WHERE s.status = 'ACTIVE' 
          AND (s.next_billing_date IS NULL OR DATE(s.next_billing_date) <= ?)`

	rows, err := db.QueryContext(ctx, query, now.Format("2006-01-02"))
	if err != nil {
		log.Printf("[%s] DB Query Error: %v", scheduleType, err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var subID, workspaceID, creatorID, currentPlanID int
		var anchorDay sql.NullInt64
		var cycle string
		var schedPlanID sql.NullInt64
		var schedDate sql.NullTime
		var provSubID sql.NullString

		if err := rows.Scan(&subID, &workspaceID, &creatorID, &currentPlanID, &schedPlanID, &schedDate, &provSubID, &anchorDay, &cycle); err != nil {
			continue
		}

		dedupeKey := fmt.Sprintf("queued:anniversary:%d:%s", workspaceID, lockKeySuffix)
		isNew, _ := rdb.SetNX(ctx, dedupeKey, "true", 24*time.Hour).Result()
		if !isNew {
			continue
		}

		action := "renewal"
		planToBill := currentPlanID
		if schedPlanID.Valid && schedDate.Valid && !now.Before(schedDate.Time) {
			action = "upgrade"
			planToBill = int(schedPlanID.Int64)
		}

		nextDate := utils.CalculateNextDate(now, cycle, int(anchorDay.Int64))

		task := models.BillingTask{
			RunID:                  globalLockKey,
			BillingType:            "ANNIVERSARY",
			WorkspaceID:            workspaceID,
			CreatorID:              creatorID,
			SubscriptionID:         subID,
			Action:                 action,
			PlanToBill:             planToBill,
			ProviderSubscriptionID: provSubID.String,
			NextBillingDate:        nextDate.Format("2006-01-02"),
		}

		body, _ := json.Marshal(task)
		err = ch.PublishWithContext(ctx, "", q.Name, false, false, amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			Body:         body,
		})

		if err != nil {
			rdb.Del(ctx, dedupeKey)
			continue
		}

		select {
		case c := <-confirms:
			if c.Ack {
				count++
			} else {
				rdb.Del(ctx, dedupeKey)
			}
		case <-time.After(5 * time.Second):
			rdb.Del(ctx, dedupeKey)
		}
	}
	log.Printf("[%s] Finished. Total Queued: %d", scheduleType, count)
}

func runBillingDistributor(scheduleType string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	now := time.Now().UTC()
	var lockKeySuffix string
	var nextDate time.Time
	if scheduleType == "ANNUAL" {
		lockKeySuffix = now.Format("2006")
		nextDate = now.AddDate(1, 0, 0)
	} else {
		lockKeySuffix = now.Format("2006-01")
		nextDate = now.AddDate(0, 1, 0)
	}

	globalLockKey := fmt.Sprintf("billing_run_lock:%s:%s", scheduleType, lockKeySuffix)
	locked, err := rdb.SetNX(ctx, globalLockKey, "running", 23*time.Hour).Result()
	if err != nil || !locked {
		log.Printf("[%s] Skip: Lock %s held by another instance.", scheduleType, globalLockKey)
		return
	}

	conn, err := amqp.Dial(os.Getenv("QUEUE_URL"))
	if err != nil {
		return
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return
	}
	defer ch.Close()

	_ = ch.Confirm(false)
	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))
	q, _ := ch.QueueDeclare("billing_tasks", true, false, false, false, nil)

	query := `
        SELECT 
            s.id, s.workspace_id, w.creator_id, s.current_plan_id, 
            s.scheduled_plan_id, s.scheduled_effective_date, s.provider_subscription_id
        FROM subscriptions s
        JOIN workspaces w ON s.workspace_id = w.id
        WHERE s.status = 'ACTIVE' 
          AND s.billing_cycle = ? 
          AND (s.next_billing_date IS NULL OR DATE(s.next_billing_date) <= ?)`

	rows, err := db.QueryContext(ctx, query, scheduleType, now.Format("2006-01-02"))
	if err != nil {
		log.Printf("[%s] DB Query Error: %v", scheduleType, err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var subID, workspaceID, creatorID, currentPlanID int
		var schedPlanID sql.NullInt64
		var schedDate sql.NullTime
		var provSubID sql.NullString

		if err := rows.Scan(&subID, &workspaceID, &creatorID, &currentPlanID, &schedPlanID, &schedDate, &provSubID); err != nil {
			continue
		}

		dedupeKey := fmt.Sprintf("queued:%s:%d:%s", scheduleType, workspaceID, lockKeySuffix)
		isNew, _ := rdb.SetNX(ctx, dedupeKey, "true", 31*24*time.Hour).Result()
		if !isNew {
			continue
		}

		action := "BILLING_RENEWAL"
		planToBill := currentPlanID
		if schedPlanID.Valid && schedDate.Valid && !now.Before(schedDate.Time) {
			action = "BILLING_UPGRADE"
			planToBill = int(schedPlanID.Int64)
		}

		task := models.BillingTask{
			RunID:                  globalLockKey,
			BillingType:            scheduleType,
			WorkspaceID:            workspaceID,
			CreatorID:              creatorID,
			SubscriptionID:         subID,
			Action:                 action,
			PlanToBill:             planToBill,
			ProviderSubscriptionID: provSubID.String,
			NextBillingDate:        nextDate.Format("2006-01-02"),
		}

		body, _ := json.Marshal(task)
		err = ch.PublishWithContext(ctx, "", q.Name, false, false, amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			Body:         body,
		})

		if err != nil {
			rdb.Del(ctx, dedupeKey)
			continue
		}

		select {
		case c := <-confirms:
			if c.Ack {
				count++
			} else {
				rdb.Del(ctx, dedupeKey)
			}
		case <-time.After(5 * time.Second):
			rdb.Del(ctx, dedupeKey)
		}
	}
	log.Printf("[%s] Finished. Total Queued: %d", scheduleType, count)
}

func runRecordingsDistributor() {
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()

	conn, err := amqp.Dial(os.Getenv("QUEUE_URL"))
	if err != nil {
		log.Printf("[RECORDINGS] MQ connection failed: %v", err)
		return
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return
	}
	defer ch.Close()

	_ = ch.Confirm(false)
	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))

	query := `
        SELECT id, status, storage_id, storage_server_ip, trim 
        FROM recordings 
        WHERE (status = 'COMPLETED' OR status = 'FAILED') 
          AND relocation_attempts <= 3`

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		log.Printf("[RECORDINGS] Query error: %v", err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var rID int
		var rStatus, sID, sIP string
		var trim sql.NullString

		if err := rows.Scan(&rID, &rStatus, &sID, &sIP, &trim); err != nil {
			continue
		}

		dedupeKey := fmt.Sprintf("queued:recording:%d", rID)
		if isNew, err := rdb.SetNX(ctx, dedupeKey, "true", 30*time.Minute).Result(); err != nil || !isNew {
			continue
		}

		task := models.RecordingTask{
			ID:              rID,
			Status:          rStatus,
			StorageID:       sID,
			StorageServerIP: sIP,
			Trim:            trim.String,
		}

		body, _ := json.Marshal(task)
		err = ch.PublishWithContext(ctx, "", "recording_tasks", false, false, amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			Body:         body,
		})

		if err != nil {
			rdb.Del(ctx, dedupeKey)
			log.Printf("[RECORDINGS] Publish error: %v", err)
			continue
		}

		select {
		case c := <-confirms:
			if c.Ack {
				count++
			} else {
				rdb.Del(ctx, dedupeKey)
			}
		case <-time.After(2 * time.Second):
			rdb.Del(ctx, dedupeKey)
			log.Printf("[RECORDINGS] Timeout waiting for ACK on ID %d", rID)
		}
	}
	log.Printf("[RECORDINGS] Finished. Total Queued: %d", count)
}

func runWorkspaceSuspensionsDistributor() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	now := time.Now().UTC()
	lockKeySuffix := now.Format("2006-01-02")
	globalLockKey := fmt.Sprintf("workspace_suspensions_run_lock:%s", lockKeySuffix)

	locked, err := rdb.SetNX(ctx, globalLockKey, "running", 23*time.Hour).Result()
	if err != nil || !locked {
		log.Printf("[SUSPENSIONS] Skip: Lock %s held by another instance.", globalLockKey)
		return
	}

	conn, err := amqp.Dial(os.Getenv("QUEUE_URL"))
	if err != nil {
		log.Printf("[SUSPENSIONS] RabbitMQ connection failed: %v", err)
		return
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return
	}
	defer ch.Close()

	_ = ch.Confirm(false)
	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))

	query := `
		SELECT
			w.id,
			i.id
		FROM workspaces w
		JOIN users_invoices i ON i.workspace_id = w.id
		WHERE i.status = 'FAILED' 
		  AND i.due_date <= ?
		GROUP BY w.id, i.id
		ORDER BY w.id`

	rows, err := db.QueryContext(ctx, query, now.Format("2006-01-02"))
	if err != nil {
		log.Printf("[SUSPENSIONS] DB Query Error: %v", err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var workspaceID int
		var invoiceID int
		if err := rows.Scan(&workspaceID, &invoiceID); err != nil {
			continue
		}

		dedupeKey := fmt.Sprintf("queued:suspension:%d:%s", workspaceID, lockKeySuffix)
		isNew, _ := rdb.SetNX(ctx, dedupeKey, "true", 24*time.Hour).Result()
		if !isNew {
			continue
		}

		// Idempotency check: verify no active suspension already exists for this invoice
		checkQuery := `SELECT COUNT(*) as count, COALESCE(MIN(id), 0)
			FROM workspaces_suspensions
			WHERE invoice_id = ?`
		var suspensionExists int
		var suspensionID int
		isFollowUp := false
		err := db.QueryRowContext(ctx, checkQuery, invoiceID).Scan(&suspensionExists, &suspensionID)
		if err != nil {
			rdb.Del(ctx, dedupeKey)
			continue
		}
		if suspensionExists > 0 {
			log.Printf("[SUSPENSIONS] Workspace %d already has active suspension (id=%d), dispatching as follow-up", workspaceID, suspensionID)
			isFollowUp = true
		}

		gracePeriodStr := utils.GetGracePeriod(customizations)
		var gracePeriod *int
		if gracePeriodStr != "" {
			val, err := strconv.Atoi(gracePeriodStr)
			if err == nil {
				gracePeriod = &val
			}
		}


		task := models.SuspensionTask{
			WorkspaceID: workspaceID,
			Status: 	"PENDING",
			Reason:      "Failed invoice payment",
			GracePeriodExtension: gracePeriod,
			SuspensionInitiatedAt: now,
			IsFollowUp: isFollowUp,
		}
		body, _ := json.Marshal(task)

		err = ch.PublishWithContext(ctx, "", "workspace_suspensions_tasks", false, false, amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			Body:         body,
		})

		if err != nil {
			rdb.Del(ctx, dedupeKey)
			log.Printf("[SUSPENSIONS] Publish error: %v", err)
			continue
		}

		select {
		case c := <-confirms:
			if c.Ack {
				count++
			} else {
				rdb.Del(ctx, dedupeKey)
			}
		case <-time.After(2 * time.Second):
			rdb.Del(ctx, dedupeKey)
			log.Printf("[SUSPENSIONS] Timeout waiting for ACK on workspaceID %d", workspaceID)
		}
	}
	log.Printf("[SUSPENSIONS] Finished. Total Queued: %d", count)
}

func runPlanMigrationsDistributor() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	now := time.Now().UTC()
	lockKeySuffix := now.Format("2006-01-02")
	globalLockKey := fmt.Sprintf("plan_migrations_run_lock:%s", lockKeySuffix)

	locked, err := rdb.SetNX(ctx, globalLockKey, "running", 23*time.Hour).Result()
	if err != nil || !locked {
		log.Printf("[MIGRATIONS] Skip: Lock %s held by another instance.", globalLockKey)
		return
	}

	conn, err := amqp.Dial(os.Getenv("QUEUE_URL"))
	if err != nil {
		log.Printf("[MIGRATIONS] RabbitMQ connection failed: %v", err)
		return
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		return
	}
	defer ch.Close()

	_ = ch.Confirm(false)
	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))

	query := `
        SELECT
            s.id, s.workspace_id, s.scheduled_plan_id
        FROM subscriptions s
        WHERE s.status = 'ACTIVE'
          AND s.scheduled_plan_id IS NOT NULL
          AND s.scheduled_effective_date IS NOT NULL
          AND DATE(s.scheduled_effective_date) <= ?`

	rows, err := db.QueryContext(ctx, query, now.Format("2006-01-02"))
	if err != nil {
		log.Printf("[MIGRATIONS] DB Query Error: %v", err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var subID, workspaceID, scheduledPlanID int

		if err := rows.Scan(&subID, &workspaceID, &scheduledPlanID); err != nil {
			continue
		}

		dedupeKey := fmt.Sprintf("queued:migration:%d:%s", workspaceID, lockKeySuffix)
		isNew, _ := rdb.SetNX(ctx, dedupeKey, "true", 24*time.Hour).Result()
		if !isNew {
			continue
		}

		// Update database directly initially, maybe best to move to worker for retry later
		updateQuery := `
			UPDATE subscriptions
			SET current_plan_id = scheduled_plan_id, 
			scheduled_plan_id = NULL,
			scheduled_effective_date = NULL
			WHERE id = ?`
		_, err = db.ExecContext(ctx, updateQuery, subID)
		if err != nil {
			log.Printf("[MIGRATIONS] Failed to update subscription %d: %v", subID, err)
			rdb.Del(ctx, dedupeKey)
			continue
		}

		// Dispatch RabbitMQ event
		type PlanUpgradeEvent struct {
			WorkspaceID int    `json:"workspace_id"`
			NewPlanID   int    `json:"new_plan_id"`
			Status      string `json:"status"`
			Type        string `json:"type"`
		}

		event := PlanUpgradeEvent{
			WorkspaceID: workspaceID,
			NewPlanID:   scheduledPlanID,
			Status:      "SUCCESS",
			Type:        "PLAN_UPGRADE",
		}

		body, _ := json.Marshal(event)
		err = ch.PublishWithContext(ctx, "", "workspaces", false, false, amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			Body:         body,
		})

		if err != nil {
			log.Printf("[MIGRATIONS] Publish error: %v", err)
			continue
		}

		select {
		case c := <-confirms:
			if c.Ack {
				count++
			}
		case <-time.After(5 * time.Second):
			log.Printf("[MIGRATIONS] Timeout waiting for ACK on workspace %d", workspaceID)
		}
	}
	log.Printf("[MIGRATIONS] Finished. Total Migrated: %d", count)
}

