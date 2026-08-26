package demo

import (
	"fmt"
	"strings"

	"github.com/mind-vm/ragit"
)

// Section prints a heading, so the phases of an example are separable when the
// two are run one after the other and compared.
func Section(title string) {
	fmt.Printf("\n── %s %s\n", title, strings.Repeat("─", max(0, 68-len(title))))
}

// PrintDocuments reports the catalog: what is indexed, in what state, and
// under which embedding model.
//
// The embedding space is on this line rather than buried because alignment is
// what makes a corpus searchable — chunks embedded under a different
// fingerprint are excluded from vector search rather than ranked, so a corpus
// that straddles two spaces looks like one that simply has fewer answers.
//
// This column used to hold the model name alone, which could not tell two
// providers serving that model apart, nor two dimensions of it — precisely the
// straddle it looked like it was reporting. It now carries the whole
// provider|model|dimension, the same identity every chunk carries.
// Processor.CountMisalignedChunks is still the read that answers whether a
// corpus has actually straddled, chunk by chunk.
func PrintDocuments(docs []ragit.Document) {
	if len(docs) == 0 {
		fmt.Println("  (no documents)")
		return
	}
	fmt.Printf("  %-18s %-18s %7s  %s\n", "FILENAME", "STATUS", "CHUNKS", "EMBEDDING SPACE")
	for _, d := range docs {
		chunks := "-"
		if d.ChunkCount != nil {
			chunks = fmt.Sprintf("%d", *d.ChunkCount)
		}
		space := "-"
		if d.EmbeddingFingerprint != nil {
			space = *d.EmbeddingFingerprint
		}
		fmt.Printf("  %-18s %-18s %7s  %s\n", d.Filename, d.Status, chunks, space)
		if d.Error != nil && *d.Error != "" {
			fmt.Printf("  %-18s   error: %s\n", "", *d.Error)
		}
	}
}

// PrintResults reports retrieved chunks the way a citation UI would need them:
// score, source document, and the heading trail that says where in it the
// answer came from.
func PrintResults(results []ragit.SearchResult) {
	if len(results) == 0 {
		fmt.Println("  (no results)")
		return
	}
	for i, r := range results {
		heading := strings.Join(r.HeadingPath, " › ")
		if heading == "" {
			heading = "(no heading)"
		}
		fmt.Printf("  %d. %.4f  %s  ›  %s\n", i+1, r.Score, r.Filename, heading)
		fmt.Printf("      %s\n", excerpt(r.Content, 150))
	}
}

func excerpt(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
