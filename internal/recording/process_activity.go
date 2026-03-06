package recording

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/CyCoreSystems/ari/v5"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"lineblocs.com/scheduler/internal/activity"
	"lineblocs.com/scheduler/models"
)

// ProcessActivity handles recording processing (ARI -> S3).
type ProcessActivity struct {
	db        *sqlx.DB
	ariClient *ari.Client
	settings  *models.Settings
	log       *zap.Logger
}

// NewProcessActivity creates a new ProcessActivity.
func NewProcessActivity(db *sqlx.DB, ariClient *ari.Client, settings *models.Settings, log *zap.Logger) *ProcessActivity {
	return &ProcessActivity{
		db:        db,
		ariClient: ariClient,
		settings:  settings,
		log:       log,
	}
}

func (a *ProcessActivity) Name() string { return "recording.process" }

func (a *ProcessActivity) Retry() activity.RetryPolicy {
	return activity.RetryPolicy{
		MaxAttempts:     3,
		InitialInterval: 10 * time.Second,
		MaxInterval:     2 * time.Minute,
		BackoffFactor:   2.0,
	}
}

func (a *ProcessActivity) Timeout() time.Duration { return 5 * time.Minute }

func (a *ProcessActivity) Execute(ctx context.Context, input []byte) ([]byte, error) {
	var task models.RecordingTask
	if err := json.Unmarshal(input, &task); err != nil {
		return nil, fmt.Errorf("recording_activity: unmarshal: %w", err)
	}

	log := a.log.With(zap.Int("recording_id", task.ID))
	log.Info("processing recording")

	// Get file from ARI
	src := ari.NewKey(ari.StoredRecordingKey, task.StorageID)
	data, err := (*a.ariClient).StoredRecording().File(src)
	if err != nil {
		if _, dbErr := a.db.ExecContext(ctx, "UPDATE recordings SET relocation_attempts = relocation_attempts + 1 WHERE id = ?", task.ID); dbErr != nil {
			log.Error("failed to increment relocation attempts", zap.Error(dbErr))
		}
		return nil, fmt.Errorf("recording_activity: get ARI file: %w", err)
	}

	// Upload to S3
	filename := fmt.Sprintf("%s.wav", task.StorageID)
	s3URL, err := a.uploadToS3(data, filename)
	if err != nil {
		return nil, fmt.Errorf("recording_activity: upload S3: %w", err)
	}

	// Update database
	if _, err := a.db.ExecContext(ctx, "UPDATE recordings SET s3_url = ?, status = 'processed' WHERE id = ?", s3URL, task.ID); err != nil {
		return nil, fmt.Errorf("recording_activity: update db: %w", err)
	}

	// Cleanup ARI
	if err := (*a.ariClient).StoredRecording().Delete(src); err != nil {
		log.Warn("failed to delete ARI recording", zap.Error(err))
	}

	log.Info("recording processed", zap.String("s3_url", s3URL))
	return nil, nil
}

func (a *ProcessActivity) uploadToS3(data []byte, filename string) (string, error) {
	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(a.settings.Credentials["aws_region"]),
		Credentials: credentials.NewStaticCredentials(
			a.settings.Credentials["aws_access_key_id"],
			a.settings.Credentials["aws_secret_access_key"], ""),
	})
	if err != nil {
		return "", fmt.Errorf("recording_activity: create AWS session: %w", err)
	}

	uploader := s3manager.NewUploader(sess)
	result, err := uploader.Upload(&s3manager.UploadInput{
		Bucket: aws.String(a.settings.Credentials["s3_bucket"]),
		Key:    aws.String("recordings/" + filename),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return "", fmt.Errorf("recording_activity: S3 upload: %w", err)
	}
	return aws.StringValue(&result.Location), nil
}
