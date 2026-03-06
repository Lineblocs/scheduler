package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os/signal"
	"syscall"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
	"lineblocs.com/scheduler/internal/config"
	"lineblocs.com/scheduler/internal/db"
	"lineblocs.com/scheduler/internal/lock"
	"lineblocs.com/scheduler/internal/logger"
	"lineblocs.com/scheduler/internal/queue"
	"lineblocs.com/scheduler/models"

	"github.com/jmoiron/sqlx"
	"github.com/robfig/cron/v3"
)

func main() {
	// Config
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// Logger
	zapLog, err := logger.New(cfg)
	if err != nil {
		log.Fatalf("logger: %v", err)
	}
	defer zapLog.Sync()

	// Database
	database, err := db.New(cfg)
	if err != nil {
		zapLog.Fatal("database", zap.Error(err))
	}
	defer database.Close()

	// Redis
	redisClient, err := lock.NewClient(cfg)
	if err != nil {
		zapLog.Fatal("redis", zap.Error(err))
	}
	defer redisClient.Close()

	// RabbitMQ (persistent connection, reused across cron ticks)
	conn, err := queue.NewConnection(cfg)
	if err != nil {
		zapLog.Fatal("rabbitmq", zap.Error(err))
	}
	defer conn.Close()

	d := &distributor{
		cfg:   cfg,
		db:    database,
		redis: redisClient,
		conn:  conn,
		log:   zapLog,
	}

	// Setup cron
	c := cron.New()

	c.AddFunc(cfg.Distributor.MonthlyCron, func() {
		d.runBilling("MONTHLY")
	})

	c.AddFunc(cfg.Distributor.AnnualCron, func() {
		d.runBilling("ANNUAL")
	})

	if cfg.Distributor.Debug {
		c.AddFunc("* * * * *", func() {
			d.runBilling("MONTHLY_DEBUG")
		})
	}

	c.AddFunc(cfg.Distributor.RecordingsCron, func() {
		d.runRecordings()
	})

	c.Start()
	zapLog.Info("distributor started")

	// Graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	zapLog.Info("stopping distributor")
	<-c.Stop().Done()
	zapLog.Info("distributor stopped")
}

type distributor struct {
	cfg   *config.Config
	db    *sqlx.DB
	redis *lock.Client
	conn  *queue.Connection
	log   *zap.Logger
}

func (d *distributor) runBilling(scheduleType string) {
	ctx, cancel := context.WithTimeout(context.Background(), d.cfg.Distributor.BillingTimeout)
	defer cancel()

	// Lock
	var lockKeySuffix string
	var lockTTL time.Duration

	switch scheduleType {
	case "MONTHLY_DEBUG":
		lockKeySuffix = time.Now().Format("2006-01-02-15:04")
		lockTTL = d.cfg.Distributor.DebugLockTTL
	case "ANNUAL":
		lockKeySuffix = time.Now().Format("2006")
		lockTTL = d.cfg.Billing.LockTTL
	default:
		lockKeySuffix = time.Now().Format("2006-01")
		lockTTL = d.cfg.Billing.LockTTL
	}

	globalLockKey := fmt.Sprintf("billing_run_lock:%s:%s", scheduleType, lockKeySuffix)

	locked, err := d.redis.Acquire(ctx, globalLockKey, lockTTL)
	if err != nil || !locked {
		d.log.Debug("billing lock held", zap.String("key", globalLockKey))
		return
	}

	// RabbitMQ channel (fresh per run, closed after)
	ch, err := d.conn.Channel()
	if err != nil {
		d.log.Error("rabbitmq channel", zap.Error(err))
		return
	}
	defer ch.Close()

	if err := ch.Confirm(false); err != nil {
		d.log.Error("rabbitmq confirm mode", zap.Error(err))
		return
	}
	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))

	q, err := ch.QueueDeclare(d.cfg.Rabbit.BillingTasksQueue, true, false, false, false, nil)
	if err != nil {
		d.log.Error("queue declare", zap.Error(err))
		return
	}

	// Query
	queryTerm := scheduleType
	if scheduleType == "MONTHLY_DEBUG" {
		queryTerm = "MONTHLY"
	}

	rows, err := d.db.QueryContext(ctx, `
		SELECT s.id, s.workspace_id, w.creator_id, s.current_plan_id,
		       s.scheduled_plan_id, s.scheduled_effective_date, s.provider_subscription_id
		FROM subscriptions s
		JOIN workspaces w ON s.workspace_id = w.id
		WHERE s.status = 'ACTIVE'
		  AND s.billing_cycle = ?
		  AND (s.next_billing_date IS NULL OR s.next_billing_date <= NOW())
	`, queryTerm)
	if err != nil {
		d.log.Error("billing query", zap.String("type", scheduleType), zap.Error(err))
		return
	}
	defer rows.Close()

	// Distribute
	count := 0
	for rows.Next() {
		var subID, workspaceID, creatorID, currentPlanID int
		var scheduledPlanID sql.NullInt64
		var scheduledDate sql.NullTime
		var providerSubID sql.NullString

		if err := rows.Scan(&subID, &workspaceID, &creatorID, &currentPlanID, &scheduledPlanID, &scheduledDate, &providerSubID); err != nil {
			d.log.Warn("row scan", zap.Error(err))
			continue
		}

		dedupeKey := fmt.Sprintf("queued:%s:%d:%s", scheduleType, workspaceID, lockKeySuffix)
		isNew, _ := d.redis.SetDedupeKey(ctx, dedupeKey, d.cfg.Distributor.DedupTTL)
		if !isNew {
			continue
		}

		action := "renewal"
		planToBill := currentPlanID

		if scheduledPlanID.Valid && scheduledDate.Valid && !time.Now().Before(scheduledDate.Time) {
			action = "upgrade"
			planToBill = int(scheduledPlanID.Int64)
		}

		task := models.BillingTask{
			RunID:                  globalLockKey,
			BillingType:            queryTerm,
			WorkspaceID:            workspaceID,
			CreatorID:              creatorID,
			SubscriptionID:         subID,
			Action:                 action,
			PlanToBill:             planToBill,
			ProviderSubscriptionID: providerSubID.String,
		}

		body, _ := json.Marshal(task)

		if err := ch.PublishWithContext(ctx, "", q.Name, false, false, amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			Body:         body,
		}); err != nil {
			d.redis.RDB.Del(ctx, dedupeKey)
			continue
		}

		select {
		case confirmed := <-confirms:
			if confirmed.Ack {
				count++
			} else {
				d.redis.RDB.Del(ctx, dedupeKey)
			}
		case <-time.After(d.cfg.Distributor.PublishConfirmTimeout):
			d.redis.RDB.Del(ctx, dedupeKey)
		}
	}

	d.log.Info("billing distribution complete",
		zap.String("type", scheduleType),
		zap.Int("queued", count))
}

func (d *distributor) runRecordings() {
	ctx, cancel := context.WithTimeout(context.Background(), d.cfg.Distributor.RecordingsTimeout)
	defer cancel()

	// Lock
	lockKeySuffix := time.Now().Format("2006-01-02-15:04")
	globalLockKey := fmt.Sprintf("recordings_run_lock:%s", lockKeySuffix)

	locked, err := d.redis.Acquire(ctx, globalLockKey, d.cfg.Recordings.LockTTL)
	if err != nil || !locked {
		d.log.Debug("recordings lock held", zap.String("key", globalLockKey))
		return
	}

	// RabbitMQ channel
	ch, err := d.conn.Channel()
	if err != nil {
		d.log.Error("rabbitmq channel", zap.Error(err))
		return
	}
	defer ch.Close()

	if err := ch.Confirm(false); err != nil {
		d.log.Error("rabbitmq confirm mode", zap.Error(err))
		return
	}
	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))

	q, err := ch.QueueDeclare(d.cfg.Rabbit.RecordingsTasksQueue, true, false, false, false, nil)
	if err != nil {
		d.log.Error("queue declare", zap.Error(err))
		return
	}

	// Query
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, status, storage_id, storage_server_ip, trim FROM recordings WHERE status = ?",
		d.cfg.Recordings.CompletedStatus)
	if err != nil {
		d.log.Error("recordings query", zap.Error(err))
		return
	}
	defer rows.Close()

	// Distribute
	count := 0
	for rows.Next() {
		var recordingID int
		var storageID, recordingStatus, storageServerIP string
		var trim sql.NullString

		if err := rows.Scan(&recordingID, &recordingStatus, &storageID, &storageServerIP, &trim); err != nil {
			d.log.Warn("row scan", zap.Error(err))
			continue
		}

		dedupeKey := fmt.Sprintf("queued:recording:%d:%s", recordingID, lockKeySuffix)
		isNew, err := d.redis.SetDedupeKey(ctx, dedupeKey, d.cfg.Distributor.DedupTTL)
		if err != nil || !isNew {
			continue
		}

		task := models.RecordingTask{
			ID:              recordingID,
			Status:          recordingStatus,
			StorageID:       storageID,
			StorageServerIP: storageServerIP,
			Trim:            trim.String,
		}

		body, _ := json.Marshal(task)

		if err := ch.PublishWithContext(ctx, "", q.Name, false, false, amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			Body:         body,
		}); err != nil {
			d.redis.RDB.Del(ctx, dedupeKey)
			d.log.Warn("publish error", zap.Int("recording_id", recordingID), zap.Error(err))
			continue
		}

		select {
		case confirmed := <-confirms:
			if confirmed.Ack {
				count++
			} else {
				d.redis.RDB.Del(ctx, dedupeKey)
				d.log.Warn("rabbitmq nack", zap.Int("recording_id", recordingID))
			}
		case <-time.After(d.cfg.Distributor.PublishConfirmTimeout):
			d.redis.RDB.Del(ctx, dedupeKey)
			d.log.Warn("publish confirm timeout", zap.Int("recording_id", recordingID))
		}
	}

	d.log.Info("recordings distribution complete", zap.Int("queued", count))
}
