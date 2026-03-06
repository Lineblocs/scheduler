package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

// DatabaseConfig holds database connection settings.
type DatabaseConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	Name     string
	MaxOpen  int
	MaxIdle  int
	Lifetime time.Duration
}

// DSN returns the MySQL data source name.
func (d DatabaseConfig) DSN() string {
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true",
		d.User, d.Password, d.Host, d.Port, d.Name)
}

// RabbitConfig holds RabbitMQ connection and queue settings.
type RabbitConfig struct {
	URL                  string
	BillingTasksQueue    string
	RecordingsTasksQueue string
	FailedPaymentsQueue  string
	PaymentReceiptsQueue string
}

// RedisConfig holds Redis connection settings.
type RedisConfig struct {
	URL string
}

// ARIConfig holds Asterisk ARI connection settings.
type ARIConfig struct {
	URL          string
	Username     string
	Password     string
	RecordingApp string
	WSURL        string
}

// BillingConfig holds billing-related settings.
type BillingConfig struct {
	RetryAttempts int
	LockTTL       time.Duration
}

// RecordingsConfig holds recording processing settings.
type RecordingsConfig struct {
	LockTTL         time.Duration
	Interval        time.Duration
	CompletedStatus string
}

// EmailConfig holds email notification thresholds.
type EmailConfig struct {
	FreeTrialReminderDays  int
	InactivityReminderDays int
	SatisfactionSurveyDays int
	CardExpiryWarningMonths int
}

// DistributorConfig holds cron distributor settings.
type DistributorConfig struct {
	Debug               bool
	BillingTimeout      time.Duration
	RecordingsTimeout   time.Duration
	DebugLockTTL        time.Duration
	DedupTTL            time.Duration
	PublishConfirmTimeout time.Duration
	MonthlyCron         string
	AnnualCron          string
	RecordingsCron      string
}

// Config holds all application configuration loaded from environment variables.
type Config struct {
	Database    DatabaseConfig
	Rabbit      RabbitConfig
	Redis       RedisConfig
	ARI         ARIConfig
	Billing     BillingConfig
	Recordings  RecordingsConfig
	Email       EmailConfig
	Distributor DistributorConfig

	// API
	APIURL       string
	APIKey       string
	DeployDomain string

	// Logging
	LogDestinations string
	LogLevel        string

	// Misc
	LogRetentionDays    int
	CleanupPasswordDays int
}

// Load reads configuration from environment variables with sensible defaults.
func Load() (*Config, error) {
	if os.Getenv("USE_DOTENV") != "off" {
		_ = godotenv.Load(".env")
	}

	cfg := &Config{
		Database: DatabaseConfig{
			Host:     envOrDefault("DB_HOST", "localhost"),
			Port:     envIntOrDefault("DB_PORT", 3306),
			User:     envOrDefault("DB_USER", "root"),
			Password: envOrDefault("DB_PASS", ""),
			Name:     envOrDefault("DB_NAME", "lineblocs"),
			MaxOpen:  envIntOrDefault("DB_MAX_OPEN", 25),
			MaxIdle:  envIntOrDefault("DB_MAX_IDLE", 5),
			Lifetime: envDurationOrDefault("DB_LIFETIME", 5*time.Minute),
		},

		Rabbit: RabbitConfig{
			URL:                  envOrDefault("QUEUE_URL", ""),
			BillingTasksQueue:    envOrDefault("BILLING_TASKS_QUEUE", "billing_tasks"),
			RecordingsTasksQueue: envOrDefault("RECORDINGS_TASKS_QUEUE", "recordings_tasks"),
			FailedPaymentsQueue:  envOrDefault("FAILED_PAYMENTS_QUEUE", "failed_payments"),
			PaymentReceiptsQueue: envOrDefault("PAYMENT_RECEIPTS_QUEUE", "payment_receipts"),
		},

		Redis: RedisConfig{
			URL: envOrDefault("REDIS_URL", ""),
		},

		ARI: ARIConfig{
			URL:          envOrDefault("ARI_URL", ""),
			Username:     envOrDefault("ARI_USERNAME", ""),
			Password:     envOrDefault("ARI_PASSWORD", ""),
			RecordingApp: envOrDefault("ARI_RECORDING_APP", ""),
			WSURL:        envOrDefault("ARI_WSURL", ""),
		},

		Billing: BillingConfig{
			RetryAttempts: envIntOrDefault("BILLING_RETRY_ATTEMPTS", 0),
			LockTTL:       envDurationOrDefault("BILLING_LOCK_TTL", 23*time.Hour),
		},

		Recordings: RecordingsConfig{
			LockTTL:         envDurationOrDefault("RECORDINGS_LOCK_TTL", 4*time.Minute),
			Interval:        envDurationOrDefault("RECORDINGS_INTERVAL", 5*time.Minute),
			CompletedStatus: envOrDefault("RECORDING_COMPLETED_STATUS", "completed"),
		},

		Email: EmailConfig{
			FreeTrialReminderDays:   envIntOrDefault("FREE_TRIAL_REMINDER_DAYS", 7),
			InactivityReminderDays:  envIntOrDefault("INACTIVITY_REMINDER_DAYS", 14),
			SatisfactionSurveyDays:  envIntOrDefault("SATISFACTION_SURVEY_DAYS", 7),
			CardExpiryWarningMonths: envIntOrDefault("CARD_EXPIRY_WARNING_MONTHS", 1),
		},

		Distributor: DistributorConfig{
			Debug:                 os.Getenv("DISTRIBUTOR_DEBUG") == "1",
			BillingTimeout:        envDurationOrDefault("BILLING_DISTRIBUTOR_TIMEOUT", 2*time.Hour),
			RecordingsTimeout:     envDurationOrDefault("RECORDINGS_DISTRIBUTOR_TIMEOUT", 1*time.Hour),
			DebugLockTTL:          envDurationOrDefault("DEBUG_LOCK_TTL", 50*time.Second),
			DedupTTL:              envDurationOrDefault("DEDUP_TTL", 31*24*time.Hour),
			PublishConfirmTimeout: envDurationOrDefault("PUBLISH_CONFIRM_TIMEOUT", 5*time.Second),
			MonthlyCron:           envOrDefault("MONTHLY_CRON", "0 0 1 * *"),
			AnnualCron:            envOrDefault("ANNUAL_CRON", "0 0 1 1 *"),
			RecordingsCron:        envOrDefault("RECORDINGS_CRON", "*/5 * * * *"),
		},

		// API
		APIURL:       envOrDefault("API_URL", ""),
		APIKey:       envOrDefault("LINEBLOCS_KEY", ""),
		DeployDomain: envOrDefault("DEPLOYMENT_DOMAIN", ""),

		// Logging
		LogDestinations: envOrDefault("LOG_DESTINATIONS", ""),
		LogLevel:        envOrDefault("LOG_LEVEL", "info"),

		// Misc
		LogRetentionDays:    envIntOrDefault("LOG_RETENTION_DAYS", 7),
		CleanupPasswordDays: envIntOrDefault("CLEANUP_PASSWORD_DAYS", 7),
	}

	return cfg, cfg.Validate()
}

// Validate checks required configuration fields.
func (c *Config) Validate() error {
	if c.Rabbit.URL == "" {
		return fmt.Errorf("config: QUEUE_URL is required")
	}
	if c.Redis.URL == "" {
		return fmt.Errorf("config: REDIS_URL is required")
	}
	return nil
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOrDefault(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

func envDurationOrDefault(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
