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

package main

import (
	"encoding/base64"
	"strings"
	"unicode"

	"github.com/heddles/agent-orca/internal/rag/extract"
)

// extractDocument runs type detection and text extraction on raw document
// bytes. For MCP results that are base64-encoded binaries (common for tools
// returning files), it retries once on the base64-decoded bytes.
func extractDocument(name, contentType, raw string) (content, docType string, extracted bool, err error) {
	result, err := extract.Extract(name, contentType, []byte(raw))
	if err != nil && isUnsupportedTypeErr(err) && looksLikeBase64(raw) {
		decoded, decErr := base64.StdEncoding.DecodeString(raw)
		if decErr == nil && len(decoded) > 0 {
			if retry, retryErr := extract.Extract(name, contentType, decoded); retryErr == nil {
				result, err = retry, nil
			} else {
				// Report the retry failure: it describes the decoded
				// content, which is what the document actually is.
				err = retryErr
			}
		}
	}
	if err != nil {
		return "", "", false, err
	}
	return result.Text, result.Type, result.Extracted, nil
}

// metadataWithDocType adds the document-type metadata produced by extraction
// to a document's base metadata map.
func metadataWithDocType(meta map[string]any, docType string, extracted bool) map[string]any {
	meta["docType"] = docType
	if extracted {
		meta["extracted"] = true
	}
	return meta
}

// isUnsupportedTypeErr reports whether err is the extract package's
// unsupported-binary-type error.
func isUnsupportedTypeErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unsupported document type")
}

// looksLikeBase64 heuristically reports whether s is a base64-encoded payload:
// non-empty, free of whitespace, a multiple of four characters long, and
// decodable with non-empty output.
func looksLikeBase64(s string) bool {
	if s == "" || strings.IndexFunc(s, unicode.IsSpace) >= 0 {
		return false
	}
	if len(s)%4 != 0 {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(s)
	return err == nil && len(decoded) > 0
}
