package extract_test

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mind-vm/ragit/extract"
)

// TestMain wires the test binary as its own extraction child, which is
// exactly the one-line integration a host application performs in main().
// Running it here means the isolation tests exercise the real spawn path
// rather than a stand-in.
func TestMain(m *testing.M) {
	extract.RunIsolatedChildIfInvoked()
	os.Exit(m.Run())
}

func TestLocalExtractor_PlainTextAndMarkdown(t *testing.T) {
	e := extract.NewLocalExtractor()

	result, err := e.Extract(context.Background(), []byte("# Title\n\nBody text.\n"), "notes.md")
	require.NoError(t, err)
	require.Contains(t, result.Text, "# Title")
	require.Equal(t, 1, result.PageCount)

	_, err = e.Extract(context.Background(), []byte{0xff, 0xfe, 0xfd}, "notes.txt")
	require.Error(t, err, "invalid UTF-8 is a verdict on the document, not a parse to guess at")
}

func TestLocalExtractor_CSVBecomesMarkdownTable(t *testing.T) {
	csv := "name,role\nada,engineer\ngrace,admiral\n"
	result, err := extract.NewLocalExtractor().Extract(context.Background(), []byte(csv), "people.csv")
	require.NoError(t, err)

	require.Contains(t, result.Text, "| name | role |")
	require.Contains(t, result.Text, "| --- | --- |")
	require.Contains(t, result.Text, "| ada | engineer |")
}

func TestLocalExtractor_CSVEscapesPipes(t *testing.T) {
	csv := "a,b\n\"x|y\",z\n"
	result, err := extract.NewLocalExtractor().Extract(context.Background(), []byte(csv), "t.csv")
	require.NoError(t, err)
	require.Contains(t, result.Text, `x\|y`, "an unescaped pipe would silently split a table cell")
}

func TestLocalExtractor_JSON(t *testing.T) {
	result, err := extract.NewLocalExtractor().Extract(context.Background(), []byte(`{"b":1,"a":2}`), "d.json")
	require.NoError(t, err)
	require.Contains(t, result.Text, "```json")
	require.Contains(t, result.Text, `"a": 2`)

	_, err = extract.NewLocalExtractor().Extract(context.Background(), []byte(`{not json`), "d.json")
	require.Error(t, err)
}

func TestLocalExtractor_HTMLStripsScriptAndStyle(t *testing.T) {
	html := `<html><head><title>T</title></head><body>
		<script>var secret = "should not appear";</script>
		<style>body { color: red; }</style>
		<p>Real content here.</p>
	</body></html>`

	result, err := extract.NewLocalExtractor().Extract(context.Background(), []byte(html), "page.html")
	require.NoError(t, err)
	require.Contains(t, result.Text, "Real content here.")
	require.NotContains(t, result.Text, "should not appear")
	require.NotContains(t, result.Text, "color: red")
}

func TestLocalExtractor_UnsupportedFormatIsAVerdict(t *testing.T) {
	_, err := extract.NewLocalExtractor().Extract(context.Background(), []byte("x"), "sheet.xlsx")
	require.ErrorIs(t, err, extract.ErrUnsupportedFormat)
	require.NotErrorIs(t, err, extract.ErrUnavailable,
		"an unsupported format is terminal: nothing further down the chain would do better")
}

func buildDocx(t *testing.T, documentXML string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	require.NoError(t, err)
	_, err = w.Write([]byte(documentXML))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func TestLocalExtractor_DOCX(t *testing.T) {
	docx := buildDocx(t, `<?xml version="1.0"?>
		<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
		  <w:body>
		    <w:p><w:r><w:t>First paragraph.</w:t></w:r></w:p>
		    <w:p><w:r><w:t>Second</w:t><w:tab/><w:t>paragraph.</w:t></w:r></w:p>
		  </w:body>
		</w:document>`)

	result, err := extract.NewLocalExtractor().Extract(context.Background(), docx, "doc.docx")
	require.NoError(t, err)
	require.Contains(t, result.Text, "First paragraph.")
	require.Contains(t, result.Text, "Second\tparagraph.")
	require.Contains(t, result.Text, "First paragraph.\n\n", "paragraphs must stay separated for the chunker")
}

func TestLocalExtractor_DOCXWithoutBodyIsRejected(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("other.xml")
	require.NoError(t, err)
	_, err = w.Write([]byte("<x/>"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	_, err = extract.NewLocalExtractor().Extract(context.Background(), buf.Bytes(), "doc.docx")
	require.Error(t, err)
	require.NotErrorIs(t, err, extract.ErrUnavailable)
}

// The decompression budget is the zip-bomb guard named in design.md §6:
// zip.NewReader on untrusted bytes will happily expand whatever an entry
// declares.
func TestLocalExtractor_DOCXRespectsDecompressionBudget(t *testing.T) {
	big := `<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>` +
		string(bytes.Repeat([]byte("A"), 200_000)) + `</w:t></w:r></w:p></w:body></w:document>`
	docx := buildDocx(t, big)

	// Highly compressible content: the archive is far smaller than what it
	// expands to, which is precisely the attack shape.
	require.Less(t, len(docx), 10_000)

	e := &extract.LocalExtractor{MaxDecompressedBytes: 50_000}
	_, err := e.Extract(context.Background(), docx, "bomb.docx")
	require.Error(t, err)
	require.Contains(t, err.Error(), "decompressed")

	// The same file parses fine when the budget allows it, so the guard is
	// bounding expansion rather than rejecting large documents outright.
	generous := &extract.LocalExtractor{MaxDecompressedBytes: 10 << 20}
	result, err := generous.Extract(context.Background(), docx, "big.docx")
	require.NoError(t, err)
	require.Contains(t, result.Text, "AAA")
}

func TestLocalExtractor_PDFGarbageIsRejectedNotPanicking(t *testing.T) {
	// ledongthuc/pdf panics on some malformed input rather than erroring.
	// A panic escaping into a River worker would take down far more than one
	// document, so this must come back as an ordinary error.
	_, err := extract.NewLocalExtractor().Extract(context.Background(), []byte("%PDF-1.4\ngarbage"), "broken.pdf")
	require.Error(t, err)
	require.NotErrorIs(t, err, extract.ErrUnavailable, "a corrupt file is a verdict, not an outage")
}
