package cmd

import (
	"fmt"

	helpers "github.com/Lineblocs/go-helpers"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/mailgun/mailgun-go/v4"
	"github.com/sirupsen/logrus"
	utils "lineblocs.com/scheduler/utils"
)

// cron tab to remove unset password users
func CleanupApp() error {
	db, err := utils.GetDBConnection()
	if err != nil {
		return err
	}
	days := "7"
	var id int
	results, err := db.Query(`SELECT id FROM `+"`"+`users`+"`"+` WHERE needs_set_password_date <= DATE_ADD(NOW(), INTERVAL -? DAY) AND needs_password_set = 1`, days)
	if err != nil {
		return err
	}
	defer results.Close()
	for results.Next() {
		results.Scan(&id)
		helpers.Log(logrus.InfoLevel, fmt.Sprintf("Removing user %d\r\n", id))
		_, err := db.Query(`DELETE FROM `+"`"+`users`+"`"+` WHERE id = ?`, id)
		if err != nil {
			helpers.Log(logrus.ErrorLevel, fmt.Sprintf("Could not remove %d\r\n", id))
			continue
		}
	}
	return nil
}
