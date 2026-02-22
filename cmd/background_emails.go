package cmd

import (
	"fmt"
	"time"

	"database/sql"
	"math"
	"strconv"

	helpers "github.com/Lineblocs/go-helpers"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/mailgun/mailgun-go/v4"
	"github.com/sirupsen/logrus"
	utils "lineblocs.com/scheduler/utils"
)

func notifyForCardExpiry(db *sql.DB) error {
	now := time.Now()
	year, monthStr, _ := now.Date()
	month := int(monthStr)

	// change this to a JOIN
	results, err := db.Query("SELECT user_cards.exp_month, users_cards.exp_year, users_cards.user_id, users_cards.workspace_id, users_cards.last_4 FROM users_cards")
	if err != nil {
		helpers.Log(logrus.ErrorLevel, "error getting workspaces..\r\n")
		helpers.Log(logrus.ErrorLevel, err.Error())
		return err
	}

	defer results.Close()
	var expMonth int
	var expYear int
	var userId int
	var workspaceId int
	var last4 string

	for results.Next() {
		args := make(map[string]string)

		subject := "Card expiring soon"

		results.Scan(&expMonth, &expYear, &userId, last4)
		currentLocation := now.Location()

		firstOfMonth := time.Date(year, monthStr, 1, 0, 0, 0, 0, currentLocation)
		lastOfMonth := firstOfMonth.AddDate(0, 1, -1)
		_, _, lastDayStr := lastOfMonth.Date()
		lastDay := lastDayStr

		daysUntilExpiry := strconv.Itoa(lastDay + 1)

		args["ending_digits"] = last4
		args["days"] = daysUntilExpiry

		if expYear == year && (expMonth-month) == 1 { // 1 month until credit card expiry
			user, err := helpers.GetUserFromDB(userId)
			if err != nil {
				helpers.Log(logrus.ErrorLevel, "could not get user from DB\r\n")
				continue
			}

			workspace, err := helpers.GetWorkspaceFromDB(workspaceId)
			if err != nil {
				helpers.Log(logrus.ErrorLevel, "could not get workspace from DB\r\n")
				continue
			}

			err = utils.DispatchEmail(subject, "card_expiring", user, workspace, args)
			if err != nil {
				helpers.Log(logrus.ErrorLevel, "could not send email\r\n")
				helpers.Log(logrus.ErrorLevel, err.Error())
				continue
			}
		}
	}

	return nil
}

func sendCustomerSatisfactionSurvey(db *sql.DB) error {
	now := time.Now()
	numDaysToWait := 7

	results, err := db.Query("SELECT workspaces.id, workspaces.name, workspaces.plan, workspaces.created_at, workspaces.sent_satisfaction_survey, users.username, users.email, users.first_name, users.last_name, users.stripe_id, users.id FROM workspaces JOIN users ON users.id = workspaces.creator_id")
	if err != nil {
		helpers.Log(logrus.ErrorLevel, "error getting workspaces..\r\n")
		helpers.Log(logrus.ErrorLevel, err.Error())
		return err
	}

	defer results.Close()
	var workspaceName string
	var workspaceId int
	var createdDate time.Time
	var workspacePlan string
	var sentSurvey int
	var username string
	var userEmail string
	var fname string
	var lname string
	var stripeId string
	var userId int

	for results.Next() {
		args := make(map[string]string)

		subject := "Customer satisfaction survey"

		results.Scan(&workspaceId, &workspaceName, &workspacePlan, &createdDate, &sentSurvey, &username, &userEmail, &fname, &lname, &stripeId, &userId)

		user := helpers.CreateUser(userId, username, fname, lname, userEmail, stripeId)
		workspace := helpers.CreateWorkspace(workspaceId, workspaceName, userId, nil, workspacePlan, nil, nil)

		diff := now.Sub(createdDate)
		daysElapsed := int(diff.Hours() / 24) // number of days

		if daysElapsed >= numDaysToWait && sentSurvey == 0 {
			err = utils.DispatchEmail(subject, "customer_satisfaction_survey", user, workspace, args)

			// TODO: move this to ensure emails are sent before updating database
			_, errdb := db.Query("UPDATE workspaces SET sent_satisfaction_survey = 1 WHERE id = ?", workspaceId)
			if errdb != nil {
				helpers.Log(logrus.ErrorLevel, fmt.Sprintf("error %s updating database. error: 5s\r\n", err.Error()))
				helpers.Log(logrus.ErrorLevel, err.Error())
				continue
			}

			if err != nil {
				helpers.Log(logrus.ErrorLevel, "could not send email\r\n")
				helpers.Log(logrus.ErrorLevel, err.Error())
				continue
			}
		}
	}

	return nil
}

// cron tab to email users to tell them that their free trial will be ending soon
func SendBackgroundEmails() error {
	db, err := utils.GetDBConnection()
	if err != nil {
		return err
	}

	ago := time.Time{}
	ago = ago.AddDate(0, 0, -14)

	dateFormatted := ago.Format("2006-01-02 15:04:05")
	results, err := db.Query("SELECT workspaces.id, workspaces.creator_id from workspaces inner join users on users.id = workspaces.creator_id where users.last_login >= ? AND users.last_login_reminded IS NULL", dateFormatted)
	if err != nil {
		helpers.Log(logrus.PanicLevel, "error getting workspaces..\r\n")
		helpers.Log(logrus.PanicLevel, err.Error())
		return err
	}

	defer results.Close()
	// declare some common variables
	var id int
	var creatorId int

	for results.Next() {
		results.Scan(&id, &creatorId)

		helpers.Log(logrus.InfoLevel, fmt.Sprintf("Reminding user %d to use Lineblocs!\r\n", creatorId))
		user, err := helpers.GetUserFromDB(creatorId)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "could not get user from DB\r\n")
			continue
		}
		workspace, err := helpers.GetWorkspaceFromDB(id)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "could not get workspace from DB\r\n")
			continue
		}

		args := make(map[string]string)
		subject := "Account Inactivity"
		err = utils.DispatchEmail(subject, "inactive_user", user, workspace, args)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "could not send email\r\n")
			helpers.Log(logrus.ErrorLevel, err.Error())
			continue
		}
		stmt, err := db.Prepare("UPDATE users SET last_login_reminded = NOW()")
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "could not prepare query..\r\n")
			continue
		}
		_, err = stmt.Exec()
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "error updating users table..\r\n")
			helpers.Log(logrus.ErrorLevel, err.Error())
			continue
		}
	}

	// usage triggers
	results, err = db.Query("SELECT workspaces.id, workspaces.creator_id from workspaces inner join users on users.id = workspaces.creator_id")
	if err != nil {
		helpers.Log(logrus.ErrorLevel, "error getting workspaces..\r\n")
		helpers.Log(logrus.ErrorLevel, err.Error())
		return err
	}

	defer results.Close()
	var creditId int
	var balance int
	var triggerId int
	var percentage int
	for results.Next() {
		results.Scan(&id, &creatorId)
		helpers.Log(logrus.InfoLevel, fmt.Sprintf("working with id: %d, creator %d\r\n", id, creatorId))
		user, err := helpers.GetUserFromDB(creatorId)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "could not get user from DB\r\n")
			continue
		}
		workspace, err := helpers.GetWorkspaceFromDB(id)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "could not get workspace from DB\r\n")
			continue
		}
		row := db.QueryRow(`SELECT id, balance FROM users_credits WHERE workspace_id=?`, workspace.Id)
		err = row.Scan(&creditId, &balance)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "could not get last balance of user..\r\n")
			helpers.Log(logrus.ErrorLevel, err.Error())
			continue
		}
		billingInfo, err := helpers.GetWorkspaceBillingInfo(workspace)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "Could not get billing info..\r\n")
			helpers.Log(logrus.ErrorLevel, err.Error())
			continue
		}

		results2, _ := db.Query("SELECT id, percentage from usage_triggers where workspace_id = ?", workspace.Id)
		defer results2.Close()

		for results2.Next() {
			results2.Scan(&triggerId, &percentage)
			var triggerUsageId int
			row := db.QueryRow(`SELECT id FROM users WHERE id=?`, triggerId)
			err := row.Scan(&triggerUsageId)
			if err == sql.ErrNoRows {
				helpers.Log(logrus.InfoLevel, "Trigger reminder already sent..\r\n")
				continue
			}
			if err != nil { //another error
				helpers.Log(logrus.ErrorLevel, "SQL error\r\n")
				helpers.Log(logrus.ErrorLevel, err.Error())
				continue
			}

			percentOfTrigger, err := strconv.ParseFloat(".%d", percentage)
			if err != nil {
				helpers.Log(logrus.ErrorLevel, fmt.Sprintf("error using ParseFloat on .%d\r\n", percentage))
				helpers.Log(logrus.ErrorLevel, err.Error())
				continue
			}
			amount := math.Round(float64(balance) * percentOfTrigger)

			if billingInfo.RemainingBalanceCents <= amount {
				args := make(map[string]string)
				args["triggerPercent"] = fmt.Sprintf("%f", percentOfTrigger)
				args["triggerBalance"] = fmt.Sprintf("%d", balance)

				subject := "Usage Trigger Alert"
				err = utils.DispatchEmail(subject, "usage_trigger", user, workspace, args)
				if err != nil {
					helpers.Log(logrus.ErrorLevel, "could not send email\r\n")
					helpers.Log(logrus.ErrorLevel, err.Error())
					continue
				}

				stmt, err := db.Prepare("INSERT INTO usage_triggers_results (usage_trigger_id) VALUES (?)")
				if err != nil {
					helpers.Log(logrus.ErrorLevel, "could not prepare query..\r\n")
					continue
				}

				defer stmt.Close()
				_, err = stmt.Exec(triggerId)
				if err != nil {
					helpers.Log(logrus.ErrorLevel, "error create usage trigger result..\r\n")
					continue
				}
			}
		}
	}

	days := "7"
	results, err = db.Query(`SELECT id, creator_id FROM `+"`"+`workspaces`+"`"+` WHERE free_trial_started <= DATE_ADD(NOW(), INTERVAL -? DAY) AND free_trial_reminder_sent = 0`, days)
	if err != nil {
		return err
	}
	defer results.Close()
	for results.Next() {
		results.Scan(&id)
		results.Scan(&creatorId)
		user, err := helpers.GetUserFromDB(creatorId)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "could not get user from DB\r\n")
			continue
		}
		workspace, err := helpers.GetWorkspaceFromDB(id)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "could not get workspace from DB\r\n")
			continue
		}
		args := make(map[string]string)
		subject := "Free trial is ending"
		err = utils.DispatchEmail(subject, "free_trial_expiring", user, workspace, args)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "could not send email\r\n")
			helpers.Log(logrus.ErrorLevel, err.Error())
			continue
		}
		stmt, err := db.Prepare("UPDATE workspaces SET free_trial_reminder_sent = 1 WHERE id = ?")
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "could not prepare query..\r\n")
			continue
		}
		_, err = stmt.Exec(workspace.Id)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, "error updating DB..\r\n")
			helpers.Log(logrus.ErrorLevel, err.Error())
			continue
		}
	}

	err = notifyForCardExpiry(db)
	if err != nil {
		return err
	}

	return nil
}
