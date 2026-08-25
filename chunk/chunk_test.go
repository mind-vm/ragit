package chunk_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mind-vm/ragit/chunk"
)

func TestSplitMarkdown_HeadingPathTracksNesting(t *testing.T) {
	text := "# Top\n\nintro text\n\n## Sub\n\nsub text that is long enough to stand on its own as content\n"
	c := chunk.New(chunk.Config{Size: 1000, Overlap: 50})

	chunks := c.SplitMarkdown(text)
	require.NotEmpty(t, chunks)

	var sawTop, sawNested bool
	for _, ch := range chunks {
		if len(ch.HeadingPath) == 1 && ch.HeadingPath[0] == "Top" {
			sawTop = true
		}
		if len(ch.HeadingPath) == 2 && ch.HeadingPath[0] == "Top" && ch.HeadingPath[1] == "Sub" {
			sawNested = true
		}
	}
	require.True(t, sawTop, "expected a chunk under just 'Top'")
	require.True(t, sawNested, "expected a chunk under 'Top' > 'Sub'")
}

func TestSplitMarkdown_RespectsSizeCap(t *testing.T) {
	text := "# Section\n\n" + strings.Repeat("word ", 500)
	c := chunk.New(chunk.Config{Size: 100, Overlap: 20})

	chunks := c.SplitMarkdown(text)
	require.NotEmpty(t, chunks)
	for _, ch := range chunks {
		require.LessOrEqualf(t, len(ch.Content), 100, "chunk %d exceeded size cap: %q", ch.Index, ch.Content)
	}
}

func TestSplitText_NoSeparators_ForceSplits(t *testing.T) {
	text := strings.Repeat("x", 250)
	c := chunk.New(chunk.Config{Size: 100, Overlap: 10})

	chunks := c.SplitText(text)
	require.Len(t, chunks, 3)
	for _, ch := range chunks {
		require.LessOrEqual(t, len(ch.Content), 100)
	}
}

func TestSplitText_Empty(t *testing.T) {
	c := chunk.New(chunk.DefaultConfig())
	require.Empty(t, c.SplitText(""))
	require.Empty(t, c.SplitText("   \n  "))
}
