package extract_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jryannel/ragit/extract"
)

// These tests spawn the test binary itself as an extraction child — TestMain
// in local_test.go calls RunIsolatedChildIfInvoked, which is the same one
// line a host application adds to main().

func TestIsolatedExtractor_ParsesInChildProcess(t *testing.T) {
	result, err := extract.NewIsolatedExtractor().Extract(
		context.Background(), []byte("# Title\n\nBody.\n"), "notes.md")
	require.NoError(t, err)
	require.Contains(t, result.Text, "# Title")
	require.Contains(t, string(result.Metadata), `"extractor":"local"`,
		"the child's own metadata must survive the round-trip")
}

func TestIsolatedExtractor_RoundTripsBinaryContent(t *testing.T) {
	// Base64 on the wire exists so bytes that look like JSON control
	// characters cannot terminate the child's stdin stream early.
	nasty := []byte("a,b\n\"quote\"\",\",\x00\x01binary\n")
	result, err := extract.NewIsolatedExtractor().Extract(context.Background(), nasty, "t.csv")
	require.NoError(t, err)
	require.Contains(t, result.Text, "| a | b |")
}

// A document the child rejected is terminal: it must NOT come back as
// ErrUnavailable, or a Chain would hand the same bytes to a less-contained
// parser — the §6 rule.
func TestIsolatedExtractor_DocumentRejectionIsTerminal(t *testing.T) {
	_, err := extract.NewIsolatedExtractor().Extract(context.Background(), []byte("x"), "sheet.xlsx")
	require.Error(t, err)
	require.NotErrorIs(t, err, extract.ErrUnavailable)
	require.Contains(t, err.Error(), "unsupported format")
}

func TestIsolatedExtractor_CorruptPDFIsTerminal(t *testing.T) {
	_, err := extract.NewIsolatedExtractor().Extract(context.Background(), []byte("%PDF-1.4\ngarbage"), "broken.pdf")
	require.Error(t, err)
	require.NotErrorIs(t, err, extract.ErrUnavailable,
		"a parser that reached a verdict inside the child is not an availability failure")
}

// The containment claim itself: a child that outgrows its cap dies on its own
// and the parent survives to report it. The cap is set below what any Go
// process can run in, so the watchdog trips deterministically rather than
// depending on a fixture that happens to allocate enough.
func TestIsolatedExtractor_MemoryCapKillsChildNotParent(t *testing.T) {
	e := &extract.IsolatedExtractor{MemoryLimit: 1 << 20} // 1 MiB

	_, err := e.Extract(context.Background(), []byte("hello"), "notes.md")
	require.Error(t, err)
	require.ErrorIs(t, err, extract.ErrUnavailable,
		"a contained blow-up is a resource failure, so the job stays retryable")
	require.Contains(t, err.Error(), "memory cap")

	// The parent is unharmed: a normal extraction still works afterwards.
	result, err := extract.NewIsolatedExtractor().Extract(context.Background(), []byte("# ok\n"), "notes.md")
	require.NoError(t, err)
	require.Contains(t, result.Text, "# ok")
}

func TestIsolatedExtractor_TimeoutIsUnavailable(t *testing.T) {
	e := &extract.IsolatedExtractor{Timeout: time.Nanosecond}

	_, err := e.Extract(context.Background(), []byte("# hi\n"), "notes.md")
	require.Error(t, err)
	require.ErrorIs(t, err, extract.ErrUnavailable)
}

// A host application that forgot the RunIsolatedChildIfInvoked wiring gets a
// degraded chain, not a broken one: the isolation layer reports itself
// unavailable and the chain falls through.
func TestIsolatedExtractor_UnwiredBinaryDegradesGracefully(t *testing.T) {
	e := &extract.IsolatedExtractor{}
	// /usr/bin/true exits 0 having produced no response, which is exactly
	// what an unwired host binary's normal startup path looks like from here.
	extract.SetBinaryForTest(e, "/usr/bin/true")

	_, err := e.Extract(context.Background(), []byte("# hi\n"), "notes.md")
	require.Error(t, err)
	require.ErrorIs(t, err, extract.ErrUnavailable)
	require.True(t,
		strings.Contains(err.Error(), "RunIsolatedChildIfInvoked") || strings.Contains(err.Error(), "no usable response"),
		"the error must point at the missing wiring; got: %v", err)
}

func TestIsolatedExtractor_MissingBinaryIsUnavailable(t *testing.T) {
	e := &extract.IsolatedExtractor{}
	extract.SetBinaryForTest(e, "/nonexistent/ragit-child")

	_, err := e.Extract(context.Background(), []byte("# hi\n"), "notes.md")
	require.ErrorIs(t, err, extract.ErrUnavailable,
		"failing to spawn is a deployment problem, never a verdict on the document")
}

// The full three-layer stack from design.md §6, with the sidecar absent.
func TestChain_XbergAbsentFallsThroughToIsolatedLocal(t *testing.T) {
	// A sidecar URL that nothing is listening on: connection refused, which
	// is a transport failure and therefore a legitimate fallback trigger.
	xberg := extract.NewXbergExtractor("http://127.0.0.1:1", 2*time.Second)
	chain := extract.NewChain(xberg, extract.NewIsolatedExtractor(), extract.NewLocalExtractor())
	require.Equal(t, 3, chain.Len())

	result, err := chain.Extract(context.Background(), []byte("# Fallback\n\nWorks.\n"), "notes.md")
	require.NoError(t, err)
	require.Contains(t, result.Text, "# Fallback")
}

// The startup check above proves a cap is enforced before parsing begins.
// This proves the other half — the watchdog catching a child that outgrows
// its cap *while* parsing, which is the shape of the real incident: a small
// file whose parse balloons.
func TestIsolatedExtractor_MemoryCapCatchesGrowthDuringParse(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates tens of MB; skipped in -short mode")
	}

	// A CSV large enough that decoding and accumulating it comfortably
	// outgrows the cap, but whose cap is far above the Go runtime's own
	// startup footprint — so a trip here can only come from parsing.
	var buf bytes.Buffer
	buf.WriteString("col_a,col_b\n")
	row := strings.Repeat("x", 512)
	for buf.Len() < 48<<20 {
		buf.WriteString(row)
		buf.WriteString(",")
		buf.WriteString(row)
		buf.WriteString("\n")
	}

	e := &extract.IsolatedExtractor{MemoryLimit: 64 << 20, Timeout: 60 * time.Second}
	_, err := e.Extract(context.Background(), buf.Bytes(), "huge.csv")
	require.Error(t, err)
	require.ErrorIs(t, err, extract.ErrUnavailable)
	require.Contains(t, err.Error(), "memory cap")
}
