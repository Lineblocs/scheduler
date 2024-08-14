package cmd

import (
	"time"

	helpers "github.com/Lineblocs/go-helpers"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/mailgun/mailgun-go/v4"
	"github.com/sirupsen/logrus"
	utils "lineblocs.com/crontabs/utils"
)

// remove any logs older than retention period
func RemoveLogs() error {
	db, err := utils.GetDBConnection()
	if err != nil {
		return err
	}

	dateNow := time.Time{}
	// 7 day retention
	dateNow = dateNow.AddDate(0, 0, -7)
	dateFormatted := dateNow.Format("2006-01-02 15:04:05")
	_, err = db.Exec("DELETE from debugger_logs where created_at >= ?", dateFormatted)
	if err != nil {
		helpers.Log(logrus.ErrorLevel, "error occurred in log removing\r\n")
		helpers.Log(logrus.ErrorLevel, err.Error())
		return err
	}
	return nil
}
