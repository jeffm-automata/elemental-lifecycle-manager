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

// Package buildstatus is the protocol between a builder and the operator: the builder writes
// status.json (with status.py) into the directory it serves on port 8080, the operator reads it.
// Every builder backend uses it, so a build looks the same wherever it runs.
package buildstatus

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/target"
)

// Port is the port a builder serves its output directory on.
const Port = 8080

// File is the name of the status document in that directory.
const File = "status.json"

//go:embed status.py
var script []byte

// Script returns status.py, which a builder runs to write the status document.
func Script() []byte { return script }

// Status is status.json as status.py writes it.
type Status struct {
	Key               string `json:"key"`
	State             string `json:"state"`
	Step              string `json:"step"`
	ExitCode          int    `json:"exitCode"`
	Error             string `json:"error"`
	LogTail           string `json:"logTail"`
	SELinux           string `json:"selinux"`
	File              string `json:"file"`
	SHA256            string `json:"sha256"`
	SHA512            string `json:"sha512"`
	SizeBytes         int64  `json:"sizeBytes"`
	ManifestDigest    string `json:"manifestDigest"`
	KubernetesVersion string `json:"kubernetesVersion"`
}

// Apply reads a status document into b, which the caller has filled with the builder's name and
// creation time. where names the builder in messages; url gives the address the target can fetch a
// file of the builder from. A document that can not be read leaves the build running.
func Apply(b *target.Build, raw []byte, req target.Request, where string, url func(file string) string) {
	var st Status
	if err := json.Unmarshal(raw, &st); err != nil {
		b.Step = "builder status unreadable"
		return
	}
	if st.Key != req.Key {
		b.State, b.Terminal = target.BuildFailed, true
		b.Message = fmt.Sprintf("builder %s belongs to another cache key", where)
		return
	}
	switch st.State {
	case "done":
		if st.File == "" || st.SHA512 == "" {
			b.State, b.Message = target.BuildFailed, "builder reported success without a file or checksum"
			return
		}
		b.State = target.BuildSucceeded
		b.Artifact = &target.Artifact{
			URL: url(st.File), File: st.File, SHA256: st.SHA256, SHA512: st.SHA512,
			SizeBytes: st.SizeBytes, KubernetesVersion: st.KubernetesVersion,
		}
	case "failed":
		b.State = target.BuildFailed
		// Exit 2: the recipe refused its arguments. Exit 3: the inspection rejected the image.
		b.Terminal = st.ExitCode == 2 || st.ExitCode == 3
		b.Message = strings.TrimSpace(fmt.Sprintf("%s (step %s, exit status %d, builder SELinux: %s)\n%s",
			st.Error, st.Step, st.ExitCode, st.SELinux, st.LogTail))
	default:
		b.Step = st.Step
	}
}
