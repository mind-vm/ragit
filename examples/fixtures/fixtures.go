// Package fixtures holds the documents both examples ingest.
//
// Three formats on purpose: Markdown with real heading depth, so HeadingPath
// has something to carry into a citation; a CSV, because tabular extraction is
// where xberg earns its place; and a PDF, because a text-layer PDF is the
// format a document pipeline is actually bought for.
//
// Everything here is written for these examples. Nothing is copied, and there
// is no customer data anywhere in it.
package fixtures

import (
	"embed"
	"fmt"
	"path"
)

//go:embed handbook.md inventory.csv field-notes.pdf
var files embed.FS

// Doc is one fixture document.
type Doc struct {
	Filename string
	MimeType string
	Data     []byte
	// Attributes is what an application would attach to it. These are
	// application facts, not extractor metadata, and they narrow a search
	// rather than confining it — see ragit.Attributes.
	Attributes map[string]string
}

// All returns every fixture, in a stable order.
func All() ([]Doc, error) {
	specs := []struct {
		name, mime, kind, team string
	}{
		{"handbook.md", "text/markdown", "handbook", "support"},
		{"inventory.csv", "text/csv", "report", "warehouse"},
		{"field-notes.pdf", "application/pdf", "notes", "warehouse"},
	}

	docs := make([]Doc, 0, len(specs))
	for _, s := range specs {
		data, err := files.ReadFile(s.name)
		if err != nil {
			return nil, fmt.Errorf("fixtures: read %s: %w", s.name, err)
		}
		docs = append(docs, Doc{
			Filename:   path.Base(s.name),
			MimeType:   s.mime,
			Data:       data,
			Attributes: map[string]string{"kind": s.kind, "team": s.team},
		})
	}
	return docs, nil
}
