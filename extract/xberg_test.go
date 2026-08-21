package extract_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jryannel/ragit/extract"
)

func TestXbergExtractor_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/extract", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{
					"content":   "# Hello\n\nworld",
					"mime_type": "text/markdown",
					"metadata":  map[string]any{"format": map[string]any{"page_count": 3}},
				},
			},
			"errors": []any{},
		})
	}))
	defer server.Close()

	x := extract.NewXbergExtractor(server.URL, 0)
	res, err := x.Extract(context.Background(), []byte("fake bytes"), "doc.pdf")
	require.NoError(t, err)
	require.Equal(t, "# Hello\n\nworld", res.Text)
	require.Equal(t, 3, res.PageCount)
}

func TestXbergExtractor_200WithErrorsArray_IsAFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []any{},
			"errors": []map[string]any{
				{"error_type": "other", "message": "Parsing error: corrupt file"},
			},
		})
	}))
	defer server.Close()

	x := extract.NewXbergExtractor(server.URL, 0)
	_, err := x.Extract(context.Background(), []byte("bad"), "bad.pdf")
	require.Error(t, err)
	require.False(t, errors.Is(err, extract.ErrUnavailable), "a 200-with-errors[] response is a document failure, not ErrUnavailable")
	require.Contains(t, err.Error(), "corrupt file")
}

func TestXbergExtractor_5xx_IsErrUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer server.Close()

	x := extract.NewXbergExtractor(server.URL, 0)
	_, err := x.Extract(context.Background(), []byte("bytes"), "doc.pdf")
	require.Error(t, err)
	require.True(t, errors.Is(err, extract.ErrUnavailable))
}

func TestXbergExtractor_4xx_IsNotErrUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error_type": "OcrError",
			"message":    "no text layer and OCR is disabled",
		})
	}))
	defer server.Close()

	x := extract.NewXbergExtractor(server.URL, 0)
	_, err := x.Extract(context.Background(), []byte("bytes"), "scanned.pdf")
	require.Error(t, err)
	require.False(t, errors.Is(err, extract.ErrUnavailable), "a 4xx is a document verdict, not a reason to retry/fallback")
	require.Contains(t, err.Error(), "OCR is disabled")
}

func TestXbergExtractor_TransportFailure_IsErrUnavailable(t *testing.T) {
	x := extract.NewXbergExtractor("http://127.0.0.1:1", 0) // nothing listening
	_, err := x.Extract(context.Background(), []byte("bytes"), "doc.pdf")
	require.Error(t, err)
	require.True(t, errors.Is(err, extract.ErrUnavailable))
}
