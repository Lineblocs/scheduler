package models

import (
	helpers "github.com/Lineblocs/go-helpers"
)

type Email struct {
	Args      map[string]string `json:"args"`
	User      helpers.User      `json:"user"`
	EmailType string            `json:"email_type"`
	Subject   string            `json:"subject"`
	To        string            `json:"to"`
	Workspace helpers.Workspace `json:"workspace"`
}
