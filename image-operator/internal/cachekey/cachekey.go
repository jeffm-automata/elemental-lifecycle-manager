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

// Package cachekey computes the key under which a built node image is stored and found again.
package cachekey

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// SchemaVersion changes whenever the layout of the hashed document changes, so that keys made
// by a different layout can never match.
const SchemaVersion = 1

// ChartValues are Helm values given for one chart of the release.
type ChartValues struct {
	Chart  string `json:"chart"`
	Values string `json:"values"`
}

// Inputs is everything that decides the content of an image. Nothing that only says where the
// image goes (request name, target address, credentials) belongs here.
type Inputs struct {
	// ManifestDigest is the digest of the release manifest ("sha256:<hex>"), never a tag.
	ManifestDigest string
	// Target is the target type, for example "harvester".
	Target string
	// Arch is the machine architecture, for example "x86_64".
	Arch string
	// DiskSize is the virtual size of the image.
	DiskSize string
	// ChartValues are optional; their order does not matter.
	ChartValues []ChartValues
	// RecipeDigest identifies the build recipe.
	RecipeDigest string
	// ToolRepo and ToolCommit identify the elemental3 source the recipe builds with.
	ToolRepo   string
	ToolCommit string
}

// document is the hashed form. Field order is fixed by the struct, so the encoding is canonical.
type document struct {
	Schema         int           `json:"schema"`
	ManifestDigest string        `json:"manifestDigest"`
	Target         string        `json:"target"`
	Arch           string        `json:"arch"`
	DiskSize       string        `json:"diskSize"`
	ChartValues    []ChartValues `json:"chartValues"`
	RecipeDigest   string        `json:"recipeDigest"`
	ToolRepo       string        `json:"toolRepo"`
	ToolCommit     string        `json:"toolCommit"`
}

// Compute returns the cache key as 64 lower-case hex characters.
func Compute(in Inputs) (string, error) {
	if !strings.HasPrefix(in.ManifestDigest, "sha256:") || len(in.ManifestDigest) != len("sha256:")+64 {
		return "", fmt.Errorf("manifest digest %q is not a sha256 digest", in.ManifestDigest)
	}
	for name, v := range map[string]string{
		"target": in.Target, "arch": in.Arch, "disk size": in.DiskSize,
		"recipe digest": in.RecipeDigest, "tool repository": in.ToolRepo, "tool commit": in.ToolCommit,
	} {
		if v == "" {
			return "", fmt.Errorf("%s is empty", name)
		}
	}
	charts := append([]ChartValues{}, in.ChartValues...)
	sort.Slice(charts, func(i, j int) bool { return charts[i].Chart < charts[j].Chart })
	for i := range charts {
		if charts[i].Chart == "" {
			return "", errors.New("chart values without a chart name")
		}
		if i > 0 && charts[i].Chart == charts[i-1].Chart {
			return "", fmt.Errorf("chart %q has values twice", charts[i].Chart)
		}
	}
	raw, err := json.Marshal(document{
		Schema: SchemaVersion, ManifestDigest: in.ManifestDigest, Target: in.Target, Arch: in.Arch,
		DiskSize: in.DiskSize, ChartValues: charts, RecipeDigest: in.RecipeDigest,
		ToolRepo: in.ToolRepo, ToolCommit: in.ToolCommit,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
