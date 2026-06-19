// Package vision defines image understanding for attachment search indexing.
package vision

import "context"

// Analyzer extracts searchable text from an image.
type Analyzer interface {
	Analyze(ctx context.Context, req Request) (*Response, error)
}

// Request is the input to an image understanding call.
type Request struct {
	Image       []byte
	ContentType string
	Filename    string
	Model       string
	Prompt      string
}

// Response is the structured searchable content extracted from an image.
type Response struct {
	OCRText string   `json:"ocr_text"`
	Caption string   `json:"caption"`
	Tags    []string `json:"tags"`
	Objects []string `json:"objects"`
}
