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

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("Critical: Could not connect to Redis: %v", err)
	}

	customizations, err := helpers.GetCustomizationKVs()
	if err != nil {
		log.Fatalf("Critical: Could not load customizations: %v", err)
	}

	billingFlow := utils.GetBillingFlow(customizations)

	c := cron.New()

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

	c.AddFunc("*/5 * * * *", func() {
		log.Println("[PROD] Triggering Recordings Distribution...")
		runRecordingsDistributor()
	})

	log.Printf("Billing Task Distributor started. Redis at: %s", opt.Addr)
	c.Start()

	select {}
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

	db, err := utils.GetDBConnection()
	if err != nil {
		log.Printf("[%s] Database connection failed: %v", scheduleType, err)
		return
	}
	defer db.Close()

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

		// Calculate intent for the next date
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

	db, err := utils.GetDBConnection()
	if err != nil {
		log.Printf("[%s] Database connection failed: %v", scheduleType, err)
		return
	}
	defer db.Close()

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

		action := "renewal"
		planToBill := currentPlanID
		if schedPlanID.Valid && schedDate.Valid && !now.Before(schedDate.Time) {
			action = "upgrade"
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
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Hour)
	defer cancel()

	lockKeySuffix := time.Now().Format("2006-01-02-15:04")
	globalLockKey := fmt.Sprintf("recordings_run_lock:%s", lockKeySuffix)

	locked, err := rdb.SetNX(ctx, globalLockKey, "running", 4*time.Minute).Result()
	if err != nil || !locked {
		return
	}

	db, err := utils.GetDBConnection()
	if err != nil {
		return
	}
	defer db.Close()

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
	q, _ := ch.QueueDeclare("recording_tasks", true, false, false, false, nil)

	rows, err := db.QueryContext(ctx, "SELECT id, status, storage_id, storage_server_ip, trim FROM recordings WHERE status = 'completed' AND relocation_attempts <= 3")
	if err != nil {
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

		dedupeKey := fmt.Sprintf("queued:recording:%d:%s", rID, lockKeySuffix)
		if isNew, err := rdb.SetNX(ctx, dedupeKey, "true", 24*time.Hour).Result(); err != nil || !isNew {
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
	log.Printf("[RECORDINGS] Finished. Queued: %d", count)
}