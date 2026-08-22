package extract_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jryannel/ragit/extract"
)

type stubExtractor struct {
	name   string
	result *extract.Result
	err    error
	calls  *[]string
}

func (s stubExtractor) Extract(_ context.Context, _ []byte, _ string) (*extract.Result, error) {
	*s.calls = append(*s.calls, s.name)
	return s.result, s.err
}

func TestChain_FallsBackOnlyWhenExtractorUnavailable(t *testing.T) {
	var calls []string
	chain := extract.NewChain(
		stubExtractor{name: "xberg", err: fmt.Errorf("%w: connection refused", extract.ErrUnavailable), calls: &calls},
		stubExtractor{name: "isolated", result: &extract.Result{Text: "hello"}, calls: &calls},
	)

	result, err := chain.Extract(context.Background(), []byte("x"), "a.pdf")
	require.NoError(t, err)
	require.Equal(t, "hello", result.Text)
	require.Equal(t, []string{"xberg", "isolated"}, calls)
}

// This is the rule design.md §6 exists for: a document the first extractor
// *rejected* must not be retried against a less-contained parser. Retrying a
// file that already broke one parser down the chain is the path that caused
// the OOM incident.
func TestChain_DoesNotFallBackOnDocumentRejection(t *testing.T) {
	var calls []string
	rejection := errors.New("xberg extraction failed: corrupt file")
	chain := extract.NewChain(
		stubExtractor{name: "xberg", err: rejection, calls: &calls},
		stubExtractor{name: "isolated", result: &extract.Result{Text: "should never be reached"}, calls: &calls},
	)

	_, err := chain.Extract(context.Background(), []byte("x"), "a.pdf")
	require.ErrorIs(t, err, rejection)
	require.Equal(t, []string{"xberg"}, calls, "a rejected document must not reach the next extractor")
}

func TestChain_AllUnavailableIsItselfUnavailable(t *testing.T) {
	var calls []string
	chain := extract.NewChain(
		stubExtractor{name: "a", err: fmt.Errorf("%w: refused", extract.ErrUnavailable), calls: &calls},
		stubExtractor{name: "b", err: fmt.Errorf("%w: no binary", extract.ErrUnavailable), calls: &calls},
	)

	_, err := chain.Extract(context.Background(), []byte("x"), "a.pdf")
	require.ErrorIs(t, err, extract.ErrUnavailable,
		"an exhausted chain is a deployment failure, so the job stays retryable")
	require.Contains(t, err.Error(), "refused")
	require.Contains(t, err.Error(), "no binary")
	require.Equal(t, []string{"a", "b"}, calls)
}

func TestChain_SkipsNilExtractors(t *testing.T) {
	var calls []string
	// The shape a caller writes when the xberg sidecar is unconfigured:
	// a typed nil rather than a branch.
	var unconfigured *extract.XbergExtractor
	chain := extract.NewChain(
		unconfigured,
		stubExtractor{name: "local", result: &extract.Result{Text: "ok"}, calls: &calls},
	)
	require.Equal(t, 1, chain.Len())

	result, err := chain.Extract(context.Background(), []byte("x"), "a.md")
	require.NoError(t, err)
	require.Equal(t, "ok", result.Text)
}

func TestChain_EmptyChainErrors(t *testing.T) {
	_, err := extract.NewChain().Extract(context.Background(), []byte("x"), "a.md")
	require.Error(t, err)
}

func TestChain_StopsOnCancelledContext(t *testing.T) {
	var calls []string
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	chain := extract.NewChain(
		stubExtractor{name: "a", err: fmt.Errorf("%w: refused", extract.ErrUnavailable), calls: &calls},
		stubExtractor{name: "b", result: &extract.Result{Text: "unreached"}, calls: &calls},
	)

	_, err := chain.Extract(ctx, []byte("x"), "a.pdf")
	require.Error(t, err)
	require.Equal(t, []string{"a"}, calls,
		"a cancelled caller ends the operation; the next extractor would only fail more slowly")
}
