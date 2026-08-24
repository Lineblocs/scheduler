package storage

import (
    "bytes"
    "context"
    "database/sql"
    "encoding/base64"
    "encoding/binary"
    "encoding/json"
    "fmt"
    "io"
    "math"
    "net/http"
    "strconv"

    "lineblocs.com/scheduler/models"

    "github.com/CyCoreSystems/ari/v5"
    "github.com/aws/aws-sdk-go/aws"
    "github.com/aws/aws-sdk-go/aws/credentials"
    "github.com/aws/aws-sdk-go/aws/session"
    "github.com/aws/aws-sdk-go/service/s3/s3manager"
    helpers "github.com/Lineblocs/go-helpers"
    "github.com/google/generative-ai-go/genai"
    "google.golang.org/api/option"
)

type RecordingService struct {
    db        *sql.DB
    ariClient *ari.Client
    settings  *helpers.APICredentials // Shared settings model
}

// CallSpeaker captures a speaker segment within an AI-generated call summary.
type CallSpeaker struct {
    SpeakerName   string  `json:"speaker_name"`
    StartTalkTime float64 `json:"start_talk_time"`
    EndTalkTime   float64 `json:"end_talk_time"`
}

// CallChapter captures a single chapter in a summarized call.
type CallChapter struct {
    Title     string  `json:"title"`
    Summary   string  `json:"summary"`
    StartTime float64 `json:"start_time"`
}

// CallActionItem tracks a follow-up action discussed during the call.
type CallActionItem struct {
    SpeakerName string `json:"speaker_name"`
    ActionItem  string `json:"action_item"`
    Status      string `json:"status"`
    Priority    string `json:"priority"`
}

// CallSummary is the expected AI summary payload returned for a call.
type CallSummary struct {
    Speakers    []CallSpeaker    `json:"speakers"`
    Chapters    []CallChapter    `json:"chapters"`
    ActionItems []CallActionItem `json:"action_items"`
}

// CallAnalyticsData represents the structure matching call_ai_analytics schema.
type CallAnalyticsData struct {
    CallID           uint32  `json:"call_id"`
    WorkspaceID      uint32  `json:"workspace_id"`
    OverallSentiment string  `json:"overall_sentiment"`
    SentimentScore   float64 `json:"sentiment_score"`
    AgentTalkTime    uint32  `json:"agent_talk_time"`
    CallerTalkTime   uint32  `json:"caller_talk_time"`
    SilenceTime      uint32  `json:"silence_time"`
    OverlapTime      uint32  `json:"overlap_time"`
    Summary          string  `json:"summary"`
    KeywordsDetected string  `json:"keywords_detected"`
}

// CallQualityMetrics contains network and audio quality measurements for a call.
// Pointer fields allow unavailable measurements to be stored as SQL NULL.
type CallQualityMetrics struct {
    MOSScore      *float64
    JitterMSAvg   *float64
    JitterMSMax   *float64
    PacketLossPct *float64
    RTTMS         *int
    AudioCodec    *string
    UserAgent     *string
}

// CallAnalytics holds the AI-derived analytics for a call, mapped to the
// call_ai_analytics table.
type CallAnalytics struct {
    OverallSentiment string
    SentimentScore   float64
    AgentTalkTime    int
    CallerTalkTime   int
    SilenceTime      int
    OverlapTime      int
    Summary          string
    KeywordsDetected string
}

// WAVHeader holds standard RIFF header metadata.
type WAVHeader struct {
    ChunkID       [4]byte
    ChunkSize     uint32
    Format        [4]byte
    Subchunk1ID   [4]byte
    Subchunk1Size uint32
    AudioFormat   uint16
    NumChannels   uint16
    SampleRate    uint32
    ByteRate      uint32
    BlockAlign    uint16
    BitsPerSample uint16
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

    client, err := s.retrieveARIConnection(task.StorageServerIP)
    if err != nil {
        return err
    }

    // 1. Get File from ARI
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
        // Run AI summary generation in a separate goroutine so it doesn't block the main processing flow
        go s.processAISummary(task)
    }

    if task.GenerateCallAnalytics {
        // Run call analytics generation in a separate goroutine so it doesn't block the main processing flow
        go s.processCallAnalytics(task)
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
    _, err = s.db.Exec("UPDATE recordings SET s3_url = ?, s3_key = ?, status='FINALIZED' WHERE id = ?", s3Url, filename, task.ID)
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

func (s *RecordingService) processAISummary(task models.RecordingTask) error {
    fmt.Printf("Generating AI summary for Recording ID: %d\n", task.ID)
    var rawwavdata []byte
    summary, err := s.generateAISummary(task.ID, rawwavdata)
    if err != nil {
        fmt.Printf("Failed to generate AI summary for Recording ID: %d, error: %v\n", task.ID, err)
    } else {
        // Save the summary results to our new tables
        if err := s.saveSummaryToDB(task.ID, summary); err != nil {
            fmt.Printf("Failed to save summary to database: %v\n", err)
        }
    }

    return nil
}

func (s *RecordingService) processCallAnalytics(task models.RecordingTask) error {
    fmt.Printf("Generating call analytics for Recording ID: %d\n", task.ID)

    analytics, err := s.generateCallAnalytics(task.ID)
    if err != nil {
        fmt.Printf("Failed to generate call analytics for Recording ID: %d, error: %v\n", task.ID, err)
        return err
    }

    if err := s.saveCallAnalyticsToDB(task, analytics); err != nil {
        fmt.Printf("Failed to save call analytics to database: %v\n", err)
        return err
    }

    if err := s.saveCallQualityMetricsToDB(task); err != nil {
        fmt.Printf("Failed to save call quality metrics to database: %v\n", err)
        return err
    }

    return nil
}

func (s *RecordingService) saveCallQualityMetricsToDB(task models.RecordingTask) error {
    metrics := CallQualityMetrics{}
    _, err := s.db.Exec(`
        INSERT INTO call_quality_metrics
            (call_id, workspace_id, mos_score, jitter_ms_avg, jitter_ms_max,
             packet_loss_pct, rtt_ms, audio_codec, user_agent, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NOW(), NOW())`,
        task.ID, task.WorkspaceID, metrics.MOSScore, metrics.JitterMSAvg,
        metrics.JitterMSMax, metrics.PacketLossPct, metrics.RTTMS,
        metrics.AudioCodec, metrics.UserAgent,
    )
    if err != nil {
        return fmt.Errorf("failed to save call quality metrics: %w", err)
    }

    return nil
}

func (s *RecordingService) generateCallAnalytics(callid int) (*CallAnalytics, error) {
    apiKey := s.settings.Credentials["anthropic_api_key"]
    if apiKey == "" {
        return nil, fmt.Errorf("anthropic API key not configured")
    }

    analytics := &CallAnalytics{
        OverallSentiment: "neutral",
        SentimentScore:   0,
        AgentTalkTime:    0,
        CallerTalkTime:   0,
        SilenceTime:      0,
        OverlapTime:      0,
        Summary:          "",
        KeywordsDetected: "",
    }

    return analytics, nil
}

func (s *RecordingService) saveCallAnalyticsToDB(task models.RecordingTask, analytics *CallAnalytics) error {
    _, err := s.db.Exec(`
        INSERT INTO call_ai_analytics
            (call_id, workspace_id, overall_sentiment, sentiment_score, agent_talk_time, caller_talk_time, silence_time, overlap_time, summary, keywords_detected, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW(), NOW())
        ON DUPLICATE KEY UPDATE
            overall_sentiment = VALUES(overall_sentiment),
            sentiment_score = VALUES(sentiment_score),
            agent_talk_time = VALUES(agent_talk_time),
            caller_talk_time = VALUES(caller_talk_time),
            silence_time = VALUES(silence_time),
            overlap_time = VALUES(overlap_time),
            summary = VALUES(summary),
            keywords_detected = VALUES(keywords_detected),
            updated_at = NOW()`,
        task.ID, task.WorkspaceID, analytics.OverallSentiment, analytics.SentimentScore,
        analytics.AgentTalkTime, analytics.CallerTalkTime, analytics.SilenceTime, analytics.OverlapTime,
        analytics.Summary, analytics.KeywordsDetected,
    )
    if err != nil {
        return fmt.Errorf("failed to save call analytics: %w", err)
    }

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
    if err := json.Unmarshal([]byte(anthropicResp.Content[0].Text), &summary); err != nil {
        return nil, fmt.Errorf("failed to parse summary JSON: %v", err)
    }

    return &summary, nil
}

func (s *RecordingService) generateAISummaryWithGemini(callid int, rawwavdata []byte) (*CallSummary, error) {
    apiKey := s.settings.Credentials["gemini_api_key"]
    if apiKey == "" {
        return nil, fmt.Errorf("gemini API key not configured")
    }

    base64Audio := base64.StdEncoding.EncodeToString(rawwavdata)

    payload := map[string]interface{}{
        "contents": []map[string]interface{}{
            {
                "parts": []map[string]interface{}{
                    {
                        "inline_data": map[string]string{
                            "mime_type": "audio/wav",
                            "data":      base64Audio,
                        },
                    },
                    {
                        "text": "Analyze the provided audio recording of a call. Extract and format the information as a JSON object.",
                    },
                },
            },
        },
        "generationConfig": map[string]interface{}{
            "response_mime_type": "application/json",
            "candidate_count":    1,
            "max_output_tokens":  4096,
        },
    }

    jsonData, err := json.Marshal(payload)
    if err != nil {
        return nil, err
    }

    apiURL := "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-pro:generateContent?key=" + apiKey

    req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonData))
    if err != nil {
        return nil, err
    }
    req.Header.Set("Content-Type", "application/json")

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
        return nil, fmt.Errorf("gemini api error (status %d): %s", resp.StatusCode, string(bodyBytes))
    }

    var geminiResp struct {
        Candidates []struct {
            Content struct {
                Parts []struct {
                    Text string `json:"text"`
                } `json:"parts"`
            } `json:"content"`
        } `json:"candidates"`
    }

    if err := json.Unmarshal(bodyBytes, &geminiResp); err != nil {
        return nil, err
    }

    if len(geminiResp.Candidates) == 0 || len(geminiResp.Candidates[0].Content.Parts) == 0 {
        return nil, fmt.Errorf("no content in gemini response")
    }

    responseText := geminiResp.Candidates[0].Content.Parts[0].Text

    var summary CallSummary
    if err := json.Unmarshal([]byte(responseText), &summary); err != nil {
        return nil, fmt.Errorf("failed to parse summary JSON: %v", err)
    }

    return &summary, nil
}

func (s *RecordingService) saveSummaryToDB(callID int, summary *CallSummary) error {
    tx, err := s.db.Begin()
    if err != nil {
        return err
    }

    defer tx.Rollback()

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

    for _, chapter := range summary.Chapters {
        _, err := tx.Exec(`
            INSERT INTO call_chapters (call_id, title, summary, start_time) 
            VALUES (?, ?, ?, ?)`,
            callID, chapter.Title, chapter.Summary, chapter.StartTime)
        if err != nil {
            return fmt.Errorf("error inserting chapter %s: %w", chapter.Title, err)
        }
    }

    for _, item := range summary.ActionItems {
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

func (s *RecordingService) ProcessCallWAV(rawWavData []byte, callID uint32, workspaceID uint32) (map[string]interface{}, error) {
    reader := bytes.NewReader(rawWavData)

    var header WAVHeader
    if err := binary.Read(reader, binary.LittleEndian, &header); err != nil {
        return nil, fmt.Errorf("failed to read wav header: %w", err)
    }

    if string(header.ChunkID[:]) != "RIFF" || string(header.Format[:]) != "WAVE" {
        return nil, fmt.Errorf("invalid wav file format")
    }

    if err := s.seekToDataChunk(reader); err != nil {
        return nil, fmt.Errorf("failed to locate data chunk: %w", err)
    }

    const speechThreshold = 500.0

    var (
        agentSpeechSamples  uint64
        callerSpeechSamples uint64
        overlapSamples      uint64
        silenceSamples      uint64
    )

    numChannels := int(header.NumChannels)
    bytesPerSample := int(header.BitsPerSample / 8)
    frameSize := numChannels * bytesPerSample
    frameSamples := int(header.SampleRate / 50)
    buffer := make([]byte, frameSamples*frameSize)

    for {
        bytesRead, err := reader.Read(buffer)
        if err != nil && err != io.EOF {
            return nil, fmt.Errorf("error reading audio stream: %w", err)
        }
        if bytesRead == 0 {
            break
        }

        framesInChunk := bytesRead / frameSize
        var agentSumSq, callerSumSq float64
        sampleCount := 0

        for i := 0; i < framesInChunk*frameSize; i += frameSize {
            agentSample := int16(binary.LittleEndian.Uint16(buffer[i : i+2]))
            agentSumSq += float64(agentSample) * float64(agentSample)

            var callerSample int16
            if numChannels >= 2 {
                callerSample = int16(binary.LittleEndian.Uint16(buffer[i+2 : i+4]))
            } else {
                callerSample = agentSample
            }
            callerSumSq += float64(callerSample) * float64(callerSample)
            sampleCount++
        }

        if sampleCount == 0 {
            continue
        }

        agentRMS := math.Sqrt(agentSumSq / float64(sampleCount))
        callerRMS := math.Sqrt(callerSumSq / float64(sampleCount))

        agentSpeaking := agentRMS > speechThreshold
        callerSpeaking := callerRMS > speechThreshold

        if agentSpeaking && callerSpeaking {
            overlapSamples += uint64(sampleCount)
            agentSpeechSamples += uint64(sampleCount)
            callerSpeechSamples += uint64(sampleCount)
        } else if agentSpeaking {
            agentSpeechSamples += uint64(sampleCount)
        } else if callerSpeaking {
            callerSpeechSamples += uint64(sampleCount)
        } else {
            silenceSamples += uint64(sampleCount)
        }
    }

    sampleRate := float64(header.SampleRate)
    agentTalkTime := uint32(math.Round(float64(agentSpeechSamples) / sampleRate))
    callerTalkTime := uint32(math.Round(float64(callerSpeechSamples) / sampleRate))
    overlapTime := uint32(math.Round(float64(overlapSamples) / sampleRate))
    silenceTime := uint32(math.Round(float64(silenceSamples) / sampleRate))

    keywords, _ := json.Marshal([]string{"pricing", "support", "billing"})
    analytics := CallAnalyticsData{
        CallID:           callID,
        WorkspaceID:      workspaceID,
        OverallSentiment: "neutral",
        SentimentScore:   0.000,
        AgentTalkTime:    agentTalkTime,
        CallerTalkTime:   callerTalkTime,
        SilenceTime:      silenceTime,
        OverlapTime:      overlapTime,
        Summary:          "Call completed.",
        KeywordsDetected: string(keywords),
    }

    return s.structToMap(analytics)
}

func (s *RecordingService) seekToDataChunk(reader *bytes.Reader) error {
    buf := make([]byte, 4)
    for {
        if _, err := reader.Read(buf); err != nil {
            return err
        }
        if string(buf) == "data" {
            reader.Seek(4, io.SeekCurrent)
            return nil
        }
        reader.Seek(-3, io.SeekCurrent)
    }
}

func (s *RecordingService) structToMap(obj interface{}) (map[string]interface{}, error) {
    data, err := json.Marshal(obj)
    if err != nil {
        return nil, err
    }
    var res map[string]interface{}
    if err := json.Unmarshal(data, &res); err != nil {
        return nil, err
    }
    return res, nil
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

// AnalyzeAudioBuffer processes a raw WAV byte slice using the Gemini API SDK and returns a structured CallSummary.
func (s *RecordingService) AnalyzeAudioBuffer(ctx context.Context, wavBuffer []byte) (*CallSummary, error) {
    apiCreds, err := helpers.GetAPICredentials()
    if err != nil {
        panic(fmt.Sprintf("Critical: Could not load API credentials: %v", err))
    }

    apiKey := apiCreds.Credentials["gemini_api_key"]
    if apiKey == "" {
        return nil, fmt.Errorf("gemini API key not configured")
    }

    client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
    if err != nil {
        return nil, fmt.Errorf("failed to create client: %w", err)
    }
    defer client.Close()

    uploadOptions := &genai.UploadFileOptions{
        MIMEType: "audio/wav",
    }
    
    fileData, err := client.UploadFile(ctx, "", bytes.NewReader(wavBuffer), uploadOptions)
    if err != nil {
        return nil, fmt.Errorf("failed to upload audio buffer: %w", err)
    }
    defer client.DeleteFile(ctx, fileData.Name)

    analysisSchema := &genai.Schema{
        Type: genai.TypeObject,
        Properties: map[string]*genai.Schema{
            "speakers": {
                Type: genai.TypeArray,
                Items: &genai.Schema{
                    Type: genai.TypeObject,
                    Properties: map[string]*genai.Schema{
                        "speaker_name":    {Type: genai.TypeString},
                        "start_talk_time": {Type: genai.TypeNumber},
                        "end_talk_time":   {Type: genai.TypeNumber},
                    },
                    Required: []string{"speaker_name", "start_talk_time", "end_talk_time"},
                },
            },
            "chapters": {
                Type: genai.TypeArray,
                Items: &genai.Schema{
                    Type: genai.TypeObject,
                    Properties: map[string]*genai.Schema{
                        "title":      {Type: genai.TypeString},
                        "summary":    {Type: genai.TypeString},
                        "start_time": {Type: genai.TypeNumber},
                    },
                    Required: []string{"title", "summary", "start_time"},
                },
            },
            "action_items": {
                Type: genai.TypeArray,
                Items: &genai.Schema{
                    Type: genai.TypeObject,
                    Properties: map[string]*genai.Schema{
                        "speaker_name": {Type: genai.TypeString},
                        "action_item":  {Type: genai.TypeString},
                        "status":       {Type: genai.TypeString, Enum: []string{"pending", "completed", "cancelled"}},
                        "priority":     {Type: genai.TypeString, Enum: []string{"low", "medium", "high"}},
                    },
                    Required: []string{"speaker_name", "action_item", "status", "priority"},
                },
            },
        },
        Required: []string{"speakers", "chapters", "action_items"},
    }

    model := client.GenerativeModel("gemini-1.5-pro")
    model.ResponseMIMEType = "application/json"
    model.ResponseSchema = analysisSchema

    prompt := genai.Text("Analyze this audio file thoroughly. Extract: 1. All unique speakers along with their start and end talk time timestamps in float seconds. 2. Semantic chapter segmentations summarizing key conversational phases with their start times in float seconds. 3. Any concrete action items or outcomes, setting priority to low/medium/high and status to pending.")

    resp, err := model.GenerateContent(ctx, genai.FileData{URI: fileData.URI}, prompt)
    if err != nil {
        return nil, fmt.Errorf("error generating content: %w", err)
    }

    if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
        return nil, fmt.Errorf("no response received from Gemini API")
    }

    jsonText, ok := resp.Candidates[0].Content.Parts[0].(genai.Text)
    if !ok {
        return nil, fmt.Errorf("unexpected non-text response part")
    }

    var result CallSummary
    if err := json.Unmarshal([]byte(jsonText), &result); err != nil {
        return nil, fmt.Errorf("failed to unmarshal response JSON: %w", err)
    }

    return &result, nil
}