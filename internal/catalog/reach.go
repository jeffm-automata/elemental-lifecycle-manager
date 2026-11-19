/*
Copyright © 2026 SUSE LLC
SPDX-License-Identifier: Apache-2.0

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

package catalog

import (
	"fmt"
	"path"
	"sort"

	lifecyclev1alpha1 "github.com/suse/elemental-lifecycle-manager/api/v1alpha1"
)

// Current describes the release a cluster runs.
type Current struct {
	// Version is the applied release version (Release.status.version).
	Version string
	// Lineage is the name the cluster's release manifest states. May be empty when unknown.
	Lineage string
	// Registry is the repository the cluster's release came from (Release.spec.registry).
	// Used only when Lineage is unknown.
	Registry string
}

// Reach is the verdict on one catalog entry for one cluster.
type Reach struct {
	Reachable bool
	// Reason is a one-line explanation when the release is not reachable.
	Reason string
}

// ResolveLineage looks the lineage of the cluster's current release up in the catalog: the
// entry with the same version, preferring one from the same repository. Returns "" when the
// catalog does not hold the current release.
func ResolveLineage(current Current, releases []lifecyclev1alpha1.AvailableRelease) string {
	if current.Lineage != "" {
		return current.Lineage
	}
	lineage := ""
	for i := range releases {
		spec := &releases[i].Spec
		if spec.Version != current.Version {
			continue
		}
		if CleanRepository(spec.Registry) == CleanRepository(current.Registry) {
			return spec.Lineage
		}
		if lineage == "" {
			lineage = spec.Lineage
		}
	}
	return lineage
}

// Reachable decides whether a cluster on the current release can validly move to the candidate:
// the same lineage, and a version higher than the current one (the Lifecycle Manager's
// admission webhook refuses an equal or lower version). When the lineage of the current release
// is unknown, the last path element of the two repositories must be equal instead.
func Reachable(current Current, candidate lifecyclev1alpha1.AvailableReleaseSpec) Reach {
	cur, err := ParseVersion(current.Version)
	if err != nil {
		return Reach{Reason: fmt.Sprintf("the current version %q cannot be compared", current.Version)}
	}
	if _, err = ParseVersion(candidate.Version); err != nil {
		return Reach{Reason: fmt.Sprintf("version %q cannot be compared", candidate.Version)}
	}

	if current.Lineage != "" {
		if candidate.Lineage != current.Lineage {
			return Reach{Reason: fmt.Sprintf("other lineage (%s; the cluster runs %s)", candidate.Lineage, current.Lineage)}
		}
	} else if current.Registry == "" ||
		path.Base(CleanRepository(candidate.Registry)) != path.Base(CleanRepository(current.Registry)) {
		return Reach{Reason: "the lineage of the current release is unknown and the repository differs"}
	}

	// Compare returns -1 when cur is lower than the argument.
	cmp, err := cur.Compare(candidate.Version)
	if err != nil {
		return Reach{Reason: fmt.Sprintf("version %q cannot be compared", candidate.Version)}
	}
	switch {
	case cmp == 0:
		return Reach{Reason: "is the current release"}
	case cmp > 0:
		return Reach{Reason: fmt.Sprintf("lower than the current release %s (downgrades are refused)", current.Version)}
	}
	return Reach{Reachable: true}
}

// Candidate is a catalog entry with its verdict for one cluster.
type Candidate struct {
	Release lifecyclev1alpha1.AvailableRelease
	Reach   Reach
}

// Evaluate applies Reachable to every catalog entry, resolving the current lineage from the
// catalog first. The result is sorted: highest version first.
func Evaluate(current Current, releases []lifecyclev1alpha1.AvailableRelease) []Candidate {
	current.Lineage = ResolveLineage(current, releases)
	out := make([]Candidate, 0, len(releases))
	for i := range releases {
		out = append(out, Candidate{Release: releases[i], Reach: Reachable(current, releases[i].Spec)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		vi, erri := ParseVersion(out[i].Release.Spec.Version)
		if erri != nil {
			return false
		}
		cmp, err := vi.Compare(out[j].Release.Spec.Version)
		if err != nil {
			return true
		}
		if cmp != 0 {
			return cmp > 0
		}
		return out[i].Release.Name < out[j].Release.Name
	})
	return out
}
