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
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/ledongthuc/pdf"
	"golang.org/x/net/html"
)

// Result holds the outcome of document detection and extraction.
type Result struct {
	Type      string // detected MIME type, e.g. "application/pdf", "text/html"
	Text      string // extracted plain text
	Extracted bool   // true when a type-specific extractor produced the text (false = pass-through)
}

// Extract detects the document type and returns extracted plain text.
// Returns an error for empty data and for known-binary types that fail
// extraction. Unknown binary types return a descriptive error so the caller
// can skip the document.
func Extract(name, contentType string, data []byte) (*Result, error) {
	if len(data) == 0 {
		return nil, errors.New("extract: document is empty")
	}

	docType := Detect(name, contentType, data)

	var (
		text      string
		err       error
		extracted = true
	)
	switch docType {
	case TypePDF:
		text, err = extractPDF(data)
	case TypeDOCX:
		text, err = extractDOCX(data)
	case TypePPTX:
		text, err = extractPPTX(data)
	case TypeXLSX:
		text, err = extractXLSX(data)
	case TypeHTML:
		text, err = extractHTML(data)
	default:
		if !isTextType(docType) {
			return nil, fmt.Errorf("unsupported document type %q", docType)
		}
		text, extracted = string(data), false
	}
	if err != nil {
		return nil, err
	}
	return &Result{Type: docType, Text: text, Extracted: extracted}, nil
}

// isTextType reports whether docType is a plain-text format that can be
// ingested as-is without a type-specific extractor.
func isTextType(docType string) bool {
	switch docType {
	case TypeJSON, TypeYAML, TypeXML, TypeMarkdown, TypeCSV:
		return true
	}
	return strings.HasPrefix(docType, "text/")
}

// extractPDF extracts the text layer of a PDF. It fails when the PDF cannot
// be parsed or has no text layer, which usually means the document is
// scanned or encrypted.
func extractPDF(data []byte) (string, error) {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("extracting pdf: %w (the PDF may be scanned, encrypted, or lack a text layer)", err)
	}

	var sb strings.Builder
	// Page numbers are 1-indexed in the pdf package.
	for i := 1; i <= r.NumPage(); i++ {
		pageText, err := r.Page(i).GetPlainText(nil)
		if err != nil {
			return "", fmt.Errorf("extracting pdf page %d: %w (the PDF may be scanned, encrypted, or lack a text layer)", i+1, err)
		}
		sb.WriteString(pageText)
	}
	if sb.Len() == 0 {
		return "", errors.New("extracting pdf: no text layer found (the PDF may be scanned or image-only)")
	}
	return normalizeText(sb.String()), nil
}

// xmlTextSpec describes which XML elements of an OOXML part carry text and
// which elements terminate a line. Elements are matched by local name so
// namespace prefixes are ignored.
type xmlTextSpec struct {
	textElems  map[string]bool // elements whose character data is collected
	breakElems map[string]bool // elements whose end starts a new line
}

var (
	docxSpec = xmlTextSpec{
		textElems:  map[string]bool{"t": true},
		breakElems: map[string]bool{"p": true, "br": true},
	}
	pptxSpec = xmlTextSpec{
		textElems:  map[string]bool{"t": true},
		breakElems: map[string]bool{"p": true},
	}
	// sharedStrings: one line per <si> entry (covers both plain <si><t> and
	// rich-text <si><r><t> runs).
	xlsxSharedSpec = xmlTextSpec{
		textElems:  map[string]bool{"t": true},
		breakElems: map[string]bool{"si": true},
	}
	// worksheets: inline strings only; one line per row.
	xlsxInlineSpec = xmlTextSpec{
		textElems:  map[string]bool{"t": true},
		breakElems: map[string]bool{"is": true, "row": true},
	}
)

// extractDOCX extracts paragraph text from word/document.xml.
func extractDOCX(data []byte) (string, error) {
	text, err := zipXMLText(data, "word/document.xml", docxSpec)
	if err != nil {
		return "", fmt.Errorf("extracting docx: %w", err)
	}
	return text, nil
}

// extractPPTX extracts text from all slides in presentation order.
func extractPPTX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("extracting pptx: %w", err)
	}

	slides, err := numberedParts(zr, "ppt/slides", "slide")
	if err != nil {
		return "", fmt.Errorf("extracting pptx: %w", err)
	}

	var sb strings.Builder
	for _, part := range slides {
		text, err := readZipXMLText(zr, part.name, pptxSpec)
		if err != nil {
			return "", fmt.Errorf("extracting pptx: %w", err)
		}
		sb.WriteString(text)
		sb.WriteByte('\n')
	}
	return normalizeText(sb.String()), nil
}

// extractXLSX extracts cell text from xl/sharedStrings.xml plus inline strings
// from the worksheets. This is an approximation that is sufficient for RAG
// ingestion: numeric, boolean, and formula cell values are not rendered.
func extractXLSX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("extracting xlsx: %w", err)
	}

	var sb strings.Builder

	// Shared strings referenced by cells across all sheets.
	if rc, openErr := zipOpen(zr, "xl/sharedStrings.xml"); openErr == nil {
		text, err := xmlText(rc, xlsxSharedSpec)
		_ = rc.Close()
		if err != nil {
			return "", fmt.Errorf("extracting xlsx: reading shared strings: %w", err)
		}
		sb.WriteString(text)
		sb.WriteByte('\n')
	}

	// Inline strings in worksheets, in sheet order.
	sheets, err := numberedParts(zr, "xl/worksheets", "sheet")
	if err != nil {
		return "", fmt.Errorf("extracting xlsx: %w", err)
	}
	for _, part := range sheets {
		text, err := readZipXMLText(zr, part.name, xlsxInlineSpec)
		if err != nil {
			return "", fmt.Errorf("extracting xlsx: %w", err)
		}
		sb.WriteString(text)
		sb.WriteByte('\n')
	}
	return normalizeText(sb.String()), nil
}

// zipXMLText reads the entry named exactly name from the zip archive in data
// and extracts text from its XML per spec.
func zipXMLText(data []byte, name string, spec xmlTextSpec) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	return readZipXMLText(zr, name, spec)
}

// readZipXMLText extracts text from the XML of the zip entry named name.
func readZipXMLText(zr *zip.Reader, name string, spec xmlTextSpec) (string, error) {
	rc, err := zipOpen(zr, name)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	return xmlText(rc, spec)
}

// zipOpen finds the entry named exactly name and returns a reader for it.
func zipOpen(zr *zip.Reader, name string) (io.ReadCloser, error) {
	for _, f := range zr.File {
		if f.Name == name {
			return f.Open()
		}
	}
	return nil, fmt.Errorf("zip entry %q not found", name)
}

// numberedPart is a zip entry with the integer parsed from its file name.
type numberedPart struct {
	name string
	num  int
}

// numberedParts collects zip entries in dir whose file name is
// "<prefix><n>.xml" with n >= 1, sorted by n. It errors when none are found.
func numberedParts(zr *zip.Reader, dir, prefix string) ([]numberedPart, error) {
	var parts []numberedPart
	for _, f := range zr.File {
		if path.Dir(f.Name) != dir {
			continue
		}
		n, ok := xmlPartNumber(f.Name, prefix)
		if !ok {
			continue
		}
		parts = append(parts, numberedPart{name: f.Name, num: n})
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("no %s parts found in archive", dir)
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].num < parts[j].num })
	return parts, nil
}

// xmlPartNumber parses the integer in a part file name like "slide12.xml"
// ("prefix" + number + ".xml"). Returns ok=false for anything else.
func xmlPartNumber(name, prefix string) (int, bool) {
	base := path.Base(name)
	if !strings.HasPrefix(base, prefix) || !strings.HasSuffix(base, ".xml") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(base, prefix), ".xml"))
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// xmlText extracts text from an XML stream: character data inside textElems
// elements is collected, and the end of a breakElems element starts a new
// line.
func xmlText(r io.Reader, spec xmlTextSpec) (string, error) {
	dec := xml.NewDecoder(r)
	var sb strings.Builder
	inText := 0 // nesting depth inside textElems elements
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if spec.textElems[t.Name.Local] {
				inText++
			}
		case xml.EndElement:
			if spec.textElems[t.Name.Local] && inText > 0 {
				inText--
			}
			if spec.breakElems[t.Name.Local] {
				sb.WriteByte('\n')
			}
		case xml.CharData:
			if inText > 0 {
				sb.Write(t)
			}
		}
	}
	return normalizeText(sb.String()), nil
}

// Elements rendered as line boundaries during HTML extraction.
var blockElements = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true,
	"details": true, "dialog": true, "dd": true, "div": true, "dl": true,
	"dt": true, "fieldset": true, "figcaption": true, "figure": true,
	"footer": true, "form": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "header": true, "hgroup": true,
	"hr": true, "li": true, "main": true, "nav": true, "ol": true,
	"p": true, "pre": true, "section": true, "table": true, "tbody": true,
	"td": true, "tfoot": true, "th": true, "thead": true, "tr": true,
	"ul": true,
}

// Elements whose entire subtree is dropped during HTML extraction.
var skipElements = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true,
}

// extractHTML renders an HTML document to plain text: script, style,
// noscript, and template content is dropped, block-level elements become line
// boundaries, and whitespace is collapsed.
func extractHTML(data []byte) (string, error) {
	root, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("extracting html: %w", err)
	}

	var sb strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			sb.WriteString(collapseSpace(n.Data))
			return
		case html.ElementNode:
			if skipElements[n.Data] {
				return
			}
			if n.Data == "br" {
				sb.WriteByte('\n')
				return
			}
			if blockElements[n.Data] {
				sb.WriteByte('\n')
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && blockElements[n.Data] {
			sb.WriteByte('\n')
		}
	}
	walk(root)
	return normalizeText(sb.String()), nil
}

// collapseSpace replaces runs of whitespace with a single space, preserving
// single leading/trailing spaces so inline elements stay separated.
func collapseSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	spacePending := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			spacePending = true
			continue
		}
		if spacePending {
			b.WriteByte(' ')
			spacePending = false
		}
		b.WriteRune(r)
	}
	if spacePending {
		b.WriteByte(' ')
	}
	return b.String()
}

// normalizeText trims each line and drops empty lines, producing stable,
// compact output for RAG chunking.
func normalizeText(s string) string {
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}
