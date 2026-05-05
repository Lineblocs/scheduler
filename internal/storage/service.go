package storage

import (
	"bytes"
	"database/sql"
	"fmt"
    "io"
	"strconv"
	"net/http"
	"encoding/json"
    "encoding/base64"
	"lineblocs.com/scheduler/models"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/CyCoreSystems/ari/v5"
	helpers "github.com/Lineblocs/go-helpers"
)

type RecordingService struct {
	db        *sql.DB
	ariClient *ari.Client
	settings  *helpers.APICredentials // Shared settings model
}


type CallSpeaker struct {
    SpeakerName   string  `json:"speaker_name"`
    StartTalkTime float64 `json:"start_talk_time"`
    EndTalkTime   float64 `json:"end_talk_time"`
}

type CallChapter struct {
    Title     string  `json:"title"`
    Summary   string  `json:"summary"`
    StartTime float64 `json:"start_time"`
}

type CallActionItem struct {
    SpeakerName string `json:"speaker_name"`
    ActionItem  string `json:"action_item"`
    Status      string `json:"status"`
    Priority    string `json:"priority"`
}

type CallSummary struct {
    Speakers    []CallSpeaker    `json:"speakers"`
    Chapters    []CallChapter    `json:"chapters"`
    ActionItems []CallActionItem `json:"action_items"`
}


func NewRecordingService(db *sql.DB, ari *ari.Client, settings *models.Settings) *RecordingService {
	apiCreds, err := helpers.GetAPICredentials()
	if err != nil {
		panic(fmt.Sprintf("Critical: Could not load API credentials: %v", err))
	}

	return &RecordingService{
		db:        db,
		ariClient: ari,
		settings:  apiCreds,
	}
}

// TODO: this should be updated to get a unique
// ARI connection for each storage server. in essence, it should pick a connection
// from a list/hashmap
func (s *RecordingService) retrieveARIConnection(storageServerIp string) (*ari.Client, error) {
	return s.ariClient, nil
}


func (s *RecordingService) ProcessRecording(task models.RecordingTask) error {
	fmt.Printf("Starting processing for Recording ID: %d, StorageID: %s, StorageServerIP: %s\n", task.ID, task.StorageID, task.StorageServerIP)

	client, err :=s.retrieveARIConnection(task.StorageServerIP)
	if err != nil {
		return err
	}

	// 1. Get File from ARI
	//src := ari.NewKey(ari.StoredRecordingKey, task.StorageID)
	src := ari.NewKey(ari.StoredRecordingKey, strconv.Itoa(task.ID))

	data, err := (*client).StoredRecording().File(src)
	if err != nil {
		s.db.Exec("UPDATE recordings SET relocation_attempts = relocation_attempts + 1, status='FAILED' WHERE id = ?", task.ID)
		return fmt.Errorf("failed to get file from ARI: %w", err)
	}

	// 2. Optional Trimming
	if task.Trim == "true" {
		// logic for trimming silence
	}


	if task.CreateAISummary {
		fmt.Printf("Generating AI summary for Recording ID: %d\n", task.ID)
		summary, err := s.generateAISummary(task.ID, data)
		if err != nil {
			fmt.Printf("Failed to generate AI summary for Recording ID: %d, error: %v\n", task.ID, err)
		} else {
            // Save the summary results to our new tables
            if err := s.saveSummaryToDB(task.ID, summary); err != nil {
                fmt.Printf("Failed to save summary to database: %v\n", err)
            }
        }
	}

	// 3. Upload to S3
	filename := fmt.Sprintf("%s.wav", task.StorageID)
	s3Url, err := s.uploadToS3(data, filename)
	if err != nil {
		_, dbErr := s.db.Exec("UPDATE recordings SET relocation_attempts = relocation_attempts + 1, status='FAILED' WHERE id = ?", task.ID)
		if dbErr != nil {
			fmt.Printf("failed to update database status to FAILED: %v\n", dbErr)
		}

		return err
	}

	// 4. Update Database
	_, err = s.db.Exec("UPDATE recordings SET s3_url = ?, status='FINALIZED' WHERE id = ?", s3Url, task.ID)
	if err != nil {
		fmt.Printf("failed to update database status to FINALIZED: %v\n", err)
		return fmt.Errorf("failed to update database: %w", err)
	}

	// 5. Cleanup ARI
	err = (*s.ariClient).StoredRecording().Delete(src)
	if err != nil {
		fmt.Printf("failed to delete recording from ARI: %v\n", err)
		return fmt.Errorf("failed to delete recording from ARI: %w", err)
	}

	fmt.Printf("Successfully processed recording ID: %d, S3 URL: %s\n", task.ID, s3Url)
	return nil
}

func (s *RecordingService) generateAISummary(callid int, rawwavdata []byte) (*CallSummary, error) {
    apiKey := s.settings.Credentials["anthropic_api_key"]
    if apiKey == "" {
        return nil, fmt.Errorf("anthropic API key not configured")
    }

    base64Audio := base64.StdEncoding.EncodeToString(rawwavdata)

    prompt := `Analyze the provided audio recording of a call. Extract and format the information as a strict JSON object with this exact structure:
{
  "speakers": [{"speaker_name": "string", "start_talk_time": 0.0, "end_talk_time": 0.0}],
  "chapters": [{"title": "string", "summary": "string", "start_time": 0.0}],
  "action_items": [{"speaker_name": "string", "action_item": "string", "status": "pending|completed|cancelled", "priority": "low|medium|high"}]
}
Respond ONLY with the JSON object.`

    payload := map[string]interface{}{
        "model":      "claude-3-5-sonnet-20241022",
        "max_tokens": 4096,
        "messages": []map[string]interface{}{
            {
                "role": "user",
                "content": []map[string]interface{}{
                    {
                        "type": "audio",
                        "source": map[string]string{
                            "type":       "base64",
                            "media_type": "audio/wav",
                            "data":       base64Audio,
                        },
                    },
                    {
                        "type": "text",
                        "text": prompt,
                    },
                },
            },
        },
    }

    jsonData, err := json.Marshal(payload)
    if err != nil {
        return nil, err
    }

    req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewBuffer(jsonData))
    if err != nil {
        return nil, err
    }

    req.Header.Set("x-api-key", apiKey)
    req.Header.Set("anthropic-version", "2023-06-01")
    req.Header.Set("content-type", "application/json")

    client := &http.Client{}
    resp, err := client.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    bodyBytes, err := io.ReadAll(resp.Body)
    if err != nil {
        return nil, err
    }

    if resp.StatusCode != http.StatusOK {
        return nil, fmt.Errorf("anthropic api error: %s", string(bodyBytes))
    }

    var anthropicResp struct {
        Content []struct {
            Text string `json:"text"`
        } `json:"content"`
    }

    if err := json.Unmarshal(bodyBytes, &anthropicResp); err != nil {
        return nil, err
    }

    if len(anthropicResp.Content) == 0 {
        return nil, fmt.Errorf("no content in response")
    }

    var summary CallSummary
    // The response is JSON text inside the message content
    if err := json.Unmarshal([]byte(anthropicResp.Content[0].Text), &summary); err != nil {
        return nil, fmt.Errorf("failed to parse summary JSON: %v", err)
    }

    return &summary, nil
}

// saveSummaryToDB handles the heavy lifting of inserting into 3 different tables
func (s *RecordingService) saveSummaryToDB(callID int, summary *CallSummary) error {
    tx, err := s.db.Begin()
    if err != nil {
        return err
    }

    // Defer a rollback in case of failure. 
    // If tx.Commit() is called first, the rollback does nothing.
    defer tx.Rollback()

    // 1. Save Speakers and map their names to the new DB IDs
    speakerMap := make(map[string]int64)
    for _, speaker := range summary.Speakers {
        res, err := tx.Exec(`
            INSERT INTO call_speakers (call_id, speaker_name, start_talk_time, end_talk_time) 
            VALUES (?, ?, ?, ?)`,
            callID, speaker.SpeakerName, speaker.StartTalkTime, speaker.EndTalkTime)
        if err != nil {
            return fmt.Errorf("error inserting speaker %s: %w", speaker.SpeakerName, err)
        }

        lastID, _ := res.LastInsertId()
        speakerMap[speaker.SpeakerName] = lastID
    }

    // 2. Save Chapters
    for _, chapter := range summary.Chapters {
        _, err := tx.Exec(`
            INSERT INTO call_chapters (call_id, title, summary, start_time) 
            VALUES (?, ?, ?, ?)`,
            callID, chapter.Title, chapter.Summary, chapter.StartTime)
        if err != nil {
            return fmt.Errorf("error inserting chapter %s: %w", chapter.Title, err)
        }
    }

    // 3. Save Action Items
    for _, item := range summary.ActionItems {
        // Find the speaker ID from our map, or use nil if name doesn't match
        var speakerID sql.NullInt64
        if id, ok := speakerMap[item.SpeakerName]; ok {
            speakerID.Int64 = id
            speakerID.Valid = true
        }

        _, err := tx.Exec(`
            INSERT INTO call_action_items (call_id, speaker_id, action_item, status, priority) 
            VALUES (?, ?, ?, ?, ?)`,
            callID, speakerID, item.ActionItem, item.Status, item.Priority)
        if err != nil {
            return fmt.Errorf("error inserting action item: %w", err)
        }
    }

    return tx.Commit()
}

func (s *RecordingService) uploadToS3(data []byte, filename string) (string, error) {
	fmt.Printf("Uploading file %s to S3\n", filename)
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

	fmt.Printf("File uploaded to S3: %s\n", result.Location)
	return aws.StringValue(&result.Location), nil
}