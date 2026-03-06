package email

import (
	"context"
	"fmt"
	"time"

	helpers "github.com/Lineblocs/go-helpers"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"lineblocs.com/scheduler/internal/activity"
	"lineblocs.com/scheduler/internal/config"
)

// FreeTrialActivity sends reminders for expiring free trials.
type FreeTrialActivity struct {
	db       *sqlx.DB
	emailSvc *Service
	cfg      *config.Config
	log      *zap.Logger
}

// NewFreeTrialActivity creates a new FreeTrialActivity.
func NewFreeTrialActivity(db *sqlx.DB, emailSvc *Service, cfg *config.Config, log *zap.Logger) *FreeTrialActivity {
	return &FreeTrialActivity{db: db, emailSvc: emailSvc, cfg: cfg, log: log}
}

func (a *FreeTrialActivity) Name() string { return "email.free_trial" }

func (a *FreeTrialActivity) Retry() activity.RetryPolicy { return activity.DefaultRetryPolicy() }

func (a *FreeTrialActivity) Timeout() time.Duration { return 5 * time.Minute }

func (a *FreeTrialActivity) Execute(ctx context.Context, _ []byte) ([]byte, error) {
	days := a.cfg.Email.FreeTrialReminderDays

	rows, err := a.db.QueryxContext(ctx,
		`SELECT id, creator_id FROM workspaces
		WHERE free_trial_started <= DATE_ADD(NOW(), INTERVAL -? DAY) AND free_trial_reminder_sent = 0`, days)
	if err != nil {
		return nil, fmt.Errorf("free_trial_activity: query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var wsID, creatorID int
		if err := rows.Scan(&wsID, &creatorID); err != nil {
			a.log.Warn("scan error", zap.Error(err))
			continue
		}

		user, err := helpers.GetUserFromDB(creatorID)
		if err != nil {
			a.log.Warn("get user error", zap.Error(err))
			continue
		}

		workspace, err := helpers.GetWorkspaceFromDB(wsID)
		if err != nil {
			a.log.Warn("get workspace error", zap.Error(err))
			continue
		}

		if err := a.emailSvc.Dispatch("Free trial is ending", "free_trial_expiring", user, workspace, map[string]string{}); err != nil {
			a.log.Error("email dispatch failed", zap.Error(err))
			continue
		}

		if _, err := a.db.ExecContext(ctx, "UPDATE workspaces SET free_trial_reminder_sent = 1 WHERE id = ?", wsID); err != nil {
			a.log.Error("update workspace failed", zap.Error(err))
		}
	}

	return nil, nil
}
