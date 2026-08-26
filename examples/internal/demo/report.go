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
// The model is on this line rather than buried because embedding-space
// alignment is what makes a corpus searchable — chunks embedded under a
// different fingerprint are excluded from vector search rather than ranked, so
// a corpus that straddles two spaces looks like one that simply has fewer
// answers.
//
// Note the column is the *model*, not the fingerprint. documents.embedding_model
// stores embedder.Model() alone, while each chunk stores the full
// provider|model|dimension. So this column cannot tell two providers serving
// the same model name apart, nor two dimensions of it — which is precisely the
// straddle it looks like it is reporting. Processor.CountMisalignedChunks is
// the read that actually answers the question.
func PrintDocuments(docs []ragit.Document) {
	if len(docs) == 0 {
		fmt.Println("  (no documents)")
		return
	}
	fmt.Printf("  %-18s %-18s %7s  %s\n", "FILENAME", "STATUS", "CHUNKS", "EMBEDDING MODEL")
	for _, d := range docs {
		chunks := "-"
		if d.ChunkCount != nil {
			chunks = fmt.Sprintf("%d", *d.ChunkCount)
		}
		space := "-"
		if d.EmbeddingModel != nil {
			space = *d.EmbeddingModel
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
