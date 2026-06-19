package vector

import (
	"strings"
	"sync"

	"github.com/pkg/errors"
	tiktoken "github.com/pkoukk/tiktoken-go"
	tiktokenloader "github.com/pkoukk/tiktoken-go-loader"
	"github.com/tmc/langchaingo/textsplitter"
)

const (
	defaultChunkSizeTokens    = 512
	defaultChunkOverlapTokens = 80
)

var (
	tokenizerLoaderOnce sync.Once

	chunkSeparators = []string{
		"\n# ", "\n## ", "\n### ", "\n\n", "\n",
		"。", "！", "？", "；", "，",
		". ", "! ", "? ", "; ", ", ", " ",
		"",
	}
)

// Chunk is one embedding unit for a memo.
type Chunk struct {
	Index int32
	Text  string
}

type chunker struct {
	splitter textsplitter.TextSplitter
}

func newChunker(model string) (*chunker, error) {
	enc, err := tokenEncoderForModel(model)
	if err != nil {
		return nil, err
	}
	splitter := textsplitter.NewRecursiveCharacter(
		textsplitter.WithChunkSize(defaultChunkSizeTokens),
		textsplitter.WithChunkOverlap(defaultChunkOverlapTokens),
		textsplitter.WithSeparators(chunkSeparators),
		textsplitter.WithKeepSeparator(true),
		textsplitter.WithLenFunc(func(s string) int {
			return len(enc.Encode(s, nil, nil))
		}),
	)
	return &chunker{splitter: splitter}, nil
}

func tokenEncoderForModel(model string) (*tiktoken.Tiktoken, error) {
	tokenizerLoaderOnce.Do(func() {
		tiktoken.SetBpeLoader(tiktokenloader.NewOfflineLoader())
	})
	if strings.TrimSpace(model) != "" {
		if enc, err := tiktoken.EncodingForModel(model); err == nil {
			return enc, nil
		}
	}
	enc, err := tiktoken.GetEncoding(tiktoken.MODEL_CL100K_BASE)
	if err != nil {
		return nil, errors.Wrap(err, "get fallback tokenizer encoding")
	}
	return enc, nil
}

func (c *chunker) split(text string) ([]Chunk, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	parts, err := c.splitter.SplitText(text)
	if err != nil {
		return nil, errors.Wrap(err, "split text into chunks")
	}
	chunks := make([]Chunk, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		chunks = append(chunks, Chunk{Index: int32(len(chunks)), Text: part})
	}
	return chunks, nil
}
