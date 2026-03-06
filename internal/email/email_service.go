package email

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	helpers "github.com/Lineblocs/go-helpers"
	"lineblocs.com/scheduler/internal/config"
	"lineblocs.com/scheduler/models"
)

// Service dispatches emails via the Lineblocs API.
type Service struct {
	apiURL string
	apiKey string
}

// NewService creates a new email service from config.
// Fixes the broken URL (was "http://com/api/sendEmail") and API key (was "xxx").
func NewService(cfg *config.Config) *Service {
	return &Service{
		apiURL: cfg.APIURL + "/sendEmail",
		apiKey: cfg.APIKey,
	}
}

// Dispatch sends an email via the API.
func (s *Service) Dispatch(subject, emailType string, user *helpers.User, workspace *helpers.Workspace, args map[string]string) error {
	email := models.Email{
		User:      *user,
		Workspace: *workspace,
		Subject:   subject,
		To:        user.Email,
		EmailType: emailType,
		Args:      args,
	}

	b, err := json.Marshal(email)
	if err != nil {
		return fmt.Errorf("email: marshal: %w", err)
	}

	req, err := http.NewRequest("POST", s.apiURL, bytes.NewBuffer(b))
	if err != nil {
		return fmt.Errorf("email: create request: %w", err)
	}
	req.Header.Set("X-Lineblocs-Key", s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("email: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("email: API returned status %d", resp.StatusCode)
	}

	return nil
}
