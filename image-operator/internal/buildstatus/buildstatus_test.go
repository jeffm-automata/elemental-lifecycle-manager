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

package buildstatus

import (
	"strings"
	"testing"

	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/target"
)

func TestApply(t *testing.T) {
	req := target.Request{Key: strings.Repeat("ab", 32)}
	url := func(file string) string { return "http://builder/" + file }
	for _, c := range []struct {
		name, doc string
		state     target.BuildState
		terminal  bool
		check     func(b target.Build) bool
	}{
		{"unreadable", `{`, target.BuildRunning, false, func(b target.Build) bool { return b.Step != "" }},
		{"other key", `{"key":"x","state":"done"}`, target.BuildFailed, true, nil},
		{"running", `{"key":"` + req.Key + `","state":"building","step":"build"}`, target.BuildRunning, false,
			func(b target.Build) bool { return b.Step == "build" }},
		{"done", `{"key":"` + req.Key + `","state":"done","file":"f.qcow2","sha512":"s5","sha256":"s2","kubernetesVersion":"v1"}`,
			target.BuildSucceeded, false, func(b target.Build) bool {
				return b.Artifact != nil && b.Artifact.URL == "http://builder/f.qcow2" && b.Artifact.SHA512 == "s5" && b.Artifact.KubernetesVersion == "v1"
			}},
		{"done without checksum", `{"key":"` + req.Key + `","state":"done","file":"f.qcow2"}`, target.BuildFailed, false, nil},
		{"failed", `{"key":"` + req.Key + `","state":"failed","exitCode":1,"error":"boom","logTail":"last line"}`, target.BuildFailed, false,
			func(b target.Build) bool {
				return strings.Contains(b.Message, "boom") && strings.Contains(b.Message, "last line")
			}},
		{"rejected", `{"key":"` + req.Key + `","state":"failed","exitCode":3}`, target.BuildFailed, true, nil},
	} {
		b := target.Build{State: target.BuildRunning, Name: "b"}
		Apply(&b, []byte(c.doc), req, "ns/b", url)
		if b.State != c.state || b.Terminal != c.terminal || (c.check != nil && !c.check(b)) {
			t.Errorf("%s: %+v", c.name, b)
		}
	}
	if len(Script()) == 0 {
		t.Error("no status script embedded")
	}
}
