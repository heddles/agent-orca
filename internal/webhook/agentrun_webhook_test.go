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

package webhook

import (
	"testing"
)

func TestCheckRegistryAllowlist(t *testing.T) {
	tests := []struct {
		name              string
		ociRef            string
		allowedRegistries []string
		wantErr           bool
	}{
		{
			name:              "empty allowlist allows anything",
			ociRef:            "evil.io/malware:latest",
			allowedRegistries: nil,
			wantErr:           false,
		},
		{
			name:              "exact match",
			ociRef:            "gcr.io/my-project",
			allowedRegistries: []string{"gcr.io/my-project"},
			wantErr:           false,
		},
		{
			name:              "prefix with sub-path",
			ociRef:            "gcr.io/my-project/image:v1",
			allowedRegistries: []string{"gcr.io/my-project"},
			wantErr:           false,
		},
		{
			name:              "bypass: prefix must not match across path boundary",
			ociRef:            "gcr.io/my-evil-org/image:v1",
			allowedRegistries: []string{"gcr.io/my"},
			wantErr:           true,
		},
		{
			name:              "bypass: similar project name",
			ociRef:            "gcr.io/my-project-evil/image:v1",
			allowedRegistries: []string{"gcr.io/my-project"},
			wantErr:           true,
		},
		{
			name:              "allowed prefix with trailing slash",
			ociRef:            "gcr.io/my-project/image:v1",
			allowedRegistries: []string{"gcr.io/my-project/"},
			wantErr:           false,
		},
		{
			name:              "trailing slash does not match different prefix",
			ociRef:            "gcr.io/my-project-evil/image:v1",
			allowedRegistries: []string{"gcr.io/my-project/"},
			wantErr:           true,
		},
		{
			name:              "registry-only prefix",
			ociRef:            "docker.io/library/nginx:latest",
			allowedRegistries: []string{"docker.io"},
			wantErr:           false,
		},
		{
			name:              "no match among multiple registries",
			ociRef:            "evil.io/malware:latest",
			allowedRegistries: []string{"gcr.io/my-project", "docker.io/library"},
			wantErr:           true,
		},
		{
			name:              "match second entry in list",
			ociRef:            "docker.io/library/nginx:latest",
			allowedRegistries: []string{"gcr.io/my-project", "docker.io/library"},
			wantErr:           false,
		},
		{
			name:              "deeply nested image under allowed prefix",
			ociRef:            "gcr.io/my-project/team/service/image:v2",
			allowedRegistries: []string{"gcr.io/my-project"},
			wantErr:           false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkRegistryAllowlist(tt.ociRef, tt.allowedRegistries)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkRegistryAllowlist(%q, %v) error = %v, wantErr %v",
					tt.ociRef, tt.allowedRegistries, err, tt.wantErr)
			}
		})
	}
}
