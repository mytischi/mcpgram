package whisper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// Transcriber converts raw audio/video bytes into text.
type Transcriber interface {
	Transcribe(ctx context.Context, filename string, data []byte) (string, error)
}

// OpenAIClient calls any OpenAI-compatible /v1/audio/transcriptions endpoint
// (faster-whisper-server / Speaches, whisper.cpp server, or OpenAI Whisper API).
type OpenAIClient struct {
	baseURL    string
	apiKey     string
	model      string
	language   string
	httpClient *http.Client
}

// NewOpenAIClient creates an OpenAI-compatible Whisper transcription client.
func NewOpenAIClient(baseURL, apiKey, model, language string) *OpenAIClient {
	return &OpenAIClient{
		baseURL:  strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:   strings.TrimSpace(apiKey),
		model:    strings.TrimSpace(model),
		language: strings.TrimSpace(language),
		httpClient: &http.Client{
			Timeout: 180 * time.Second,
		},
	}
}

func (c *OpenAIClient) Transcribe(ctx context.Context, filename string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", errors.New("empty audio payload")
	}
	if strings.TrimSpace(filename) == "" {
		filename = "voice.ogg"
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return "", fmt.Errorf("create multipart file field: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return "", fmt.Errorf("write multipart audio bytes: %w", err)
	}
	if err := writer.WriteField("model", c.model); err != nil {
		return "", fmt.Errorf("write model field: %w", err)
	}
	if c.language != "" {
		if err := writer.WriteField("language", c.language); err != nil {
			return "", fmt.Errorf("write language field: %w", err)
		}
	}
	if err := writer.WriteField("response_format", "json"); err != nil {
		return "", fmt.Errorf("write response_format field: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/audio/transcriptions", &body)
	if err != nil {
		return "", c.redactErr(fmt.Errorf("build whisper request: %w", err))
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", c.redactErr(fmt.Errorf("whisper request failed: %w", err))
	}
	defer resp.Body.Close()

	rawResp, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("read whisper response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return "", c.redactErr(fmt.Errorf("whisper api error (http %d): %s", resp.StatusCode, strings.TrimSpace(string(rawResp))))
	}

	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rawResp, &parsed); err != nil {
		// Some whisper servers return plain text even when json is requested.
		plain := strings.TrimSpace(string(rawResp))
		if plain != "" {
			return plain, nil
		}
		return "", fmt.Errorf("decode whisper response: %w", err)
	}
	return strings.TrimSpace(parsed.Text), nil
}

func (c *OpenAIClient) redactErr(err error) error {
	if err == nil || c.apiKey == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.apiKey, "<redacted>"))
}
