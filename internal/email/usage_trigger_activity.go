package email

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"time"

	helpers "github.com/Lineblocs/go-helpers"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"lineblocs.com/scheduler/internal/activity"
)

// UsageTriggerActivity sends usage trigger alerts.
type UsageTriggerActivity struct {
	db       *sqlx.DB
	emailSvc *Service
	log      *zap.Logger
}

// NewUsageTriggerActivity creates a new UsageTriggerActivity.
func NewUsageTriggerActivity(db *sqlx.DB, emailSvc *Service, log *zap.Logger) *UsageTriggerActivity {
	return &UsageTriggerActivity{db: db, emailSvc: emailSvc, log: log}
}

func (a *UsageTriggerActivity) Name() string { return "email.usage_trigger" }

func (a *UsageTriggerActivity) Retry() activity.RetryPolicy { return activity.DefaultRetryPolicy() }

func (a *UsageTriggerActivity) Timeout() time.Duration { return 5 * time.Minute }

func (a *UsageTriggerActivity) Execute(ctx context.Context, _ []byte) ([]byte, error) {
	rows, err := a.db.QueryxContext(ctx,
		`SELECT workspaces.id, workspaces.creator_id FROM workspaces
		INNER JOIN users ON users.id = workspaces.creator_id`)
	if err != nil {
		return nil, fmt.Errorf("usage_trigger_activity: query workspaces: %w", err)
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

		var creditID, balance int
		if err := a.db.QueryRowContext(ctx, "SELECT id, balance FROM users_credits WHERE workspace_id = ?", wsID).Scan(&creditID, &balance); err != nil {
			if err != sql.ErrNoRows {
				a.log.Warn("get credits error", zap.Error(err))
			}
			continue
		}

		billingInfo, err := helpers.GetWorkspaceBillingInfo(workspace)
		if err != nil {
			a.log.Warn("get billing info error", zap.Error(err))
			continue
		}

		triggerRows, err := a.db.QueryxContext(ctx, "SELECT id, percentage FROM usage_triggers WHERE workspace_id = ?", wsID)
		if err != nil {
			a.log.Warn("get triggers error", zap.Error(err))
			continue
		}

		for triggerRows.Next() {
			var triggerID, percentage int
			if err := triggerRows.Scan(&triggerID, &percentage); err != nil {
				a.log.Warn("scan trigger error", zap.Error(err))
				continue
			}

			// Check if trigger already fired
			var existingID int
			err := a.db.QueryRowContext(ctx, "SELECT id FROM usage_triggers_results WHERE usage_trigger_id = ?", triggerID).Scan(&existingID)
			if err == nil {
				continue // already sent
			}
			if err != sql.ErrNoRows {
				a.log.Warn("check trigger result error", zap.Error(err))
				continue
			}

			// Fixed: parse percentage correctly (was using invalid format string ".%d")
			percentOfTrigger := float64(percentage) / 100.0
			amount := math.Round(float64(balance) * percentOfTrigger)

			if billingInfo.RemainingBalanceCents <= int64(amount) {
				args := map[string]string{
					"triggerPercent": fmt.Sprintf("%.2f", percentOfTrigger),
					"triggerBalance": strconv.Itoa(balance),
				}

				if err := a.emailSvc.Dispatch("Usage Trigger Alert", "usage_trigger", user, workspace, args); err != nil {
					a.log.Error("email dispatch failed", zap.Error(err))
					continue
				}

				if _, err := a.db.ExecContext(ctx, "INSERT INTO usage_triggers_results (usage_trigger_id) VALUES (?)", triggerID); err != nil {
					a.log.Error("insert trigger result failed", zap.Error(err))
				}
			}
		}
		triggerRows.Close()
	}

	return nil, nil
}
