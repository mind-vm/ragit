package extract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// DefaultTimeout bounds one extraction call. Generous because OCR on a
// scanned multi-page PDF is genuinely slow.
const DefaultTimeout = 120 * time.Second

// outputFormat asks xberg for Markdown rather than its plain-text default.
// Chunks are embedded and fed to a model as context, so retaining headings
// and table structure is worth the extra tokens.
const outputFormat = "markdown"

// ErrUnavailable marks a transport/deployment failure — connection refused,
// DNS, timeout, or a 5xx from the xberg service itself. It is the only case
// worth retrying or falling back on; an extraction xberg *rejected* (4xx) is
// a verdict on the document and is returned as a plain error instead.
var ErrUnavailable = errors.New("xberg service unavailable")

// XbergExtractor extracts documents through an xberg REST server
// (`xberg serve`). See https://docs.xberg.io.
type XbergExtractor struct {
	// BaseURL is the xberg server root, e.g. http://xberg:8000.
	BaseURL string
	// HTTPClient is used for every call. Nil builds a client from Timeout.
	HTTPClient *http.Client
	// Timeout bounds a single extraction. Zero means DefaultTimeout. Ignored
	// when HTTPClient is set.
	Timeout time.Duration
}

var _ Extractor = (*XbergExtractor)(nil)

// NewXbergExtractor builds an extractor with production defaults.
func NewXbergExtractor(baseURL string, timeout time.Duration) *XbergExtractor {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &XbergExtractor{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Timeout: timeout,
	}
}

// response is the /extract response envelope.
type response struct {
	Results []result `json:"results"`
	Errors  []apiErr `json:"errors"`
}

type result struct {
	Content  string          `json:"content"`
	MimeType string          `json:"mime_type"`
	Metadata json.RawMessage `json:"metadata"`
}

// apiErr is both the per-file error entry in a 200 response and the
// top-level error body of a 4xx/5xx response.
type apiErr struct {
	ErrorType string `json:"error_type"`
	Message   string `json:"message"`
	Source    string `json:"source"`
	Code      int    `json:"code"`
}

func (e apiErr) String() string {
	switch {
	case e.Message != "" && (e.ErrorType == "" || e.ErrorType == "other"):
		return e.Message
	case e.ErrorType != "" && e.Message != "":
		return e.ErrorType + ": " + e.Message
	case e.Message != "":
		return e.Message
	case e.ErrorType != "":
		return e.ErrorType
	default:
		return "unknown extraction error"
	}
}

// metadata is the subset of the per-result metadata this package consumes.
// xberg returns a much wider bag; everything else is left in Result.Metadata
// verbatim rather than parsed here.
type metadata struct {
	PageCount int `json:"page_count"`
	Format    struct {
		PageCount int `json:"page_count"`
	} `json:"format"`
}

// pageCount returns the first page count present, or 0 when neither is —
// xberg puts it at metadata.format.page_count, with a top-level fallback in
// case a given deployment's version differs.
func (m metadata) pageCount() int {
	if m.Format.PageCount > 0 {
		return m.Format.PageCount
	}
	return m.PageCount
}

// Extract parses data via the xberg service.
func (e *XbergExtractor) Extract(ctx context.Context, data []byte, filename string) (*Result, error) {
	body, contentType, err := multipartBody(data, filename)
	if err != nil {
		return nil, fmt.Errorf("build xberg request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/extract", body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := e.client().Do(req)
	if err != nil {
		// Connection refused, DNS failure, client timeout — the service, not
		// the document.
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: read response: %v", ErrUnavailable, err)
	}

	if resp.StatusCode >= 500 {
		// The server broke, not the file.
		return nil, fmt.Errorf("%w: http %d: %s",
			ErrUnavailable, resp.StatusCode, truncate(strings.TrimSpace(string(payload)), 200))
	}
	if resp.StatusCode != http.StatusOK {
		// 400 validation / 422 parsing|OCR — a verdict on the document, not
		// a reason to retry.
		var e apiErr
		if json.Unmarshal(payload, &e) == nil && (e.Message != "" || e.ErrorType != "") {
			return nil, fmt.Errorf("xberg extraction failed: %s", e.String())
		}
		return nil, fmt.Errorf("xberg extraction failed (http %d): %s",
			resp.StatusCode, truncate(strings.TrimSpace(string(payload)), 200))
	}

	var decoded response
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("decode xberg response: %w", err)
	}
	if len(decoded.Results) == 0 {
		// A 200 can still carry a failed extraction — status code alone
		// never tells you it succeeded.
		if len(decoded.Errors) > 0 {
			return nil, fmt.Errorf("xberg extraction failed: %s", decoded.Errors[0].String())
		}
		return nil, fmt.Errorf("xberg returned no result for %s", filename)
	}

	first := decoded.Results[0]
	pageCount := 1
	if len(first.Metadata) > 0 {
		var m metadata
		if json.Unmarshal(first.Metadata, &m) == nil && m.pageCount() > 0 {
			pageCount = m.pageCount()
		}
	}

	return &Result{
		Text:      first.Content,
		PageCount: pageCount,
		Metadata:  first.Metadata,
	}, nil
}

// multipartBody builds the multipart body for a single-file /extract call.
func multipartBody(data []byte, filename string) (io.Reader, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	name := filepath.Base(filename)
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "document"
	}
	part, err := w.CreateFormFile("files", name)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(data); err != nil {
		return nil, "", err
	}
	if err := w.WriteField("output_format", outputFormat); err != nil {
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return &buf, w.FormDataContentType(), nil
}

func (e *XbergExtractor) client() *http.Client {
	if e.HTTPClient != nil {
		return e.HTTPClient
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &http.Client{Timeout: timeout}
}

// Health reports whether the xberg service answers. Meant to be called once
// at startup so a missing sidecar is a clear log line instead of a surprise
// on the first upload — it should never block boot.
func (e *XbergExtractor) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.BaseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := e.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health returned http %d", resp.StatusCode)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
