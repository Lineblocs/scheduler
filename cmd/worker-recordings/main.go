package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"os"

	amqp "github.com/rabbitmq/amqp091-go"
	"lineblocs.com/scheduler/internal/storage"
	"lineblocs.com/scheduler/models"
	"lineblocs.com/scheduler/utils"
)

const (
	queueName = "recording_tasks"
)

// CallAnalyticsData represents the structure matching call_ai_analytics schema
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

// WAVHeader holds standard RIFF header metadata
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

// ProcessCallWAV analyzes an in-memory stereo WAV byte buffer and returns a map matching the DB table
func ProcessCallWAV(rawWavData []byte, callID uint32, workspaceID uint32) (map[string]interface{}, error) {
	reader := bytes.NewReader(rawWavData)

	// 1. Read WAV Header
	var header WAVHeader
	if err := binary.Read(reader, binary.LittleEndian, &header); err != nil {
		return nil, fmt.Errorf("failed to read wav header: %w", err)
	}

	// Verify RIFF/WAV format
	if string(header.ChunkID[:]) != "RIFF" || string(header.Format[:]) != "WAVE" {
		return nil, fmt.Errorf("invalid wav file format")
	}

	// Seek past header to the raw audio PCM data
	// (handling potential metadata chunks before 'data')
	if err := seekToDataChunk(reader); err != nil {
		return nil, fmt.Errorf("failed to locate data chunk: %w", err)
	}

	// 2. Analyze PCM Audio Frames
	// Energy threshold (RMS) for speech detection (16-bit PCM scale is -32768 to 32767)
	const speechThreshold = 500.0

	var (
		agentSpeechSamples  uint64
		callerSpeechSamples uint64
		overlapSamples      uint64
		silenceSamples      uint64
		totalFrames         uint64
	)

	numChannels := int(header.NumChannels)
	bytesPerSample := int(header.BitsPerSample / 8)
	frameSize := numChannels * bytesPerSample

	// Sample buffer for processing chunks (e.g., 20ms frames)
	frameSamples := int(header.SampleRate / 50) // 20ms frame size
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
		totalFrames += uint64(framesInChunk)

		var agentSumSq, callerSumSq float64
		sampleCount := 0

		for i := 0; i < framesInChunk*frameSize; i += frameSize {
			// Channel 1: Agent (Left)
			agentSample := int16(binary.LittleEndian.Uint16(buffer[i : i+2]))
			agentSumSq += float64(agentSample) * float64(agentSample)

			// Channel 2: Caller (Right) if stereo, else mirror channel 1
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

		// Calculate Root Mean Square (RMS) energy for 20ms block
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

	// 3. Convert Sample Counts to Durations (Seconds)
	sampleRate := float64(header.SampleRate)
	agentTalkTime := uint32(math.Round(float64(agentSpeechSamples) / sampleRate))
	callerTalkTime := uint32(math.Round(float64(callerSpeechSamples) / sampleRate))
	overlapTime := uint32(math.Round(float64(overlapSamples) / sampleRate))
	silenceTime := uint32(math.Round(float64(silenceSamples) / sampleRate))

	// 4. Placeholder / Integration hook for AI NLP (Speech-To-Text / Sentiment API)
	keywords, _ := json.Marshal([]string{"pricing", "support", "billing"})
	
	analytics := CallAnalyticsData{
		CallID:           callID,
		WorkspaceID:      workspaceID,
		OverallSentiment: "neutral", // Updated by STT/NLP engine
		SentimentScore:   0.000,
		AgentTalkTime:    agentTalkTime,
		CallerTalkTime:   callerTalkTime,
		SilenceTime:      silenceTime,
		OverlapTime:      overlapTime,
		Summary:          "Call completed.", // Updated by STT/NLP engine
		KeywordsDetected: string(keywords),
	}

	// 5. Convert Struct to map[string]interface{} for MySQL/GORM insert
	return structToMap(analytics)
}

// Helper to locate the PCM 'data' chunk in an in-memory WAV byte stream
func seekToDataChunk(reader *bytes.Reader) error {
	buf := make([]byte, 4)
	for {
		if _, err := reader.Read(buf); err != nil {
			return err
		}
		if string(buf) == "data" {
			// Skip chunk size uint32 (4 bytes)
			reader.Seek(4, io.SeekCurrent)
			return nil
		}
		// Skip byte-by-byte if non-aligned metadata header
		reader.Seek(-3, io.SeekCurrent)
	}
}

// Convert struct to map matching DB column names
func structToMap(obj interface{}) (map[string]interface{}, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var res map[string]interface{}
	err = json.Unmarshal(data, &res)
	return res, err
}

func main() {
	db, _ := utils.GetDBConnection()
	ariClient, _ := utils.CreateARIConnection()
	settings, _ := utils.GetSettingsFromAPI() // Centralized settings fetcher

	storageSvc := storage.NewRecordingService(db, ariClient, settings)

	conn, err := amqp.Dial(os.Getenv("QUEUE_URL"))
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		panic(err)
	}
	defer ch.Close()

	// Ensure queue exists
	q, err := ch.QueueDeclare(queueName, true, false, false, false, nil)
	if err != nil {
		panic(err)
	}

	msgs, _ := ch.Consume(q.Name, "", false, false, false, false, nil)

	log.Println("S3 Recording Worker Started...")

	for d := range msgs {
		var task models.RecordingTask
		if err := json.Unmarshal(d.Body, &task); err != nil {
			log.Printf("Error decoding task: %v", err)
			d.Ack(false) // Drop malformed messages
			continue
		}

		if err := storageSvc.ProcessRecording(task); err != nil {
			log.Printf("Worker failed to process recording %d: %v", task.ID, err)
			d.Ack(false)
			continue
		}

		d.Ack(false)
	}
}