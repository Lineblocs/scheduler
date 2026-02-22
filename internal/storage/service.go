package storage

import (
	"bytes"
	"database/sql"
	"fmt"
	"lineblocs.com/scheduler/models"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/CyCoreSystems/ari/v5"
)

type RecordingService struct {
	db        *sql.DB
	ariClient *ari.Client
	settings  *models.Settings // Shared settings model
}

func NewRecordingService(db *sql.DB, ari *ari.Client, settings *models.Settings) *RecordingService {
	return &RecordingService{
		db:        db,
		ariClient: ari,
		settings:  settings,
	}
}

func (s *RecordingService) ProcessRecording(task models.RecordingTask) error {
	fmt.Printf("Processing Recording ID: %d, StorageID: %d\n", task.ID, task.StorageID)

	// 1. Get File from ARI
	src := ari.NewKey(ari.StoredRecordingKey, fmt.Sprintf("%d", task.StorageID))
	data, err := (*s.ariClient).StoredRecording().File(src)
	if err != nil {
		s.db.Exec("UPDATE recordings SET relocation_attempts = relocation_attempts + 1 WHERE id = ?", task.ID)
		return fmt.Errorf("failed to get file from ARI: %w", err)
	}

	// 2. Optional Trimming
	if task.Trim == "true" {
		// logic for trimming silence
	}

	// 3. Upload to S3
	filename := fmt.Sprintf("%d.wav", task.StorageID)
	s3Url, err := s.uploadToS3(data, filename)
	if err != nil {
		return err
	}

	// 4. Update Database
	_, err = s.db.Exec("UPDATE recordings SET s3_url = ?, status='processed' WHERE id = ?", s3Url, task.ID)
	if err != nil {
		return fmt.Errorf("failed to update database: %w", err)
	}

	// 5. Cleanup ARI
	return (*s.ariClient).StoredRecording().Delete(src)
}

func (s *RecordingService) uploadToS3(data []byte, filename string) (string, error) {
	sess, _ := session.NewSession(&aws.Config{
		Region: aws.String(s.settings.Credentials["aws_region"]),
		Credentials: credentials.NewStaticCredentials(
			s.settings.Credentials["aws_access_key_id"],
			s.settings.Credentials["aws_secret_access_key"], ""),
	})

	uploader := s3manager.NewUploader(sess)
	result, err := uploader.Upload(&s3manager.UploadInput{
		Bucket: aws.String(s.settings.Credentials["s3_bucket"]),
		Key:    aws.String("recordings/" + filename),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return "", err
	}
	return aws.StringValue(&result.Location), nil
}