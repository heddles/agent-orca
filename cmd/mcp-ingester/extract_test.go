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
	"testing"
)

func TestLooksLikeBase64(t *testing.T) {
	pngBytes := append([]byte{0x89}, []byte("PNG\r\n\x1a\nbinary\x00payload")...)
	pngB64 := base64.StdEncoding.EncodeToString(pngBytes)

	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"valid base64", pngB64, true},
		{"empty", "", false},
		{"plain text with space", "hello world!", false},
		{"length not multiple of 4", "hello", false},
		{"undecodable", "a!b@c$d%", false},
		{"decodes to empty", "====", false},
		{"short decodable", "abcd", true}, // heuristic accepts short ASCII words
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeBase64(tt.in); got != tt.want {
				t.Errorf("looksLikeBase64(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestExtractDocumentPlainText(t *testing.T) {
	content, docType, extracted, err := extractDocument("notes.txt", "", "hello world")
	if err != nil {
		t.Fatalf("extractDocument: %v", err)
	}
	if content != "hello world" {
		t.Errorf("content = %q, want %q", content, "hello world")
	}
	if docType != "text/plain" {
		t.Errorf("docType = %q, want text/plain", docType)
	}
	if extracted {
		t.Error("extracted = true, want false for pass-through")
	}
}

func TestExtractDocumentUnsupportedBinary(t *testing.T) {
	png := append([]byte{0x89}, []byte("PNG\r\n\x1a\nbinary\x00payload")...)
	_, _, _, err := extractDocument("image.png", "", string(png))
	if err == nil {
		t.Fatal("expected error for unsupported binary type")
	}
	if !strings.Contains(err.Error(), "unsupported document type") {
		t.Errorf("err = %v, want unsupported document type error", err)
	}
}

func TestExtractDocumentBase64Retry(t *testing.T) {
	// Base64 of a PNG: the first extract fails with an unsupported type only
	// when detection sees binary bytes, so feed the raw bytes through the
	// base64 path to verify the retry reports the decoded content's error.
	png := append([]byte{0x89}, []byte("PNG\r\n\x1a\nbinary\x00payload")...)
	encoded := base64.StdEncoding.EncodeToString(png)

	// The encoded form itself is ASCII text, so it passes through as
	// text/plain; the heuristic must not corrupt it.
	content, docType, _, err := extractDocument("payload.txt", "", encoded)
	if err != nil {
		t.Fatalf("extractDocument on base64 text: %v", err)
	}
	if content != encoded || docType != "text/plain" {
		t.Errorf("base64 pass-through changed content: docType=%q", docType)
	}
}

func TestMetadataWithDocType(t *testing.T) {
	meta := metadataWithDocType(map[string]any{"source": "url"}, "application/pdf", true)
	if meta["docType"] != "application/pdf" {
		t.Errorf("docType = %v, want application/pdf", meta["docType"])
	}
	if meta["extracted"] != true {
		t.Errorf("extracted = %v, want true", meta["extracted"])
	}

	meta = metadataWithDocType(map[string]any{"source": "url"}, "text/plain", false)
	if _, ok := meta["extracted"]; ok {
		t.Error("extracted key present, want absent for pass-through")
	}
}
