package semantic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Embedder turns one or more text strings into float32 embedding vectors.
type Embedder interface {
	Model() string
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
}

// OpenAIEmbedder calls any OpenAI-compatible /v1/embeddings endpoint
// (Ollama, llama.cpp server, HuggingFace TEI, FastEmbed, or OpenAI).
type OpenAIEmbedder struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
}

// NewOpenAIEmbedder constructs an OpenAI-compatible embeddings client.
func NewOpenAIEmbedder(baseURL, apiKey, model string) *OpenAIEmbedder {
	return &OpenAIEmbedder{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:  strings.TrimSpace(apiKey),
		model:   strings.TrimSpace(model),
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

func (e *OpenAIEmbedder) Model() string { return e.model }

type embeddingReq struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embeddingResp struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (e *OpenAIEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}

	rawReq, err := json.Marshal(embeddingReq{
		Model: e.model,
		Input: inputs,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal embedding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embeddings", bytes.NewReader(rawReq))
	if err != nil {
		return nil, e.redactErr(fmt.Errorf("build embedding request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, e.redactErr(fmt.Errorf("embedding request failed: %w", err))
	}
	defer resp.Body.Close()

	rawResp, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read embedding response: %w", err)
	}

	var parsed embeddingResp
	if err := json.Unmarshal(rawResp, &parsed); err != nil {
		return nil, fmt.Errorf("decode embedding response (http %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode >= 400 {
		msg := strings.TrimSpace(string(rawResp))
		if parsed.Error != nil && parsed.Error.Message != "" {
			msg = parsed.Error.Message
		}
		return nil, e.redactErr(fmt.Errorf("embedding api error (http %d): %s", resp.StatusCode, msg))
	}
	if len(parsed.Data) != len(inputs) {
		return nil, fmt.Errorf("embedding count mismatch: got %d, want %d", len(parsed.Data), len(inputs))
	}

	sort.Slice(parsed.Data, func(i, j int) bool {
		return parsed.Data[i].Index < parsed.Data[j].Index
	})

	out := make([][]float32, len(parsed.Data))
	for i, d := range parsed.Data {
		if len(d.Embedding) == 0 {
			return nil, fmt.Errorf("empty embedding vector at index %d", i)
		}
		out[i] = d.Embedding
	}
	return out, nil
}

func (e *OpenAIEmbedder) redactErr(err error) error {
	if err == nil || e.apiKey == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), e.apiKey, "<redacted>"))
}
