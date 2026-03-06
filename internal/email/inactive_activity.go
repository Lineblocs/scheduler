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

// InactiveActivity sends reminders to inactive users.
type InactiveActivity struct {
	db      *sqlx.DB
	emailSvc *Service
	cfg     *config.Config
	log     *zap.Logger
}

// NewInactiveActivity creates a new InactiveActivity.
func NewInactiveActivity(db *sqlx.DB, emailSvc *Service, cfg *config.Config, log *zap.Logger) *InactiveActivity {
	return &InactiveActivity{db: db, emailSvc: emailSvc, cfg: cfg, log: log}
}

func (a *InactiveActivity) Name() string { return "email.inactive" }

func (a *InactiveActivity) Retry() activity.RetryPolicy { return activity.DefaultRetryPolicy() }

func (a *InactiveActivity) Timeout() time.Duration { return 5 * time.Minute }

func (a *InactiveActivity) Execute(ctx context.Context, _ []byte) ([]byte, error) {
	days := a.cfg.Email.InactivityReminderDays
	ago := time.Now().AddDate(0, 0, -days)
	dateFormatted := ago.Format("2006-01-02 15:04:05")

	rows, err := a.db.QueryxContext(ctx,
		`SELECT workspaces.id, workspaces.creator_id FROM workspaces
		INNER JOIN users ON users.id = workspaces.creator_id
		WHERE users.last_login >= ? AND users.last_login_reminded IS NULL`, dateFormatted)
	if err != nil {
		return nil, fmt.Errorf("inactive_activity: query: %w", err)
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
			a.log.Warn("get user error", zap.Int("user_id", creatorID), zap.Error(err))
			continue
		}

		workspace, err := helpers.GetWorkspaceFromDB(wsID)
		if err != nil {
			a.log.Warn("get workspace error", zap.Int("workspace_id", wsID), zap.Error(err))
			continue
		}

		if err := a.emailSvc.Dispatch("Account Inactivity", "inactive_user", user, workspace, map[string]string{}); err != nil {
			a.log.Error("email dispatch failed", zap.Error(err))
			continue
		}

		if _, err := a.db.ExecContext(ctx, "UPDATE users SET last_login_reminded = NOW() WHERE id = ?", creatorID); err != nil {
			a.log.Error("update user failed", zap.Error(err))
		}
	}

	return nil, nil
}
