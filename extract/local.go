package extract

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

// ErrUnsupportedFormat marks a document LocalExtractor has no parser for. It
// is a verdict on the document, not an availability failure, so a Chain stops
// on it rather than falling through — there is nothing further down the chain
// that would do better, since local parsing is already the last layer.
var ErrUnsupportedFormat = errors.New("extract: unsupported format")

// LocalExtractor parses documents in-process, with no sidecar.
//
// It exists so a deployment that has not stood up an xberg sidecar still
// works, at a smaller supported format set and with no OCR (design.md §4).
// That convenience comes with real risk: these parsers run untrusted bytes
// through code that allocates based on what the file *claims*. A 212 kB PDF
// driving a parser to ~5 GB is the documented incident behind design.md §6.
//
// So: do not hand a LocalExtractor untrusted uploads directly. Wrap it in an
// [IsolatedExtractor], which runs exactly this code in a memory-capped child
// process. LocalExtractor is the thing being contained, not the containment.
type LocalExtractor struct {
	// MaxDecompressedBytes caps the total bytes read out of a container
	// format (docx's zip). Zero means DefaultMaxDecompressedBytes. This is
	// the zip-bomb guard: a 200 kB .docx can legitimately declare gigabytes
	// of entry content.
	MaxDecompressedBytes int64
}

// DefaultMaxDecompressedBytes bounds what one archive-backed document may
// expand to. Generous for real documents, ruinous for a zip bomb.
const DefaultMaxDecompressedBytes = 64 << 20 // 64 MiB

var _ Extractor = (*LocalExtractor)(nil)

// NewLocalExtractor builds a LocalExtractor with default limits.
func NewLocalExtractor() *LocalExtractor { return &LocalExtractor{} }

func (e *LocalExtractor) maxDecompressed() int64 {
	if e.MaxDecompressedBytes > 0 {
		return e.MaxDecompressedBytes
	}
	return DefaultMaxDecompressedBytes
}

// SupportedExtensions lists what LocalExtractor can parse. Deliberately much
// narrower than xberg's ~101 formats — this is a fallback, not a replacement.
func SupportedExtensions() []string {
	return []string{".txt", ".md", ".markdown", ".csv", ".tsv", ".json", ".html", ".htm", ".xml", ".pdf", ".docx"}
}

// Extract parses data locally, dispatching on the filename's extension.
func (e *LocalExtractor) Extract(_ context.Context, data []byte, filename string) (*Result, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".txt", ".md", ".markdown":
		return e.plainText(data)
	case ".csv":
		return e.separatedValues(data, ',')
	case ".tsv":
		return e.separatedValues(data, '\t')
	case ".json":
		return e.jsonDoc(data)
	case ".html", ".htm", ".xml":
		return e.markup(data)
	case ".pdf":
		return e.pdf(data)
	case ".docx":
		return e.docx(data)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedFormat, ext)
	}
}

func (e *LocalExtractor) plainText(data []byte) (*Result, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("extract: file is not valid UTF-8 text")
	}
	return &Result{Text: string(data), PageCount: 1, Metadata: localMetadata("text")}, nil
}

// separatedValues renders tabular data as a Markdown table, which is what
// makes it useful as retrieval context rather than a wall of commas.
func (e *LocalExtractor) separatedValues(data []byte, comma rune) (*Result, error) {
	r := csv.NewReader(bytes.NewReader(data))
	r.Comma = comma
	// Real-world CSVs are ragged far more often than they are malformed.
	r.FieldsPerRecord = -1
	r.LazyQuotes = true

	// Read row-by-row rather than ReadAll: an unbounded ReadAll on a
	// hostile file is one of the allocation paths design.md §6 calls out.
	var rows [][]string
	var total int64
	for {
		record, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("extract: parse delimited file: %w", err)
		}
		for _, f := range record {
			total += int64(len(f))
		}
		if total > e.maxDecompressed() {
			return nil, fmt.Errorf("extract: delimited file exceeds %d bytes of cell content", e.maxDecompressed())
		}
		rows = append(rows, record)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("extract: delimited file has no rows")
	}

	var b strings.Builder
	writeRow := func(cells []string) {
		b.WriteString("| ")
		for i, c := range cells {
			if i > 0 {
				b.WriteString(" | ")
			}
			b.WriteString(strings.ReplaceAll(strings.TrimSpace(c), "|", `\|`))
		}
		b.WriteString(" |\n")
	}

	header := rows[0]
	writeRow(header)
	b.WriteString("|")
	for range header {
		b.WriteString(" --- |")
	}
	b.WriteString("\n")
	for _, row := range rows[1:] {
		writeRow(row)
	}

	return &Result{Text: b.String(), PageCount: 1, Metadata: localMetadata("delimited")}, nil
}

func (e *LocalExtractor) jsonDoc(data []byte) (*Result, error) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("extract: parse json: %w", err)
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("extract: render json: %w", err)
	}
	return &Result{
		Text:      "```json\n" + string(pretty) + "\n```\n",
		PageCount: 1,
		Metadata:  localMetadata("json"),
	}, nil
}

// markup strips tags with encoding/xml's tokenizer rather than a regex, and
// rather than pulling in an HTML5 parser. It handles well-formed markup and
// degrades to "best effort" on the rest, which is the right trade for a
// fallback path — an xberg sidecar is what handles real-world HTML properly.
func (e *LocalExtractor) markup(data []byte) (*Result, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.Strict = false
	decoder.AutoClose = xml.HTMLAutoClose
	decoder.Entity = xml.HTMLEntity

	var b strings.Builder
	skipDepth := 0
	for {
		tok, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("extract: parse markup: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			// script/style content is markup noise, never document text.
			switch strings.ToLower(t.Name.Local) {
			case "script", "style", "head":
				skipDepth++
			}
		case xml.EndElement:
			switch strings.ToLower(t.Name.Local) {
			case "script", "style", "head":
				if skipDepth > 0 {
					skipDepth--
				}
			}
		case xml.CharData:
			if skipDepth > 0 {
				continue
			}
			text := strings.TrimSpace(string(t))
			if text == "" {
				continue
			}
			b.WriteString(text)
			b.WriteString("\n\n")
		}
		if int64(b.Len()) > e.maxDecompressed() {
			return nil, fmt.Errorf("extract: markup exceeds %d bytes of text", e.maxDecompressed())
		}
	}

	text := strings.TrimSpace(b.String())
	if text == "" {
		return nil, fmt.Errorf("extract: markup contained no text")
	}
	return &Result{Text: text + "\n", PageCount: 1, Metadata: localMetadata("markup")}, nil
}

// pdf extracts a text layer. It does not OCR: a scanned PDF with no text
// layer yields an error here rather than silently producing an empty
// document, since "no text" and "text we failed to read" are different
// outcomes and only one of them is worth a retry with OCR enabled.
func (e *LocalExtractor) pdf(data []byte) (result *Result, err error) {
	// ledongthuc/pdf panics on a number of malformed files rather than
	// returning an error. It is exactly the library design.md §6's incident
	// involved, so this recover is load-bearing, not defensive dressing.
	defer func() {
		if r := recover(); r != nil {
			result = nil
			err = fmt.Errorf("extract: pdf parser panicked: %v", r)
		}
	}()

	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("extract: open pdf: %w", err)
	}

	pageCount := reader.NumPage()
	var b strings.Builder
	for i := 1; i <= pageCount; i++ {
		page := reader.Page(i)
		if page.V.IsNull() {
			continue
		}
		text, err := page.GetPlainText(nil)
		if err != nil {
			return nil, fmt.Errorf("extract: read pdf page %d: %w", i, err)
		}
		b.WriteString(text)
		b.WriteString("\n\n")
		if int64(b.Len()) > e.maxDecompressed() {
			return nil, fmt.Errorf("extract: pdf exceeds %d bytes of text", e.maxDecompressed())
		}
	}

	text := strings.TrimSpace(b.String())
	if text == "" {
		return nil, fmt.Errorf("extract: pdf has no text layer (a scan needs OCR, which local extraction does not do)")
	}
	return &Result{Text: text + "\n", PageCount: pageCount, Metadata: localMetadata("pdf")}, nil
}

// docx reads word/document.xml out of the package zip.
//
// zip.NewReader on untrusted bytes is a named zip-bomb target in design.md
// §6, so every read here goes through a byte budget rather than trusting the
// entry's declared size.
func (e *LocalExtractor) docx(data []byte) (*Result, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("extract: open docx: %w", err)
	}

	var doc *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			doc = f
			break
		}
	}
	if doc == nil {
		return nil, fmt.Errorf("extract: docx has no word/document.xml")
	}

	rc, err := doc.Open()
	if err != nil {
		return nil, fmt.Errorf("extract: open docx body: %w", err)
	}
	defer func() { _ = rc.Close() }()

	budget := e.maxDecompressed()
	// LimitReader by budget+1 so hitting exactly the budget is detectable as
	// "there was more" rather than passing silently.
	body, err := io.ReadAll(io.LimitReader(rc, budget+1))
	if err != nil {
		return nil, fmt.Errorf("extract: read docx body: %w", err)
	}
	if int64(len(body)) > budget {
		return nil, fmt.Errorf("extract: docx body exceeds %d bytes decompressed", budget)
	}

	text, err := docxText(body)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("extract: docx contained no text")
	}
	return &Result{Text: text, PageCount: 1, Metadata: localMetadata("docx")}, nil
}

// docxText turns WordprocessingML into Markdown-ish text: one paragraph per
// <w:p>, with <w:tab> and <w:br> preserved as whitespace. Headings and tables
// are not reconstructed — that is exactly the work xberg does properly, and
// approximating it badly here would produce citations that look right and are
// not.
func docxText(body []byte) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	var out strings.Builder
	var para strings.Builder

	flush := func() {
		if s := strings.TrimSpace(para.String()); s != "" {
			out.WriteString(s)
			out.WriteString("\n\n")
		}
		para.Reset()
	}

	for {
		tok, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("extract: parse docx xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "tab":
				para.WriteString("\t")
			case "br", "cr":
				para.WriteString("\n")
			}
		case xml.EndElement:
			if t.Name.Local == "p" {
				flush()
			}
		case xml.CharData:
			para.Write(t)
		}
	}
	flush()
	return out.String(), nil
}

func localMetadata(kind string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"extractor":"local","parser":%q}`, kind))
}
