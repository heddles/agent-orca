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
	"bytes"
	"mime"
	"path"
	"strings"

	"github.com/gabriel-vasile/mimetype"
)

// MIME types recognized by this package.
const (
	TypeTextPlain   = "text/plain"
	TypeOctetStream = "application/octet-stream"
	TypeMarkdown    = "text/markdown"
	TypeJSON        = "application/json"
	TypeYAML        = "application/yaml"
	TypeCSV         = "text/csv"
	TypeXML         = "text/xml"
	TypeHTML        = "text/html"
	TypePDF         = "application/pdf"
	TypeDOCX        = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	TypePPTX        = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	TypeXLSX        = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

// Detect identifies the document type from magic bytes, falling back to
// contentType header and file name extension.
func Detect(name, contentType string, data []byte) string {
	if len(data) == 0 {
		return TypeTextPlain
	}

	// Magic bytes are authoritative when they identify anything other than
	// generic text.
	detected := mimetype.Detect(data).String()
	// mimetype appends parameters such as "; charset=utf-8" to text types.
	if base, _, err := mime.ParseMediaType(detected); err == nil {
		detected = base
	}
	switch detected {
	case "application/xml":
		detected = TypeXML
	case "text/yaml":
		detected = TypeYAML
	}
	if detected != TypeTextPlain {
		return detected
	}

	// mimetype misclassifies some binary content as text; a NUL byte is a
	// reliable binary indicator for document ingestion.
	if bytes.IndexByte(data, 0) >= 0 {
		return TypeOctetStream
	}

	// mimetype reports generic text/plain for markdown, JSON, YAML, CSV, and
	// XML, so refine using the file name extension, then the content type.
	if t := typeFromExtension(path.Ext(name)); t != "" {
		return t
	}
	if t := typeFromContentType(contentType); t != "" {
		return t
	}
	return TypeTextPlain
}

// typeFromExtension maps a file extension to a MIME type for the text formats
// that magic-byte detection cannot distinguish. Returns "" when unknown.
func typeFromExtension(ext string) string {
	switch strings.ToLower(ext) {
	case ".html", ".htm":
		return TypeHTML
	case ".md", ".markdown":
		return TypeMarkdown
	case ".json":
		return TypeJSON
	case ".yaml", ".yml":
		return TypeYAML
	case ".csv":
		return TypeCSV
	case ".xml":
		return TypeXML
	default:
		return ""
	}
}

// typeFromContentType maps a Content-Type header value to a MIME type for the
// text formats that magic-byte detection cannot distinguish. Returns "" when
// unknown.
func typeFromContentType(contentType string) string {
	if contentType == "" {
		return ""
	}
	base, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		// Best-effort parse of a non-conforming header value.
		base = contentType
		if i := strings.IndexByte(base, ';'); i >= 0 {
			base = base[:i]
		}
		base = strings.ToLower(strings.TrimSpace(base))
	}
	switch base {
	case TypeHTML, TypeMarkdown, TypeJSON, TypeYAML, TypeCSV, TypeXML:
		return base
	case "text/yaml":
		return TypeYAML
	case "application/xml":
		return TypeXML
	default:
		return ""
	}
}
