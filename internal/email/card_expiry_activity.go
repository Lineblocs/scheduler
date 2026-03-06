package email

import (
	"context"
	"fmt"
	"strconv"
	"time"

	helpers "github.com/Lineblocs/go-helpers"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"lineblocs.com/scheduler/internal/activity"
	"lineblocs.com/scheduler/internal/config"
)

// CardExpiryActivity sends notifications for expiring cards.
type CardExpiryActivity struct {
	db       *sqlx.DB
	emailSvc *Service
	cfg      *config.Config
	log      *zap.Logger
}

// NewCardExpiryActivity creates a new CardExpiryActivity.
func NewCardExpiryActivity(db *sqlx.DB, emailSvc *Service, cfg *config.Config, log *zap.Logger) *CardExpiryActivity {
	return &CardExpiryActivity{db: db, emailSvc: emailSvc, cfg: cfg, log: log}
}

func (a *CardExpiryActivity) Name() string { return "email.card_expiry" }

func (a *CardExpiryActivity) Retry() activity.RetryPolicy { return activity.DefaultRetryPolicy() }

func (a *CardExpiryActivity) Timeout() time.Duration { return 5 * time.Minute }

func (a *CardExpiryActivity) Execute(ctx context.Context, _ []byte) ([]byte, error) {
	now := time.Now()
	year, monthTime, _ := now.Date()
	month := int(monthTime)

	rows, err := a.db.QueryxContext(ctx,
		`SELECT users_cards.exp_month, users_cards.exp_year, users_cards.user_id, users_cards.workspace_id, users_cards.last_4 FROM users_cards`)
	if err != nil {
		return nil, fmt.Errorf("card_expiry_activity: query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var expMonth, expYear, userID, workspaceID int
		var last4 string
		if err := rows.Scan(&expMonth, &expYear, &userID, &workspaceID, &last4); err != nil {
			a.log.Warn("scan error", zap.Error(err))
			continue
		}

		if expYear == year && (expMonth-month) <= a.cfg.Email.CardExpiryWarningMonths && (expMonth-month) > 0 {
			user, err := helpers.GetUserFromDB(userID)
			if err != nil {
				a.log.Warn("get user error", zap.Error(err))
				continue
			}

			workspace, err := helpers.GetWorkspaceFromDB(workspaceID)
			if err != nil {
				a.log.Warn("get workspace error", zap.Error(err))
				continue
			}

			firstOfMonth := time.Date(year, monthTime, 1, 0, 0, 0, 0, now.Location())
			lastOfMonth := firstOfMonth.AddDate(0, 1, -1)
			_, _, lastDay := lastOfMonth.Date()

			args := map[string]string{
				"ending_digits": last4,
				"days":          strconv.Itoa(lastDay + 1),
			}

			if err := a.emailSvc.Dispatch("Card expiring soon", "card_expiring", user, workspace, args); err != nil {
				a.log.Error("email dispatch failed", zap.Error(err))
			}
		}
	}

	return nil, nil
}
