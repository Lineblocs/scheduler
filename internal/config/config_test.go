package config

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_Defaults(t *testing.T) {
	// Set required env vars
	os.Setenv("QUEUE_URL", "amqp://guest:guest@localhost:5672/")
	os.Setenv("REDIS_URL", "redis://localhost:6379")
	os.Setenv("USE_DOTENV", "off")
	defer func() {
		os.Unsetenv("QUEUE_URL")
		os.Unsetenv("REDIS_URL")
		os.Unsetenv("USE_DOTENV")
	}()

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "localhost", cfg.Database.Host)
	assert.Equal(t, 3306, cfg.Database.Port)
	assert.Equal(t, 25, cfg.Database.MaxOpen)
	assert.Equal(t, 5*time.Minute, cfg.Database.Lifetime)
	assert.Equal(t, "billing_tasks", cfg.Rabbit.BillingTasksQueue)
	assert.Equal(t, "recordings_tasks", cfg.Rabbit.RecordingsTasksQueue)
	assert.Equal(t, 7, cfg.Email.FreeTrialReminderDays)
	assert.Equal(t, 14, cfg.Email.InactivityReminderDays)
	assert.Equal(t, "0 0 1 * *", cfg.Distributor.MonthlyCron)
}

func TestLoad_MissingQueueURL(t *testing.T) {
	os.Setenv("QUEUE_URL", "")
	os.Setenv("REDIS_URL", "redis://localhost:6379")
	os.Setenv("USE_DOTENV", "off")
	defer func() {
		os.Unsetenv("QUEUE_URL")
		os.Unsetenv("REDIS_URL")
		os.Unsetenv("USE_DOTENV")
	}()

	_, err := Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "QUEUE_URL")
}

func TestLoad_MissingRedisURL(t *testing.T) {
	os.Setenv("QUEUE_URL", "amqp://localhost")
	os.Setenv("REDIS_URL", "")
	os.Setenv("USE_DOTENV", "off")
	defer func() {
		os.Unsetenv("QUEUE_URL")
		os.Unsetenv("REDIS_URL")
		os.Unsetenv("USE_DOTENV")
	}()

	_, err := Load()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "REDIS_URL")
}

func TestDSN(t *testing.T) {
	cfg := &Config{
		Database: DatabaseConfig{
			User:     "user",
			Password: "pass",
			Host:     "db.example.com",
			Port:     3307,
			Name:     "mydb",
		},
	}
	expected := "user:pass@tcp(db.example.com:3307)/mydb?parseTime=true"
	assert.Equal(t, expected, cfg.Database.DSN())
}
