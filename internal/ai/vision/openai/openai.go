// Package openai implements image understanding against OpenAI-compatible
// chat completions endpoints, including Ollama's /v1/chat/completions.
package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/usememos/memos/internal/ai"
	"github.com/usememos/memos/internal/ai/vision"
)

const defaultEndpoint = "https://api.openai.com/v1"

// Analyzer calls an OpenAI-compatible multimodal chat endpoint.
type Analyzer struct {
	endpoint   string
	apiKey     string
	httpClient *http.Client
}

// New constructs an Analyzer from a provider config.
func New(cfg ai.ProviderConfig) (*Analyzer, error) {
	endpoint, err := normalizeEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	if cfg.APIKey == "" {
		return nil, errors.New("OpenAI API key is required")
	}
	return &Analyzer{endpoint: endpoint, apiKey: cfg.APIKey, httpClient: &http.Client{Timeout: 2 * time.Minute}}, nil
}

// Analyze extracts OCR text and visual descriptions from an image.
func (a *Analyzer) Analyze(ctx context.Context, req vision.Request) (*vision.Response, error) {
	if len(req.Image) == 0 {
		return nil, errors.New("image is required")
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, errors.New("vision model is required")
	}
	contentType := strings.TrimSpace(req.ContentType)
	if contentType == "" {
		contentType = "image/png"
	}
	instruction := defaultInstruction(req.Filename, req.Prompt)
	body := chatRequest{
		Model: req.Model,
		Messages: []chatMessage{{
			Role: "user",
			Content: []messagePart{
				{Type: "text", Text: instruction},
				{Type: "image_url", ImageURL: &imageURL{URL: "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(req.Image)}},
			},
		}},
		Temperature: floatPtr(0),
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, errors.Wrap(err, "marshal vision request")
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, errors.Wrap(err, "create vision request")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)

	httpResp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return nil, errors.Wrap(err, "call vision endpoint")
	}
	defer httpResp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, 8<<20))
	if err != nil {
		return nil, errors.Wrap(err, "read vision response")
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, errors.Errorf("vision endpoint returned %s: %s", httpResp.Status, strings.TrimSpace(string(respBody)))
	}

	var decoded chatResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return nil, errors.Wrap(err, "decode vision response")
	}
	if len(decoded.Choices) == 0 {
		return nil, errors.New("vision response contained no choices")
	}
	return parseStructuredContent(decoded.Choices[0].Message.Content)
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature *float32      `json:"temperature,omitempty"`
}

type chatMessage struct {
	Role    string        `json:"role"`
	Content []messagePart `json:"content"`
}

type messagePart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func defaultInstruction(filename, prompt string) string {
	base := `Extract searchable information from this image. Return JSON only with exactly these fields: "ocr_text" (all visible text, preserving useful line breaks), "caption" (one concise description), "tags" (short keywords), and "objects" (visible objects or document types). Do not include markdown.`
	if strings.TrimSpace(filename) != "" {
		base += " Filename: " + filename + "."
	}
	if strings.TrimSpace(prompt) != "" {
		base += " Additional instructions: " + strings.TrimSpace(prompt)
	}
	return base
}

func parseStructuredContent(content string) (*vision.Response, error) {
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)
	var resp vision.Response
	if err := json.Unmarshal([]byte(content), &resp); err != nil {
		return nil, errors.Wrap(err, "parse vision JSON content")
	}
	resp.OCRText = strings.TrimSpace(resp.OCRText)
	resp.Caption = strings.TrimSpace(resp.Caption)
	resp.Tags = compactStrings(resp.Tags)
	resp.Objects = compactStrings(resp.Objects)
	return &resp, nil
}

func compactStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func floatPtr(v float32) *float32 { return &v }

func normalizeEndpoint(endpoint string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	if _, err := url.ParseRequestURI(endpoint); err != nil {
		return "", errors.Wrap(err, "invalid OpenAI endpoint")
	}
	return strings.TrimRight(endpoint, "/"), nil
}

var _ vision.Analyzer = (*Analyzer)(nil)
