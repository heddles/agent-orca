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

package controller

import (
	"testing"
)

func TestParseQdrantVersion(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    qdrantVersion
		wantErr bool
	}{
		{
			name:  "full semver",
			input: "1.17.1",
			want:  qdrantVersion{Major: 1, Minor: 17, Patch: 1},
		},
		{
			name:  "with v prefix",
			input: "v1.13.6",
			want:  qdrantVersion{Major: 1, Minor: 13, Patch: 6},
		},
		{
			name:  "major.minor only",
			input: "1.12",
			want:  qdrantVersion{Major: 1, Minor: 12, Patch: 0},
		},
		{
			name:  "major.minor with v prefix",
			input: "v1.15",
			want:  qdrantVersion{Major: 1, Minor: 15, Patch: 0},
		},
		{
			name:  "zero patch",
			input: "1.14.0",
			want:  qdrantVersion{Major: 1, Minor: 14, Patch: 0},
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:    "garbage",
			input:   "latest",
			wantErr: true,
		},
		{
			name:    "just a number",
			input:   "17",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseQdrantVersion(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseQdrantVersion(%q) expected error, got %v", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseQdrantVersion(%q) unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("parseQdrantVersion(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestNeedsUpgrade(t *testing.T) {
	tests := []struct {
		name    string
		current qdrantVersion
		target  qdrantVersion
		want    bool
	}{
		{
			name:    "same version",
			current: qdrantVersion{1, 17, 1},
			target:  qdrantVersion{1, 17, 1},
			want:    false,
		},
		{
			name:    "patch only difference",
			current: qdrantVersion{1, 17, 0},
			target:  qdrantVersion{1, 17, 1},
			want:    false,
		},
		{
			name:    "minor behind",
			current: qdrantVersion{1, 12, 6},
			target:  qdrantVersion{1, 17, 1},
			want:    true,
		},
		{
			name:    "one minor behind",
			current: qdrantVersion{1, 16, 1},
			target:  qdrantVersion{1, 17, 1},
			want:    true,
		},
		{
			name:    "ahead of target",
			current: qdrantVersion{1, 17, 1},
			target:  qdrantVersion{1, 16, 0},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := needsUpgrade(tt.current, tt.target)
			if got != tt.want {
				t.Errorf("needsUpgrade(%v, %v) = %v, want %v", tt.current, tt.target, got, tt.want)
			}
		})
	}
}

func TestQdrantUpgradePath(t *testing.T) {
	tests := []struct {
		name    string
		current qdrantVersion
		target  qdrantVersion
		want    []qdrantVersion
		wantErr bool
	}{
		{
			name:    "same version",
			current: qdrantVersion{1, 17, 1},
			target:  qdrantVersion{1, 17, 1},
			want:    nil,
		},
		{
			name:    "patch only — no path",
			current: qdrantVersion{1, 17, 0},
			target:  qdrantVersion{1, 17, 1},
			want:    nil,
		},
		{
			name:    "one minor step",
			current: qdrantVersion{1, 16, 1},
			target:  qdrantVersion{1, 17, 1},
			want:    []qdrantVersion{{1, 17, 1}},
		},
		{
			name:    "multiple minor steps",
			current: qdrantVersion{1, 12, 6},
			target:  qdrantVersion{1, 15, 2},
			want: []qdrantVersion{
				{1, 13, 6}, // latest known patch for 1.13
				{1, 14, 1}, // latest known patch for 1.14
				{1, 15, 2}, // target patch for final step
			},
		},
		{
			name:    "full path 12 to 17",
			current: qdrantVersion{1, 12, 0},
			target:  qdrantVersion{1, 17, 1},
			want: []qdrantVersion{
				{1, 13, 6},
				{1, 14, 1},
				{1, 15, 2},
				{1, 16, 1},
				{1, 17, 1},
			},
		},
		{
			name:    "major version mismatch",
			current: qdrantVersion{1, 12, 0},
			target:  qdrantVersion{2, 0, 0},
			wantErr: true,
		},
		{
			name:    "target behind current — no path",
			current: qdrantVersion{1, 17, 1},
			target:  qdrantVersion{1, 12, 6},
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := qdrantUpgradePath(tt.current, tt.target)
			if tt.wantErr {
				if err == nil {
					t.Errorf("qdrantUpgradePath(%v, %v) expected error, got %v", tt.current, tt.target, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("qdrantUpgradePath(%v, %v) unexpected error: %v", tt.current, tt.target, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("qdrantUpgradePath(%v, %v) returned %d steps, want %d: %v",
					tt.current, tt.target, len(got), len(tt.want), got)
			}
			for i, v := range got {
				if v != tt.want[i] {
					t.Errorf("step %d: got %v, want %v", i, v, tt.want[i])
				}
			}
		})
	}
}

func TestQdrantVersionString(t *testing.T) {
	v := qdrantVersion{1, 17, 1}
	if got := v.String(); got != "1.17.1" {
		t.Errorf("String() = %q, want %q", got, "1.17.1")
	}
	if got := v.ImageTag(); got != "v1.17.1" {
		t.Errorf("ImageTag() = %q, want %q", got, "v1.17.1")
	}
	if got := v.Image("qdrant/qdrant"); got != "qdrant/qdrant:v1.17.1" {
		t.Errorf("Image() = %q, want %q", got, "qdrant/qdrant:v1.17.1")
	}
}

func TestImageTagAndRepo(t *testing.T) {
	tests := []struct {
		image    string
		wantTag  string
		wantRepo string
	}{
		{"qdrant/qdrant:v1.17.1", "v1.17.1", "qdrant/qdrant"},
		{"registry.io/qdrant/qdrant:v1.12.6", "v1.12.6", "registry.io/qdrant/qdrant"},
		{"qdrant/qdrant", "latest", "qdrant/qdrant"},
	}

	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			if got := imageTag(tt.image); got != tt.wantTag {
				t.Errorf("imageTag(%q) = %q, want %q", tt.image, got, tt.wantTag)
			}
			if got := imageRepo(tt.image); got != tt.wantRepo {
				t.Errorf("imageRepo(%q) = %q, want %q", tt.image, got, tt.wantRepo)
			}
		})
	}
}
