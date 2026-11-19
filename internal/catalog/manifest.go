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
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/suse/elemental/v3/pkg/manifest/api/core"
	"github.com/suse/elemental/v3/pkg/manifest/api/solution"
	"k8s.io/apimachinery/pkg/util/version"

	lifecyclev1alpha1 "github.com/suse/elemental-lifecycle-manager/api/v1alpha1"
)

// Info is what the catalog publishes about one release manifest.
type Info struct {
	Lineage         string
	CreationDate    string
	OperatingSystem lifecyclev1alpha1.AvailableOperatingSystem
	Kubernetes      lifecyclev1alpha1.AvailableKubernetes
	CorePlatform    *lifecyclev1alpha1.AvailableCorePlatform
}

// ParseVersion parses a release version the way the Lifecycle Manager's admission webhook does
// (k8s.io/apimachinery version.Parse), so that the catalog and the webhook order versions alike.
func ParseVersion(v string) (*version.Version, error) {
	if v == "" {
		return nil, errors.New("empty version")
	}
	parsed, err := version.Parse(v)
	if err != nil {
		return nil, fmt.Errorf("%q is not a version", v)
	}
	return parsed, nil
}

// tagVersion matches tags that name a release: they start with major.minor.patch.
var tagVersion = regexp.MustCompile(`^v?\d+\.\d+\.\d+([-+._][0-9A-Za-z][0-9A-Za-z.+_-]*)?$`)

// IsReleaseTag tells whether a registry tag names a release version. Tags such as "latest",
// signatures ("sha256-....sig") and attestations are not releases.
func IsReleaseTag(tag string) bool {
	if !tagVersion.MatchString(tag) {
		return false
	}
	_, err := ParseVersion(tag)
	return err == nil
}

// imageTag returns the tag of an image reference, or "" when it has none.
func imageTag(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		return ref[colon+1:]
	}
	return ""
}

// ReadCore reads the manifest of a core platform release referenced by a solution release.
type ReadCore func(ctx context.Context, ref string) ([]byte, error)

// ParseManifest extracts the catalog information from a release manifest. A solution manifest is
// resolved through readCore to the core platform release it pins. A manifest with no Kubernetes
// (an operating-system-only release) is refused with a NotAReleaseError: it is not a cluster version.
func ParseManifest(ctx context.Context, data []byte, readCore ReadCore) (*Info, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, &NotAReleaseError{Err: errors.New("empty release manifest")}
	}

	info := &Info{}
	coreData := data

	sol, solErr := solution.Parse(data)
	if solErr == nil && sol.CorePlatform != nil && sol.CorePlatform.Image != "" {
		if sol.Metadata == nil || sol.Metadata.Name == "" {
			return nil, &NotAReleaseError{Err: errors.New("solution manifest states no metadata.name")}
		}
		info.Lineage = sol.Metadata.Name
		info.CreationDate = sol.Metadata.CreationDate
		info.CorePlatform = &lifecyclev1alpha1.AvailableCorePlatform{Image: sol.CorePlatform.Image}

		if readCore == nil {
			return nil, errors.New("solution manifest, and no way to read its core platform manifest")
		}
		var err error
		coreData, err = readCore(ctx, strings.TrimPrefix(sol.CorePlatform.Image, "oci://"))
		if err != nil {
			return nil, fmt.Errorf("reading core platform manifest %s: %w", sol.CorePlatform.Image, err)
		}
	}

	c, err := core.Parse(coreData)
	if err != nil {
		if info.CorePlatform == nil && solErr != nil {
			err = errors.Join(solErr, err)
		}
		return nil, &NotAReleaseError{Err: fmt.Errorf("not a valid release manifest: %w", err)}
	}

	if info.CorePlatform != nil {
		if c.Metadata != nil {
			info.CorePlatform.Name = c.Metadata.Name
		}
	} else {
		if c.Metadata == nil || c.Metadata.Name == "" {
			return nil, &NotAReleaseError{Err: errors.New("manifest states no metadata.name")}
		}
		info.Lineage = c.Metadata.Name
		info.CreationDate = c.Metadata.CreationDate
	}

	if c.Components.Kubernetes == nil || c.Components.Kubernetes.Version == "" {
		return nil, &NotAReleaseError{Err: errors.New("operating system only (no Kubernetes): not a cluster release")}
	}
	if c.Components.OperatingSystem == nil || c.Components.OperatingSystem.Image.Base == "" {
		return nil, &NotAReleaseError{Err: errors.New("manifest states no operating system image")}
	}

	info.Kubernetes = lifecyclev1alpha1.AvailableKubernetes{
		Version: c.Components.Kubernetes.Version,
		Image:   c.Components.Kubernetes.Image,
	}
	info.OperatingSystem = lifecyclev1alpha1.AvailableOperatingSystem{
		Image:   c.Components.OperatingSystem.Image.Base,
		Version: imageTag(c.Components.OperatingSystem.Image.Base),
	}

	return info, nil
}
