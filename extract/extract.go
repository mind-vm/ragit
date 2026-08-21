// Package extract turns raw document bytes into extracted text.
package extract

import (
	"context"
	"encoding/json"
)

// Result is what an Extractor produces from one document.
type Result struct {
	// Text is the extracted content, in Markdown.
	Text      string
	PageCount int
	// Metadata is the extractor's own structured output (detected tables,
	// warnings, source language, ...), stored verbatim rather than parsed
	// field-by-field so new fields don't require a schema migration.
	Metadata json.RawMessage
}

// Extractor turns document bytes into text. Implementations decide what a
// terminal, non-retryable failure looks like versus a transient one — see
// [XbergExtractor] and [ErrUnavailable].
type Extractor interface {
	Extract(ctx context.Context, data []byte, filename string) (*Result, error)
}
