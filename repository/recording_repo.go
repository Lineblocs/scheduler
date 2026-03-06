package repository

import (
	"context"
	"fmt"

	"lineblocs.com/scheduler/internal/db"
	"lineblocs.com/scheduler/models"
)

// RecordingRepo provides database access for recordings.
type RecordingRepo struct {
	db db.DBTX
}

// NewRecordingRepo creates a new RecordingRepo.
func NewRecordingRepo(dbtx db.DBTX) *RecordingRepo {
	return &RecordingRepo{db: dbtx}
}

// WithTx returns a copy using the given transaction.
func (r *RecordingRepo) WithTx(tx db.DBTX) *RecordingRepo {
	return &RecordingRepo{db: tx}
}

// GetForPeriod returns recordings for a user within a billing period.
func (r *RecordingRepo) GetForPeriod(ctx context.Context, userID int, start, end string) ([]models.RecordingRow, error) {
	var recordings []models.RecordingRow
	err := r.db.SelectContext(ctx, &recordings,
		"SELECT id, size, created_at FROM recordings WHERE user_id = ? AND created_at BETWEEN ? AND ?",
		userID, start, end)
	if err != nil {
		return nil, fmt.Errorf("recording_repo: get for period: %w", err)
	}
	return recordings, nil
}

