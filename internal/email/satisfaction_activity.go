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

// SatisfactionActivity sends customer satisfaction surveys.
type SatisfactionActivity struct {
	db       *sqlx.DB
	emailSvc *Service
	cfg      *config.Config
	log      *zap.Logger
}

// NewSatisfactionActivity creates a new SatisfactionActivity.
func NewSatisfactionActivity(db *sqlx.DB, emailSvc *Service, cfg *config.Config, log *zap.Logger) *SatisfactionActivity {
	return &SatisfactionActivity{db: db, emailSvc: emailSvc, cfg: cfg, log: log}
}

func (a *SatisfactionActivity) Name() string { return "email.satisfaction" }

func (a *SatisfactionActivity) Retry() activity.RetryPolicy { return activity.DefaultRetryPolicy() }

func (a *SatisfactionActivity) Timeout() time.Duration { return 5 * time.Minute }

func (a *SatisfactionActivity) Execute(ctx context.Context, _ []byte) ([]byte, error) {
	numDays := a.cfg.Email.SatisfactionSurveyDays

	rows, err := a.db.QueryxContext(ctx,
		`SELECT workspaces.id, workspaces.name, workspaces.plan, workspaces.created_at, workspaces.sent_satisfaction_survey,
		users.username, users.email, users.first_name, users.last_name, users.stripe_id, users.id
		FROM workspaces JOIN users ON users.id = workspaces.creator_id`)
	if err != nil {
		return nil, fmt.Errorf("satisfaction_activity: query: %w", err)
	}
	defer rows.Close()

	now := time.Now()

	for rows.Next() {
		var wsID int
		var wsName, wsPlan string
		var createdDate time.Time
		var sentSurvey int
		var username, email, fname, lname, stripeID string
		var userID int

		if err := rows.Scan(&wsID, &wsName, &wsPlan, &createdDate, &sentSurvey,
			&username, &email, &fname, &lname, &stripeID, &userID); err != nil {
			a.log.Warn("scan error", zap.Error(err))
			continue
		}

		daysElapsed := int(now.Sub(createdDate).Hours() / 24)
		if daysElapsed >= numDays && sentSurvey == 0 {
			user := helpers.CreateUser(userID, username, fname, lname, email, stripeID)
			workspace := helpers.CreateWorkspace(wsID, wsName, userID, nil, wsPlan, nil, nil)

			if err := a.emailSvc.Dispatch("Customer satisfaction survey", "customer_satisfaction_survey", user, workspace, map[string]string{}); err != nil {
				a.log.Error("email dispatch failed", zap.Error(err))
				continue
			}

			if _, err := a.db.ExecContext(ctx, "UPDATE workspaces SET sent_satisfaction_survey = 1 WHERE id = ?", wsID); err != nil {
				a.log.Error("update workspace failed", zap.Error(err))
			}
		}
	}

	return nil, nil
}
