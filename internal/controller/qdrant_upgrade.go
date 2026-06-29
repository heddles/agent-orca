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
	"fmt"
	"strings"
)

// qdrantVersion represents a parsed Qdrant semver.
type qdrantVersion struct {
	Major int
	Minor int
	Patch int
}

func (v qdrantVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// ImageTag returns the version as a Docker image tag (e.g. "v1.17.1").
func (v qdrantVersion) ImageTag() string {
	return "v" + v.String()
}

// Image returns the full image reference (e.g. "qdrant/qdrant:v1.17.1").
func (v qdrantVersion) Image(repo string) string {
	return repo + ":" + v.ImageTag()
}

// qdrantLatestPatch maps each known Qdrant minor version to the latest
// known-good patch release. Updated when the operator bumps its go-client
// dependency.
var qdrantLatestPatch = map[int]int{
	7:  4,
	8:  4,
	9:  7,
	10: 1,
	11: 5,
	12: 6,
	13: 6,
	14: 1,
	15: 2,
	16: 1,
	17: 1,
}

// parseQdrantVersion parses a version string like "1.13.2" or "v1.13.2".
func parseQdrantVersion(s string) (qdrantVersion, error) {
	s = strings.TrimPrefix(s, "v")
	var v qdrantVersion
	n, err := fmt.Sscanf(s, "%d.%d.%d", &v.Major, &v.Minor, &v.Patch)
	if err != nil || n != 3 {
		// Try major.minor only (e.g. "1.13").
		n, err = fmt.Sscanf(s, "%d.%d", &v.Major, &v.Minor)
		if err != nil || n != 2 {
			return qdrantVersion{}, fmt.Errorf("invalid qdrant version %q", s)
		}
	}
	return v, nil
}

// needsUpgrade returns true if current is behind target at the minor level.
// Patch-only differences within the same minor are not considered upgrades
// since Qdrant handles those transparently.
func needsUpgrade(current, target qdrantVersion) bool {
	if current.Major != target.Major {
		return current.Major < target.Major
	}
	return current.Minor < target.Minor
}

// qdrantUpgradePath returns the ordered list of intermediate versions between
// current (exclusive) and target (inclusive). Each intermediate step uses the
// latest known-good patch for that minor version from qdrantLatestPatch.
//
// Returns an error if the major versions differ or the path would be a
// downgrade.
func qdrantUpgradePath(current, target qdrantVersion) ([]qdrantVersion, error) {
	if current.Major != target.Major {
		return nil, fmt.Errorf("cannot upgrade across major versions: %s → %s", current, target)
	}
	if !needsUpgrade(current, target) {
		return nil, nil // already at or ahead of target
	}

	var path []qdrantVersion
	for minor := current.Minor + 1; minor <= target.Minor; minor++ {
		patch, ok := qdrantLatestPatch[minor]
		if !ok {
			return nil, fmt.Errorf("no known patch version for qdrant v%d.%d; update qdrantLatestPatch table", current.Major, minor)
		}
		v := qdrantVersion{Major: current.Major, Minor: minor, Patch: patch}
		// Use the actual target patch for the final step.
		if minor == target.Minor {
			v.Patch = target.Patch
		}
		path = append(path, v)
	}
	return path, nil
}
