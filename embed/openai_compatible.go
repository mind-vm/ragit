package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is EdenAI's EU-region gateway — keeps embedding
	// requests in-region for data residency.
	DefaultBaseURL = "https://api.eu.edenai.run/v3"
	// DefaultModel proxies gemini-embedding-001 without needing a direct
	// GCP setup.
	DefaultModel = "google/gemini-embedding-001"
	// DefaultDimension is the pgvector width this package's default
	// configuration targets.
	DefaultDimension = 1536

	defaultProvider = "edenai"
)

// OpenAICompatibleConfig configures Client. BaseURL/Model default to
// EdenAI's EU gateway and gemini-embedding-001 when unset.
type OpenAICompatibleConfig struct {
	APIKey  string
	BaseURL string
	Model   string
	// Provider is a free-form label used only in Fingerprint (e.g. "edenai",
	// "openai") — not sent on the wire.
	Provider string
	// Dimension is the target pgvector width. Defaults to DefaultDimension.
	Dimension int
	// HTTPClient overrides the default client (2 minute timeout).
	HTTPClient *http.Client
}

// Client embeds text via an OpenAI-wire-compatible /embeddings endpoint:
// request {model, input, dimensions, encoding_format}, response
// {data: [{index, embedding}]}.
//
// Not every backend honors the `dimensions` field reliably — EdenAI, for
// gemini-embedding-001, always returns the native 3072-wide vector
// regardless of what was requested. When the response is wider than the
// configured Dimension, Client Matryoshka-truncates and L2-renormalizes to
// fit — valid for MRL-trained embeddings like Gemini's. A narrower-than-
// requested response is a hard error: there's no safe way to pad it back to
// more information.
type Client struct {
	apiKey    string
	baseURL   string
	model     string
	provider  string
	dimension int
	http      *http.Client
}

var _ Embedder = (*Client)(nil)

// NewOpenAICompatible builds a Client. Errors if the API key is missing.
func NewOpenAICompatible(cfg OpenAICompatibleConfig) (*Client, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("embed: API key is required")
	}
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	model := cfg.Model
	if model == "" {
		model = DefaultModel
	}
	provider := cfg.Provider
	if provider == "" {
		provider = defaultProvider
	}
	dim := cfg.Dimension
	if dim <= 0 {
		dim = DefaultDimension
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	return &Client{
		apiKey:    cfg.APIKey,
		baseURL:   baseURL,
		model:     model,
		provider:  provider,
		dimension: dim,
		http:      hc,
	}, nil
}

func (c *Client) Provider() string { return c.provider }
func (c *Client) Model() string    { return c.model }
func (c *Client) Dimension() int   { return c.dimension }

type embedRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	Dimensions     int      `json:"dimensions,omitempty"`
	EncodingFormat string   `json:"encoding_format,omitempty"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed embeds a batch of texts. The wire format has no query/document
// task-type distinction (unlike some native provider SDKs), so this is
// symmetric: the same call embeds both queries and documents.
func (c *Client) Embed(ctx context.Context, texts []string) ([]Vector, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	payload, err := json.Marshal(embedRequest{
		Model:          c.model,
		Input:          texts,
		Dimensions:     c.dimension,
		EncodingFormat: "float",
	})
	if err != nil {
		return nil, fmt.Errorf("embed: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("embed: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("embed: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var decoded embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("embed: decode response: %w", err)
	}
	if len(decoded.Data) != len(texts) {
		return nil, fmt.Errorf("embed: expected %d embeddings, got %d", len(texts), len(decoded.Data))
	}

	out := make([]Vector, len(texts))
	for _, d := range decoded.Data {
		if d.Index < 0 || d.Index >= len(texts) {
			return nil, fmt.Errorf("embed: embedding index %d out of range", d.Index)
		}
		vec, err := c.fitDimension(d.Embedding)
		if err != nil {
			return nil, err
		}
		out[d.Index] = vec
	}
	return out, nil
}

// fitDimension brings a provider embedding to the configured width: an
// exact-length vector passes through, a longer one is truncated and
// renormalized, a shorter one is an error.
func (c *Client) fitDimension(raw []float32) (Vector, error) {
	switch {
	case len(raw) == c.dimension:
		vec := make(Vector, len(raw))
		copy(vec, raw)
		return vec, nil
	case len(raw) > c.dimension:
		return truncateAndRenormalize(raw, c.dimension), nil
	default:
		return nil, fmt.Errorf("embed: got %d dims, want %d (cannot pad)", len(raw), c.dimension)
	}
}

// truncateAndRenormalize keeps the first dim components — a valid
// lower-dimension embedding for Matryoshka-trained models — and
// renormalizes to unit length so cosine distance stays comparable.
func truncateAndRenormalize(raw []float32, dim int) Vector {
	head := raw[:dim]
	var sumSq float64
	for _, x := range head {
		sumSq += float64(x) * float64(x)
	}
	norm := math.Sqrt(sumSq)
	out := make(Vector, dim)
	if norm == 0 {
		copy(out, head)
		return out
	}
	for i, x := range head {
		out[i] = float32(float64(x) / norm)
	}
	return out
}
