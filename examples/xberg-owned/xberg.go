package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mind-vm/ragit/embed"
)

// Client talks to xberg's /extract endpoint asking for everything at once:
// extraction, chunking, and an embedding per chunk.
//
// ragit's own extract.XbergExtractor deliberately asks for none of that — it
// requests Markdown and returns a flat string, because extract.Result has
// nowhere to put chunks. This is the same endpoint with a different config, and
// a separate client only because the library's Extractor interface cannot carry
// the extra output.
//
// The config shape is not in xberg's OpenAPI description: /extract's body is
// declared as bare multipart with no schema. These field names were recovered
// from the CLI and confirmed over HTTP. In particular it is `max_chars`, not
// the `max_characters` the published docs give — and a request using the
// documented name is accepted with the field silently ignored.
type Client struct {
	BaseURL string
	HTTP    *http.Client

	// MaxChars and Overlap mirror chunk.DefaultConfig(), so the two examples
	// differ in which chunker ran rather than in how it was tuned.
	MaxChars int
	Overlap  int

	// Counters live here rather than on the Embedder, and that placement is
	// itself a finding.
	//
	// The extract-only example counts embedding work by wrapping
	// embed.Embedder, because every vector in that pipeline goes through it.
	// Here the corpus is embedded *inside* extraction — the vectors arrive as
	// a side effect of /extract — so an Embedder decorator sees only the query
	// embeddings and reports near-zero however much work was really done.
	// Anything measuring embedding cost has to instrument the transport.
	extractCalls   atomic.Int64
	chunksEmbedded atomic.Int64
	queryEmbeds    atomic.Int64
}

// ExtractCalls is how many /extract round trips happened.
func (c *Client) ExtractCalls() int64 { return c.extractCalls.Load() }

// ChunksEmbedded is how many chunks came back carrying a vector.
func (c *Client) ChunksEmbedded() int64 { return c.chunksEmbedded.Load() }

// QueryEmbeds is how many of those calls were embedding a search query rather
// than indexing a document.
func (c *Client) QueryEmbeds() int64 { return c.queryEmbeds.Load() }

// NewClient builds a Client with ragit's own chunker settings.
func NewClient(baseURL string, maxChars, overlap int) *Client {
	return &Client{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		HTTP:     &http.Client{Timeout: 5 * time.Minute},
		MaxChars: maxChars,
		Overlap:  overlap,
	}
}

// EmbeddingSpace is where xberg's default local ONNX preset puts its vectors:
// BGE-base-en-v1.5 at 768 dimensions, the "balanced" preset.
//
// One value, used twice — it is what the corpus is written under and what the
// query embedder in queryembed.go reports. They cannot drift apart, which is
// the whole reason ragit takes an embed.Space rather than a fingerprint
// string: retrieval filters on that fingerprint, and a corpus written under
// one that disagrees with the query's returns nothing at all.
//
// Every field is still *asserted* rather than observed. xberg's response says
// nothing about which model produced the vectors — not in the chunk, not in
// the result metadata — so this is a promise this program makes on xberg's
// behalf. Change the preset and the fingerprint keeps claiming BGE-base while
// the corpus straddles two spaces looking like one. A struct does not fix
// that; only xberg reporting its model would.
var EmbeddingSpace = embed.Space{
	Provider:  "xberg",
	Model:     "bge-base-en-v1.5",
	Dimension: 768,
}

// PreparedChunk is a chunk that arrives already embedded.
//
// It is chunk.Chunk plus a vector — the shape ragit had no way to accept when
// this example was written, and the reason it existed. ragit.PreparedChunk is
// now that shape; this type survives as the wire-side one, because xberg's
// chunk_type has no home in ragit's and belongs in metadata. See ingest.go.
type PreparedChunk struct {
	Index       int
	Content     string
	HeadingPath []string
	Embedding   []float32
	// ChunkType is xberg's own classification ("heading", "text", …). ragit
	// has no column for it, so it rides in the chunk's metadata JSONB.
	ChunkType string
}

// Result is one document, extracted and chunked and embedded.
type Result struct {
	Content   string
	Metadata  json.RawMessage
	PageCount int
	Chunks    []PreparedChunk
}

// wire types mirror the parts of xberg's response this example reads.
type wireResponse struct {
	Results []wireResult `json:"results"`
	Errors  []wireError  `json:"errors"`
}

type wireResult struct {
	Content  string          `json:"content"`
	MimeType string          `json:"mime_type"`
	Metadata json.RawMessage `json:"metadata"`
	Chunks   []wireChunk     `json:"chunks"`
}

type wireChunk struct {
	Content   string        `json:"content"`
	ChunkType string        `json:"chunk_type"`
	Embedding []float32     `json:"embedding"`
	Metadata  wireChunkMeta `json:"metadata"`
}

type wireChunkMeta struct {
	ChunkIndex  int `json:"chunk_index"`
	TotalChunks int `json:"total_chunks"`
	// HeadingContext is richer than ragit's HeadingPath: a level and a text per
	// heading rather than a bare string. Only the text survives the mapping.
	HeadingContext struct {
		Headings []struct {
			Level int    `json:"level"`
			Text  string `json:"text"`
		} `json:"headings"`
	} `json:"heading_context"`
}

type wireError struct {
	ErrorType string `json:"error_type"`
	Message   string `json:"message"`
}

// extractConfig is the `config` form field.
type extractConfig struct {
	OutputFormat string         `json:"output_format"`
	Chunking     chunkingConfig `json:"chunking"`
}

type chunkingConfig struct {
	MaxChars    int    `json:"max_chars"`
	Overlap     int    `json:"overlap"`
	ChunkerType string `json:"chunker_type"`
	// Embedding present at all turns embeddings on, with the default local
	// ONNX preset. An empty object is the whole configuration — no model name,
	// no provider, no API key. The first call downloads the model.
	Embedding map[string]any `json:"embedding"`
}

// ExtractChunkEmbed runs the whole front half of the pipeline in one call.
func (c *Client) ExtractChunkEmbed(ctx context.Context, data []byte, filename string) (*Result, error) {
	cfg := extractConfig{
		OutputFormat: "markdown",
		Chunking: chunkingConfig{
			MaxChars:    c.MaxChars,
			Overlap:     c.Overlap,
			ChunkerType: "markdown",
			Embedding:   map[string]any{},
		},
	}
	return c.post(ctx, data, filename, cfg)
}

// EmbedText embeds a single string.
//
// There is no HTTP endpoint for this. xberg exposes embed_texts() as an
// in-process library function in its language bindings, and `xberg embed` as a
// CLI command, but the REST server offers neither — so the only way to reach
// the embedder over the wire is to post the text as a document and read the
// vector off the chunk that comes back.
//
// The chunk budget is set absurdly high so the text cannot split; a query that
// somehow produced two chunks would have no single vector to return, and this
// errors rather than guessing at an average.
func (c *Client) EmbedText(ctx context.Context, text string) ([]float32, error) {
	cfg := extractConfig{
		OutputFormat: "markdown",
		Chunking: chunkingConfig{
			MaxChars:    1_000_000,
			Overlap:     0,
			ChunkerType: "text",
			Embedding:   map[string]any{},
		},
	}
	c.queryEmbeds.Add(1)
	res, err := c.post(ctx, []byte(text), "query.txt", cfg)
	if err != nil {
		return nil, err
	}
	switch len(res.Chunks) {
	case 1:
		return res.Chunks[0].Embedding, nil
	case 0:
		return nil, fmt.Errorf("xberg returned no chunk for the query text")
	default:
		return nil, fmt.Errorf("xberg split the query into %d chunks; no single vector to use", len(res.Chunks))
	}
}

func (c *Client) post(ctx context.Context, data []byte, filename string, cfg extractConfig) (*Result, error) {
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("encode xberg config: %w", err)
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("files", filepath.Base(filename))
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(data); err != nil {
		return nil, err
	}
	if err := w.WriteField("config", string(encoded)); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/extract", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	c.extractCalls.Add(1)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("xberg unavailable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("xberg http %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	var decoded wireResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode xberg response: %w", err)
	}
	if len(decoded.Results) == 0 {
		if len(decoded.Errors) > 0 {
			return nil, fmt.Errorf("xberg extraction failed: %s %s",
				decoded.Errors[0].ErrorType, decoded.Errors[0].Message)
		}
		return nil, fmt.Errorf("xberg returned no result for %s", filename)
	}

	first := decoded.Results[0]
	out := &Result{
		Content:   first.Content,
		Metadata:  first.Metadata,
		PageCount: 1,
		Chunks:    make([]PreparedChunk, 0, len(first.Chunks)),
	}
	for i, ch := range first.Chunks {
		if len(ch.Embedding) == 0 {
			return nil, fmt.Errorf("chunk %d came back without an embedding; is chunking.embedding set?", i)
		}
		if len(ch.Embedding) != EmbeddingSpace.Dimension {
			return nil, fmt.Errorf("chunk %d has %d components, expected %d",
				i, len(ch.Embedding), EmbeddingSpace.Dimension)
		}
		headings := make([]string, 0, len(ch.Metadata.HeadingContext.Headings))
		for _, h := range ch.Metadata.HeadingContext.Headings {
			headings = append(headings, h.Text)
		}
		c.chunksEmbedded.Add(1)
		out.Chunks = append(out.Chunks, PreparedChunk{
			Index:       ch.Metadata.ChunkIndex,
			Content:     ch.Content,
			HeadingPath: headings,
			Embedding:   ch.Embedding,
			ChunkType:   ch.ChunkType,
		})
	}
	return out, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
