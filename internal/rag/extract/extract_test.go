/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package extract

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// zipEntry is a single file in a programmatically built zip fixture.
type zipEntry struct {
	name    string
	content string
}

// zipFixture builds an in-memory zip archive with entries in the given order.
// Order matters: mimetype's OOXML detection requires the first entry to be
// "[Content_Types].xml" (or a _rels/ entry).
func zipFixture(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range entries {
		fw, err := w.Create(e.name)
		if err != nil {
			t.Fatalf("creating zip entry %q: %v", e.name, err)
		}
		if _, err := fw.Write([]byte(e.content)); err != nil {
			t.Fatalf("writing zip entry %q: %v", e.name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing zip writer: %v", err)
	}
	return buf.Bytes()
}

const contentTypesXML = `<?xml version="1.0" encoding="UTF-8"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="xml" ContentType="application/xml"/></Types>`

func docxFixture(t *testing.T) []byte {
	return zipFixture(t,
		zipEntry{"[Content_Types].xml", contentTypesXML},
		zipEntry{"word/document.xml", `<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>
    <w:p><w:r><w:t>Hello document</w:t></w:r></w:p>
    <w:p><w:r><w:t>Second &amp; final paragraph</w:t></w:r></w:p>
  </w:body>
</w:document>`},
	)
}

func pptxFixture(t *testing.T) []byte {
	return zipFixture(t,
		zipEntry{"[Content_Types].xml", contentTypesXML},
		zipEntry{"ppt/slides/slide2.xml", `<?xml version="1.0" encoding="UTF-8"?>
<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main">
  <p:cSld><p:spTree><p:sp><p:txBody>
    <a:p><a:r><a:t>Slide two text</a:t></a:r></a:p>
  </p:txBody></p:sp></p:spTree></p:cSld>
</p:sld>`},
		zipEntry{"ppt/slides/slide1.xml", `<?xml version="1.0" encoding="UTF-8"?>
<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main">
  <p:cSld><p:spTree><p:sp><p:txBody>
    <a:p><a:r><a:t>Slide one title</a:t></a:r></a:p>
    <a:p><a:r><a:t>Slide one body</a:t></a:r></a:p>
  </p:txBody></p:sp></p:spTree></p:cSld>
</p:sld>`},
	)
}

func xlsxFixture(t *testing.T) []byte {
	return zipFixture(t,
		zipEntry{"[Content_Types].xml", contentTypesXML},
		zipEntry{"xl/sharedStrings.xml", `<?xml version="1.0" encoding="UTF-8"?>
<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" count="2" uniqueCount="2">
  <si><t>Shared value one</t></si>
  <si><t>Shared value two</t></si>
</sst>`},
		zipEntry{"xl/worksheets/sheet1.xml", `<?xml version="1.0" encoding="UTF-8"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <sheetData>
    <row r="1"><c t="inlineStr"><is><t>Inline cell A</t></is></c><c t="inlineStr"><is><t>Inline cell B</t></is></c></row>
  </sheetData>
</worksheet>`},
	)
}

// buildPDF assembles a minimal valid PDF with one page per text argument,
// computing the xref table offsets from the actual byte positions.
func buildPDF(pageTexts ...string) []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")

	offsets := map[int]int{}
	writeObj := func(num int, body string) {
		offsets[num] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", num, body)
	}

	// Object numbering: 1 catalog, 2 pages tree, then per page a page object
	// and a content stream, and finally the font.
	fontNum := 3 + 2*len(pageTexts)
	kids := make([]string, len(pageTexts))
	for i, text := range pageTexts {
		pageNum := 3 + 2*i
		contentNum := pageNum + 1
		kids[i] = fmt.Sprintf("%d 0 R", pageNum)
		stream := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", text)
		writeObj(pageNum, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 %d 0 R >> >> /Contents %d 0 R >>",
			fontNum, contentNum))
		writeObj(contentNum, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}
	writeObj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObj(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pageTexts)))
	writeObj(fontNum, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	xrefStart := buf.Len()
	lastObj := fontNum
	fmt.Fprintf(&buf, "xref\n0 %d\n", lastObj+1)
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i <= lastObj; i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", lastObj+1, xrefStart)
	return buf.Bytes()
}

const htmlFixture = `<!DOCTYPE html>
<html>
<head>
  <title>Test page</title>
  <style>body { color: red; }</style>
  <script>var excluded = "script content";</script>
</head>
<body>
  <h1>Heading</h1>
  <p>First paragraph with <b>bold</b> text.</p>
  <p>Second paragraph</p>
  <div><span>Div</span> content</div>
</body>
</html>`

func TestDetect(t *testing.T) {
	tests := []struct {
		name        string
		fileName    string
		contentType string
		data        []byte
		want        string
	}{
		{"pdf magic", "doc.pdf", "", buildPDF("Hello World"), TypePDF},
		{"docx magic", "doc.docx", "", docxFixture(t), TypeDOCX},
		{"pptx magic", "deck.pptx", "", pptxFixture(t), TypePPTX},
		{"xlsx magic", "book.xlsx", "", xlsxFixture(t), TypeXLSX},
		{"html magic", "page", "", []byte(htmlFixture), TypeHTML},
		{"json magic", "data", "", []byte(`{"key": "value"}`), TypeJSON},
		{"markdown by extension", "README.md", "", []byte("# Title\n\nSome body text."), TypeMarkdown},
		{"yaml by extension", "config.yaml", "", []byte("key: value\nother: 2\n"), TypeYAML},
		{"yml by extension", "config.yml", "text/plain", []byte("key: value\n"), TypeYAML},
		{"csv by extension", "table.csv", "", []byte("name,age\nalice,30\n"), TypeCSV},
		{"xml by extension", "doc.xml", "", []byte("<root><item>1</item></root>"), TypeXML},
		{"html by extension", "page.html", "", []byte("<p>hello</p>"), TypeHTML},
		{"htm by extension", "page.htm", "", []byte("<p>hello</p>"), TypeHTML},
		{"html by content type", "page", "text/html; charset=utf-8", []byte("<p>hello</p>"), TypeHTML},
		{"markdown by content type", "notes", "text/markdown", []byte("# Title"), TypeMarkdown},
		{"plain text", "notes.txt", "", []byte("just some text"), TypeTextPlain},
		{"plain text no name", "", "", []byte("just some text"), TypeTextPlain},
		{"unknown binary", "blob.bin", "", bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0x00}, 8), TypeOctetStream},
		{"png magic", "image.png", "", []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D}, "image/png"},
		{"empty data", "anything.txt", "text/html", nil, TypeTextPlain},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Detect(tt.fileName, tt.contentType, tt.data); got != tt.want {
				t.Errorf("Detect(%q, %q) = %q, want %q", tt.fileName, tt.contentType, got, tt.want)
			}
		})
	}
}

func TestExtract(t *testing.T) {
	tests := []struct {
		name          string
		fileName      string
		contentType   string
		data          []byte
		wantType      string
		wantText      string
		wantExtracted bool
		wantErr       string
	}{
		{
			name:          "pdf single page",
			fileName:      "doc.pdf",
			data:          buildPDF("Hello PDF world"),
			wantType:      TypePDF,
			wantText:      "Hello PDF world",
			wantExtracted: true,
		},
		{
			name:          "pdf multiple pages",
			fileName:      "doc.pdf",
			data:          buildPDF("Page one text", "Page two text"),
			wantType:      TypePDF,
			wantText:      "Page one text\nPage two text",
			wantExtracted: true,
		},
		{
			name:          "docx paragraphs",
			fileName:      "doc.docx",
			data:          docxFixture(t),
			wantType:      TypeDOCX,
			wantText:      "Hello document\nSecond & final paragraph",
			wantExtracted: true,
		},
		{
			name:          "pptx slides in order",
			fileName:      "deck.pptx",
			data:          pptxFixture(t),
			wantType:      TypePPTX,
			wantText:      "Slide one title\nSlide one body\nSlide two text",
			wantExtracted: true,
		},
		{
			name:          "xlsx shared and inline strings",
			fileName:      "book.xlsx",
			data:          xlsxFixture(t),
			wantType:      TypeXLSX,
			wantText:      "Shared value one\nShared value two\nInline cell A\nInline cell B",
			wantExtracted: true,
		},
		{
			name:          "html strips script and style",
			fileName:      "page.html",
			data:          []byte(htmlFixture),
			wantType:      TypeHTML,
			wantText:      "Test page\nHeading\nFirst paragraph with bold text.\nSecond paragraph\nDiv content",
			wantExtracted: true,
		},
		{
			name:          "markdown pass-through",
			fileName:      "README.md",
			data:          []byte("# Title\n\nSome body text."),
			wantType:      TypeMarkdown,
			wantText:      "# Title\n\nSome body text.",
			wantExtracted: false,
		},
		{
			name:          "json pass-through",
			fileName:      "data.json",
			data:          []byte(`{"key": "value"}`),
			wantType:      TypeJSON,
			wantText:      `{"key": "value"}`,
			wantExtracted: false,
		},
		{
			name:          "plain text pass-through",
			fileName:      "notes.txt",
			data:          []byte("just some text\n"),
			wantType:      TypeTextPlain,
			wantText:      "just some text\n",
			wantExtracted: false,
		},
		{
			name:     "empty data",
			fileName: "doc.txt",
			data:     nil,
			wantErr:  "empty",
		},
		{
			name:     "unknown binary",
			fileName: "blob.bin",
			data:     bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0x00}, 8),
			wantErr:  `unsupported document type "application/octet-stream"`,
		},
		{
			name:     "png binary",
			fileName: "image.png",
			data:     []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D},
			wantErr:  `unsupported document type "image/png"`,
		},
		{
			name:     "corrupt pdf",
			fileName: "doc.pdf",
			data:     []byte("%PDF-1.4\nthis is not a valid pdf body"),
			wantErr:  "scanned",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Extract(tt.fileName, tt.contentType, tt.data)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Extract() error = nil, want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Extract() error = %q, want containing %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Extract() error = %v", err)
			}
			if got.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", got.Type, tt.wantType)
			}
			if got.Text != tt.wantText {
				t.Errorf("Text = %q, want %q", got.Text, tt.wantText)
			}
			if got.Extracted != tt.wantExtracted {
				t.Errorf("Extracted = %v, want %v", got.Extracted, tt.wantExtracted)
			}
		})
	}
}

// TestExtractHTMLExcludesHiddenContent verifies script/style/noscript content
// never leaks into the extracted text.
func TestExtractHTMLExcludesHiddenContent(t *testing.T) {
	data := []byte(htmlFixture)
	res, err := Extract("page.html", "", data)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	for _, excluded := range []string{"excluded", "script content", "color: red", "color"} {
		if strings.Contains(res.Text, excluded) {
			t.Errorf("extracted text contains %q:\n%s", excluded, res.Text)
		}
	}
}
