// Package chunk splits extracted Markdown into retrieval-sized pieces.
//
// This is deliberately not delegated to xberg's own chunker: chunk metadata
// (HeadingPath, CharStart/CharEnd) needs to round-trip into a citation UI,
// and Markdown-aware recursive splitting is a small, well-understood
// algorithm — not a place outsourcing saves meaningful effort, unlike
// OCR/table-reconstruction.
package chunk

import (
	"regexp"
	"strings"
)

// Chunk is one piece of a document, ready to embed.
type Chunk struct {
	Index       int
	Content     string
	CharStart   int
	CharEnd     int
	HeadingPath []string // e.g. {"Chapter 2", "Section 2.1"}, for citations
}

// Config controls chunk size.
type Config struct {
	// Size is the target maximum chunk length, in bytes.
	Size int
	// Overlap is how much trailing content of one chunk is repeated at the
	// start of the next, to avoid losing context at a chunk boundary.
	Overlap int
}

// DefaultConfig matches the values validated in the reference RAG
// implementation this package's algorithm is modeled on.
func DefaultConfig() Config {
	return Config{Size: 1000, Overlap: 200}
}

// Chunker splits text into overlapping, size-bounded chunks.
type Chunker struct {
	config Config
}

// New creates a Chunker with the given config.
func New(config Config) *Chunker {
	return &Chunker{config: config}
}

// separators are tried in order of preference: prefer splitting on
// paragraph, then line, then sentence, then word boundaries before falling
// back to a hard byte-boundary split.
var separators = []string{"\n\n", "\n", ". ", " ", ""}

var headerRegexp = regexp.MustCompile(`(?m)^(#{1,6})\s+(.+)$`)

// SplitMarkdown splits Markdown text heading-aware: each top-level unit is a
// heading's content, further split by SplitText if still too large. The
// heading trail becomes each resulting chunk's HeadingPath.
func (c *Chunker) SplitMarkdown(text string) []Chunk {
	sections := c.extractSections(text)

	var chunks []Chunk
	charOffset := 0
	for _, s := range sections {
		if len(s.content) > c.config.Size {
			for _, sub := range c.recursiveSplit(s.content, separators, charOffset) {
				chunks = append(chunks, Chunk{
					Index:       len(chunks),
					Content:     sub.Content,
					CharStart:   sub.CharStart,
					CharEnd:     sub.CharEnd,
					HeadingPath: s.path,
				})
			}
		} else if len(s.content) > 0 {
			chunks = append(chunks, Chunk{
				Index:       len(chunks),
				Content:     s.content,
				CharStart:   charOffset,
				CharEnd:     charOffset + len(s.content),
				HeadingPath: s.path,
			})
		}
		charOffset += len(s.content)
	}
	return chunks
}

// SplitText splits arbitrary (non-Markdown) text via recursive character
// splitting, with no heading awareness.
func (c *Chunker) SplitText(text string) []Chunk {
	return c.recursiveSplit(text, separators, 0)
}

type section struct {
	content string
	path    []string // heading trail, outermost first
}

// extractSections walks headings top-to-bottom, tracking a stack of open
// heading levels so a chunk's HeadingPath reflects nesting (e.g. an H3 under
// an H2 under an H1), not just its immediate heading.
func (c *Chunker) extractSections(text string) []section {
	matches := headerRegexp.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return []section{{content: text}}
	}

	var sections []section
	var stack []struct {
		level int
		title string
	}
	lastEnd := 0

	pathOf := func() []string {
		path := make([]string, len(stack))
		for i, s := range stack {
			path[i] = s.title
		}
		return path
	}

	for i, m := range matches {
		if m[0] > lastEnd {
			content := strings.TrimSpace(text[lastEnd:m[0]])
			if content != "" {
				sections = append(sections, section{content: content, path: pathOf()})
			}
		}

		level := m[3] - m[2] // number of '#' characters
		title := text[m[4]:m[5]]

		for len(stack) > 0 && stack[len(stack)-1].level >= level {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, struct {
			level int
			title string
		}{level, title})

		end := len(text)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		content := strings.TrimSpace(text[m[0]:end])
		if content != "" {
			sections = append(sections, section{content: content, path: pathOf()})
		}
		lastEnd = end
	}

	return sections
}

func (c *Chunker) recursiveSplit(text string, seps []string, charOffset int) []Chunk {
	if len(text) <= c.config.Size {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []Chunk{{Content: text, CharStart: charOffset, CharEnd: charOffset + len(text)}}
	}

	sepIndex := -1
	for i, sep := range seps {
		if sep == "" {
			break
		}
		if strings.Contains(text, sep) {
			sepIndex = i
			break
		}
	}
	if sepIndex == -1 {
		return c.forceSplit(text, charOffset)
	}

	sep := seps[sepIndex]
	finer := seps[sepIndex+1:]
	parts := strings.Split(text, sep)

	var chunks []Chunk
	var current strings.Builder
	currentStart := charOffset
	pos := charOffset

	flush := func() {
		if current.Len() == 0 {
			return
		}
		content := current.String()
		chunks = append(chunks, Chunk{Content: content, CharStart: currentStart, CharEnd: currentStart + len(content)})
	}

	for i, part := range parts {
		piece := part
		if i > 0 {
			piece = sep + part
		}

		if len(piece) > c.config.Size {
			flush()
			current.Reset()
			chunks = append(chunks, c.recursiveSplit(piece, finer, pos)...)
			pos += len(piece)
			currentStart = pos
			continue
		}

		if current.Len()+len(piece) > c.config.Size && current.Len() > 0 {
			flush()
			overlap := c.overlapOf(current.String())
			current.Reset()
			if len(overlap)+len(piece) <= c.config.Size {
				current.WriteString(overlap)
				currentStart = currentStart + len(chunks[len(chunks)-1].Content) - len(overlap)
			} else {
				currentStart = pos
			}
		}

		current.WriteString(piece)
		pos += len(piece)
	}
	flush()

	for i := range chunks {
		chunks[i].Index = i
	}
	return chunks
}

func (c *Chunker) forceSplit(text string, charOffset int) []Chunk {
	stride := c.config.Size - c.config.Overlap
	if stride <= 0 {
		stride = c.config.Size
	}
	if stride <= 0 {
		stride = 1
	}

	var chunks []Chunk
	for i := 0; i < len(text); i += stride {
		end := min(i+c.config.Size, len(text))
		chunks = append(chunks, Chunk{Content: text[i:end], CharStart: charOffset + i, CharEnd: charOffset + end, Index: len(chunks)})
		if end >= len(text) {
			break
		}
	}
	return chunks
}

func (c *Chunker) overlapOf(text string) string {
	if len(text) <= c.config.Overlap {
		return text
	}
	overlap := text[len(text)-c.config.Overlap:]
	if idx := strings.Index(overlap, " "); idx > 0 && idx < len(overlap)/2 {
		overlap = overlap[idx+1:]
	}
	return overlap
}
