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

package rag

import (
	"strings"
	"unicode"
)

// ChunkConfig controls how documents are split into chunks.
type ChunkConfig struct {
	ChunkSize    int // approximate max tokens per chunk (default 512)
	ChunkOverlap int // tokens of overlap between consecutive chunks (default 64)
}

// DefaultChunkConfig returns sensible defaults.
func DefaultChunkConfig() ChunkConfig {
	return ChunkConfig{ChunkSize: 512, ChunkOverlap: 64}
}

// Chunk represents a segment of a document.
type Chunk struct {
	Index    int
	Text     string
	DocID    string
	Metadata map[string]interface{}
}

// ChunkText splits a document into overlapping chunks.
// Uses word-count as a token approximation (1 token ≈ 0.75 words, so we use words * 0.75).
func ChunkText(docID, text string, cfg ChunkConfig, metadata map[string]interface{}) []Chunk {
	if cfg.ChunkSize <= 0 {
		cfg.ChunkSize = 512
	}
	if cfg.ChunkOverlap < 0 {
		cfg.ChunkOverlap = 0
	}
	if cfg.ChunkOverlap >= cfg.ChunkSize {
		cfg.ChunkOverlap = cfg.ChunkSize / 4
	}

	// Split into sentences for cleaner chunk boundaries.
	sentences := splitSentences(text)
	if len(sentences) == 0 {
		return nil
	}

	// Convert token counts to word counts (tokens ≈ words * 1.33).
	maxWords := int(float64(cfg.ChunkSize) / 1.33)
	overlapWords := int(float64(cfg.ChunkOverlap) / 1.33)
	if maxWords < 1 {
		maxWords = 1
	}

	var chunks []Chunk
	var currentWords []string
	var currentSentences []string
	wordCount := 0

	for _, sent := range sentences {
		sentWords := strings.Fields(sent)
		if len(sentWords) == 0 {
			continue
		}

		// If adding this sentence exceeds the limit and we have content, emit a chunk.
		if wordCount+len(sentWords) > maxWords && wordCount > 0 {
			chunkText := strings.Join(currentSentences, " ")
			chunks = append(chunks, Chunk{
				Index:    len(chunks),
				Text:     strings.TrimSpace(chunkText),
				DocID:    docID,
				Metadata: metadata,
			})

			// Keep overlap words from the end of current chunk.
			if overlapWords > 0 && len(currentWords) > overlapWords {
				overlapStart := len(currentWords) - overlapWords
				currentWords = currentWords[overlapStart:]
				// Rebuild sentences from overlap words.
				currentSentences = []string{strings.Join(currentWords, " ")}
				wordCount = len(currentWords)
			} else {
				currentWords = nil
				currentSentences = nil
				wordCount = 0
			}
		}

		currentWords = append(currentWords, sentWords...)
		currentSentences = append(currentSentences, sent)
		wordCount += len(sentWords)
	}

	// Emit remaining content.
	if wordCount > 0 {
		chunkText := strings.Join(currentSentences, " ")
		chunks = append(chunks, Chunk{
			Index:    len(chunks),
			Text:     strings.TrimSpace(chunkText),
			DocID:    docID,
			Metadata: metadata,
		})
	}

	return chunks
}

// splitSentences does basic sentence splitting on period, question mark, exclamation,
// and newlines. Preserves the delimiter with the sentence.
func splitSentences(text string) []string {
	var sentences []string
	var current strings.Builder

	for _, r := range text {
		current.WriteRune(r)
		if r == '.' || r == '?' || r == '!' || r == '\n' {
			s := strings.TrimSpace(current.String())
			if s != "" {
				sentences = append(sentences, s)
			}
			current.Reset()
		}
	}

	// Remaining text.
	s := strings.TrimSpace(current.String())
	if s != "" {
		sentences = append(sentences, s)
	}

	return sentences
}

// WordCount returns the approximate token count for a string.
func WordCount(s string) int {
	count := 0
	inWord := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			inWord = false
		} else if !inWord {
			inWord = true
			count++
		}
	}
	return count
}
