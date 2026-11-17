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

package recipe

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
)

func TestRecipeIsComplete(t *testing.T) {
	raw, err := Tarball()
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]int64{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		modes[h.Name] = h.Mode
	}
	for _, need := range []string{EntryPoint, "inspect.sh", "defaults.env", "deviations.json", "lib/inspect.py",
		"lib/ociref.py", "lib/extract-in-container.sh", "lib/Containerfile.inspect", "targets/harvester/install.yaml",
		"targets/harvester/release.yaml", "targets/harvester/butane.yaml", "targets/harvester/kubernetes/cluster.yaml"} {
		if _, ok := modes[need]; !ok {
			t.Errorf("recipe lacks %s", need)
		}
	}
	for name, mode := range modes {
		if strings.HasSuffix(name, ".sh") && mode&0o111 == 0 {
			t.Errorf("%s is not executable", name)
		}
	}
}

func TestDigestIsStable(t *testing.T) {
	a, err := Digest()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Digest()
	if a != b || !strings.HasPrefix(a, "sha256:") || len(a) != 71 {
		t.Fatalf("digest %q / %q", a, b)
	}
	x, _ := Tarball()
	y, _ := Tarball()
	if !bytes.Equal(x, y) {
		t.Fatal("two tarballs of the same recipe differ")
	}
}
