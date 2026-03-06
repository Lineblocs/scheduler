package cleanup

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"lineblocs.com/scheduler/internal/activity"
	"lineblocs.com/scheduler/internal/config"
)

// StaleUsersActivity removes users who haven't set their password.
type StaleUsersActivity struct {
	db  *sqlx.DB
	cfg *config.Config
	log *zap.Logger
}

// NewStaleUsersActivity creates a new StaleUsersActivity.
func NewStaleUsersActivity(db *sqlx.DB, cfg *config.Config, log *zap.Logger) *StaleUsersActivity {
	return &StaleUsersActivity{db: db, cfg: cfg, log: log}
}

func (a *StaleUsersActivity) Name() string { return "cleanup.stale_users" }

func (a *StaleUsersActivity) Retry() activity.RetryPolicy { return activity.DefaultRetryPolicy() }

func (a *StaleUsersActivity) Timeout() time.Duration { return 2 * time.Minute }

func (a *StaleUsersActivity) Execute(ctx context.Context, _ []byte) ([]byte, error) {
	days := a.cfg.CleanupPasswordDays

	rows, err := a.db.QueryxContext(ctx,
		"SELECT id FROM users WHERE needs_set_password_date <= DATE_ADD(NOW(), INTERVAL -? DAY) AND needs_password_set = 1", days)
	if err != nil {
		return nil, fmt.Errorf("cleanup_activity: query: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			a.log.Warn("scan error", zap.Error(err))
			continue
		}

		if _, err := a.db.ExecContext(ctx, "DELETE FROM users WHERE id = ?", id); err != nil {
			a.log.Error("failed to delete user", zap.Int("user_id", id), zap.Error(err))
			continue
		}
		count++
	}

	a.log.Info("stale users cleanup complete", zap.Int("removed", count))
	return nil, nil
}
