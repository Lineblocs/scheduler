package logs

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"lineblocs.com/scheduler/internal/activity"
	"lineblocs.com/scheduler/internal/config"
)

// PurgeActivity removes old debugger logs.
type PurgeActivity struct {
	db  *sqlx.DB
	cfg *config.Config
	log *zap.Logger
}

// NewPurgeActivity creates a new PurgeActivity.
func NewPurgeActivity(db *sqlx.DB, cfg *config.Config, log *zap.Logger) *PurgeActivity {
	return &PurgeActivity{db: db, cfg: cfg, log: log}
}

func (a *PurgeActivity) Name() string { return "logs.purge" }

func (a *PurgeActivity) Retry() activity.RetryPolicy { return activity.DefaultRetryPolicy() }

func (a *PurgeActivity) Timeout() time.Duration { return 2 * time.Minute }

func (a *PurgeActivity) Execute(ctx context.Context, _ []byte) ([]byte, error) {
	cutoff := time.Now().AddDate(0, 0, -a.cfg.LogRetentionDays)
	dateFormatted := cutoff.Format("2006-01-02 15:04:05")

	result, err := a.db.ExecContext(ctx, "DELETE FROM debugger_logs WHERE created_at <= ?", dateFormatted)
	if err != nil {
		return nil, fmt.Errorf("purge_activity: delete: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	a.log.Info("logs purge complete", zap.Int64("deleted", rowsAffected))
	return nil, nil
}
