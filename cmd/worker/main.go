package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"lineblocs.com/scheduler/internal/activity"
	"lineblocs.com/scheduler/internal/billing"
	"lineblocs.com/scheduler/internal/cleanup"
	"lineblocs.com/scheduler/internal/config"
	"lineblocs.com/scheduler/internal/db"
	"lineblocs.com/scheduler/internal/email"
	"lineblocs.com/scheduler/internal/logger"
	"lineblocs.com/scheduler/internal/logs"
	"lineblocs.com/scheduler/internal/queue"
	"lineblocs.com/scheduler/repository"
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

	// RabbitMQ
	conn, err := queue.NewConnection(cfg)
	if err != nil {
		zapLog.Fatal("rabbitmq", zap.Error(err))
	}
	defer conn.Close()

	// Repositories
	wsRepo := repository.NewWorkspaceRepo(database)
	invRepo := repository.NewInvoiceRepo(database)
	debitRepo := repository.NewDebitRepo(database)
	recRepo := repository.NewRecordingRepo(database)
	payRepo := repository.NewPaymentRepo(database)
	subRepo := repository.NewSubscriptionRepo(database)

	// Services
	costCalc := billing.NewCostCalculator(wsRepo, debitRepo, recRepo, payRepo, zapLog)
	invoiceSvc := billing.NewInvoiceService(database, invRepo, zapLog)
	chargeSvc := billing.NewChargeService(zapLog)
	emailSvc := email.NewService(cfg)

	// Activity registry
	reg := activity.NewRegistry()

	reg.Register(billing.NewMonthlyActivity(database, costCalc, invoiceSvc, chargeSvc, wsRepo, subRepo, payRepo, zapLog))
	reg.Register(billing.NewAnnualActivity(database, costCalc, invoiceSvc, chargeSvc, wsRepo, subRepo, payRepo, zapLog))
	reg.Register(billing.NewRetryActivity(invRepo, wsRepo, payRepo, chargeSvc, zapLog))
	reg.Register(billing.NewProratedActivity(invoiceSvc, chargeSvc, wsRepo, subRepo, payRepo, zapLog))

	reg.Register(email.NewInactiveActivity(database, emailSvc, cfg, zapLog))
	reg.Register(email.NewCardExpiryActivity(database, emailSvc, cfg, zapLog))
	reg.Register(email.NewFreeTrialActivity(database, emailSvc, cfg, zapLog))
	reg.Register(email.NewSatisfactionActivity(database, emailSvc, cfg, zapLog))
	reg.Register(email.NewUsageTriggerActivity(database, emailSvc, zapLog))

	reg.Register(cleanup.NewStaleUsersActivity(database, cfg, zapLog))
	reg.Register(logs.NewPurgeActivity(database, cfg, zapLog))

	// Graceful shutdown context — cancelled on SIGINT/SIGTERM
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Health tracking — shared across consumers
	var healthy atomic.Bool
	healthy.Store(true)

	// Health endpoint for k8s/Docker liveness probes
	go serveHealth(&healthy, zapLog)

	// Start supervised consumers
	g, gCtx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return supervise(gCtx, "billing", func(ctx context.Context) error {
			ch, err := conn.Channel()
			if err != nil {
				return fmt.Errorf("open channel: %w", err)
			}
			defer ch.Close()
			return consumeQueue(ctx, ch, cfg.Rabbit.BillingTasksQueue, reg, zapLog)
		}, &healthy, zapLog)
	})

	g.Go(func() error {
		return supervise(gCtx, "recordings", func(ctx context.Context) error {
			ch, err := conn.Channel()
			if err != nil {
				return fmt.Errorf("open channel: %w", err)
			}
			defer ch.Close()
			return consumeQueue(ctx, ch, cfg.Rabbit.RecordingsTasksQueue, reg, zapLog)
		}, &healthy, zapLog)
	})

	zapLog.Info("worker started",
		zap.String("billing_queue", cfg.Rabbit.BillingTasksQueue),
		zap.String("recordings_queue", cfg.Rabbit.RecordingsTasksQueue))

	if err := g.Wait(); err != nil {
		zapLog.Error("worker exited with error", zap.Error(err))
	}

	zapLog.Info("worker stopped")
}

// supervise runs fn in a loop, restarting on failure with exponential backoff.
// It only gives up when the context is cancelled (shutdown signal).
func supervise(ctx context.Context, name string, fn func(ctx context.Context) error, healthy *atomic.Bool, log *zap.Logger) error {
	const maxBackoff = 60 * time.Second
	backoff := 1 * time.Second

	for {
		err := fn(ctx)

		// Clean shutdown — not an error
		if ctx.Err() != nil {
			return nil
		}

		// Consumer failed — log and backoff
		healthy.Store(false)
		log.Error("consumer crashed, restarting",
			zap.String("consumer", name),
			zap.Error(err),
			zap.Duration("backoff", backoff))

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}

		// Exponential backoff, capped
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}

		healthy.Store(true)
		log.Info("restarting consumer", zap.String("consumer", name))
	}
}

// serveHealth exposes GET /healthz for liveness probes.
func serveHealth(healthy *atomic.Bool, log *zap.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if healthy.Load() {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("unhealthy"))
		}
	})

	server := &http.Server{Addr: ":8086", Handler: mux}
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("health server failed", zap.Error(err))
	}
}

func consumeQueue(ctx context.Context, ch *amqp.Channel, queueName string, reg *activity.Registry, log *zap.Logger) error {
	consumer := queue.NewRabbitConsumer(ch)
	msgs, err := consumer.Consume(queueName, 1)
	if err != nil {
		return fmt.Errorf("consume %s: %w", queueName, err)
	}

	for {
		select {
		case <-ctx.Done():
			log.Info("consumer stopping", zap.String("queue", queueName))
			return nil
		case d, ok := <-msgs:
			if !ok {
				return fmt.Errorf("channel closed for queue %s", queueName)
			}
			processMessage(ctx, d, queueName, reg, log)
		}
	}
}

func processMessage(ctx context.Context, d amqp.Delivery, queueName string, reg *activity.Registry, log *zap.Logger) {
	activityName := resolveActivityName(queueName, d.Body)

	act, err := reg.Get(activityName)
	if err != nil {
		log.Error("unknown activity", zap.String("activity", activityName), zap.Error(err))
		d.Ack(false)
		return
	}

	_, err = activity.ExecuteWithRetry(ctx, act, d.Body, log)
	if err != nil {
		log.Error("activity failed",
			zap.String("activity", activityName),
			zap.Error(err))
		d.Nack(false, true)
		return
	}

	d.Ack(false)
}

func resolveActivityName(queueName string, body []byte) string {
	switch queueName {
	case "billing_tasks":
		var task struct {
			Action      string `json:"action"`
			BillingType string `json:"billing_type"`
		}
		if err := json.Unmarshal(body, &task); err == nil {
			if task.Action == "immediate" {
				return "billing.prorated"
			}
			if task.BillingType == "ANNUAL" {
				return "billing.annual"
			}
		}
		return "billing.monthly"

	case "recordings_tasks":
		return "recording.process"

	default:
		return queueName
	}
}
