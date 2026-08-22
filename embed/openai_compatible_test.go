package embed_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jryannel/ragit/embed"
)

func TestClient_Embed_ExactDimension_PassesThrough(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model      string   `json:"model"`
			Input      []string `json:"input"`
			Dimensions int      `json:"dimensions"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		require.Equal(t, 4, req.Dimensions)
		require.Equal(t, []string{"a", "b"}, req.Input)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"index": 1, "embedding": []float32{0, 0, 0, 1}},
				{"index": 0, "embedding": []float32{1, 0, 0, 0}},
			},
		})
	}))
	defer server.Close()

	c, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{
		APIKey: "k", BaseURL: server.URL, Dimension: 4,
	})
	require.NoError(t, err)

	vecs, err := c.Embed(context.Background(), []string{"a", "b"})
	require.NoError(t, err)
	require.Len(t, vecs, 2)
	// Order follows the request's Input order, not the response's array order.
	require.Equal(t, embed.Vector{1, 0, 0, 0}, vecs[0])
	require.Equal(t, embed.Vector{0, 0, 0, 1}, vecs[1])
}

func TestClient_Embed_WiderResponse_TruncatesAndRenormalizes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Provider ignores the requested `dimensions` and returns its native
		// width — the EdenAI/gemini-embedding-001 quirk this defends against.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"index": 0, "embedding": []float32{3, 4, 0, 0}}, // norm 5
			},
		})
	}))
	defer server.Close()

	c, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{
		APIKey: "k", BaseURL: server.URL, Dimension: 2,
	})
	require.NoError(t, err)

	vecs, err := c.Embed(context.Background(), []string{"x"})
	require.NoError(t, err)
	require.Len(t, vecs, 1)
	require.Len(t, vecs[0], 2)

	// Truncated to the first 2 components (3, 4) then L2-renormalized: norm(3,4)=5 -> (0.6, 0.8).
	require.InDelta(t, 0.6, vecs[0][0], 1e-6)
	require.InDelta(t, 0.8, vecs[0][1], 1e-6)

	var norm float64
	for _, x := range vecs[0] {
		norm += float64(x) * float64(x)
	}
	require.InDelta(t, 1.0, math.Sqrt(norm), 1e-6)
}

func TestClient_Embed_NarrowerResponse_IsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float32{1, 2}}},
		})
	}))
	defer server.Close()

	c, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{
		APIKey: "k", BaseURL: server.URL, Dimension: 5,
	})
	require.NoError(t, err)

	_, err = c.Embed(context.Background(), []string{"x"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot pad")
}

func TestClient_Embed_5xx_IsErrUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	c, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{APIKey: "k", BaseURL: server.URL})
	require.NoError(t, err)

	_, err = c.Embed(context.Background(), []string{"x"})
	require.Error(t, err)
	require.True(t, errors.Is(err, embed.ErrUnavailable))
}

func TestClient_Embed_TransportFailure_IsErrUnavailable(t *testing.T) {
	c, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{APIKey: "k", BaseURL: "http://127.0.0.1:1"})
	require.NoError(t, err)

	_, err = c.Embed(context.Background(), []string{"x"})
	require.Error(t, err)
	require.True(t, errors.Is(err, embed.ErrUnavailable))
}

func TestClient_Embed_429_IsErrRateLimited(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	c, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{APIKey: "k", BaseURL: server.URL})
	require.NoError(t, err)

	_, err = c.Embed(context.Background(), []string{"x"})
	require.Error(t, err)
	require.True(t, errors.Is(err, embed.ErrRateLimited))
}

func TestClient_Embed_Other4xx_IsPlainError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	c, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{APIKey: "bad-key", BaseURL: server.URL})
	require.NoError(t, err)

	_, err = c.Embed(context.Background(), []string{"x"})
	require.Error(t, err)
	require.False(t, errors.Is(err, embed.ErrUnavailable), "a bad API key is a permanent failure, not a reason to retry")
	require.False(t, errors.Is(err, embed.ErrRateLimited))
}

func TestClient_Fingerprint_ReflectsProviderModelDimension(t *testing.T) {
	c, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{
		APIKey: "k", Provider: "edenai", Model: "google/gemini-embedding-001", Dimension: 1536,
	})
	require.NoError(t, err)
	require.Equal(t, "edenai|google/gemini-embedding-001|1536", embed.Fingerprint(c))
}

func TestNewOpenAICompatible_RequiresAPIKey(t *testing.T) {
	_, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{})
	require.Error(t, err)
}
