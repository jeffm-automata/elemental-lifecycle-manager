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

package cachekey

import (
	"strings"
	"testing"
)

func base() Inputs {
	return Inputs{
		ManifestDigest: "sha256:" + strings.Repeat("a", 64),
		Target:         "harvester",
		Arch:           "x86_64",
		DiskSize:       "20G",
		ChartValues:    []ChartValues{{Chart: "b", Values: "x: 1\n"}, {Chart: "a", Values: "y: 2\n"}},
		RecipeDigest:   "sha256:" + strings.Repeat("b", 64),
		ToolRepo:       "https://example.invalid/elemental.git",
		ToolCommit:     strings.Repeat("c", 40),
	}
}

func mustKey(t *testing.T, in Inputs) string {
	t.Helper()
	k, err := Compute(in)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	return k
}

func TestKeyIsStable(t *testing.T) {
	// A fixed vector, computed independently (sha256 of the compact JSON document with the fields
	// in the order of the document struct and charts sorted by name). If this changes, every cached
	// image is orphaned: bump SchemaVersion instead of changing the layout silently.
	const want = "0609ed2f49766c5b7bf3f0a66c4b3e92eef30e95bf03f612e56382d2d3e9df58"
	got := mustKey(t, base())
	if got != want {
		t.Fatalf("key %s, want %s", got, want)
	}
	if got != mustKey(t, base()) {
		t.Fatal("same inputs gave different keys")
	}
}

func TestChartOrderDoesNotMatter(t *testing.T) {
	a := base()
	b := base()
	b.ChartValues = []ChartValues{b.ChartValues[1], b.ChartValues[0]}
	if mustKey(t, a) != mustKey(t, b) {
		t.Fatal("order of chart values changed the key")
	}
	if a.ChartValues[0].Chart != "b" {
		t.Fatal("Compute sorted the caller's slice in place")
	}
}

func TestEveryInputChangesTheKey(t *testing.T) {
	ref := mustKey(t, base())
	changes := map[string]func(*Inputs){
		"manifest digest": func(i *Inputs) { i.ManifestDigest = "sha256:" + strings.Repeat("d", 64) },
		"target":          func(i *Inputs) { i.Target = "vsphere" },
		"arch":            func(i *Inputs) { i.Arch = "aarch64" },
		"disk size":       func(i *Inputs) { i.DiskSize = "21G" },
		"chart values":    func(i *Inputs) { i.ChartValues[0].Values = "x: 2\n" },
		"chart name":      func(i *Inputs) { i.ChartValues[0].Chart = "c" },
		"chart removed":   func(i *Inputs) { i.ChartValues = i.ChartValues[:1] },
		"no charts":       func(i *Inputs) { i.ChartValues = nil },
		"recipe":          func(i *Inputs) { i.RecipeDigest = "sha256:" + strings.Repeat("e", 64) },
		"tool repo":       func(i *Inputs) { i.ToolRepo = "https://example.invalid/other.git" },
		"tool commit":     func(i *Inputs) { i.ToolCommit = strings.Repeat("f", 40) },
	}
	seen := map[string]string{ref: "reference"}
	for name, change := range changes {
		in := base()
		change(&in)
		k := mustKey(t, in)
		if other, dup := seen[k]; dup {
			t.Errorf("changing %s gave the same key as %s", name, other)
		}
		seen[k] = name
	}
}

func TestFieldsCanNotAlias(t *testing.T) {
	// Moving text from one field to its neighbour must not give the same key.
	a := base()
	a.Target, a.Arch = "harvesterx86", "_64"
	b := base()
	b.Target, b.Arch = "harvester", "x86_64"
	if mustKey(t, a) == mustKey(t, b) {
		t.Fatal("field boundaries are not part of the key")
	}
}

func TestInvalidInputs(t *testing.T) {
	cases := map[string]func(*Inputs){
		"tag instead of digest": func(i *Inputs) { i.ManifestDigest = "1.37.2-20.3" },
		"short digest":          func(i *Inputs) { i.ManifestDigest = "sha256:abc" },
		"empty target":          func(i *Inputs) { i.Target = "" },
		"empty arch":            func(i *Inputs) { i.Arch = "" },
		"empty disk size":       func(i *Inputs) { i.DiskSize = "" },
		"empty recipe":          func(i *Inputs) { i.RecipeDigest = "" },
		"empty tool repo":       func(i *Inputs) { i.ToolRepo = "" },
		"empty tool commit":     func(i *Inputs) { i.ToolCommit = "" },
		"duplicate chart":       func(i *Inputs) { i.ChartValues[1].Chart = i.ChartValues[0].Chart },
		"unnamed chart":         func(i *Inputs) { i.ChartValues[0].Chart = "" },
	}
	for name, change := range cases {
		in := base()
		change(&in)
		if _, err := Compute(in); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
