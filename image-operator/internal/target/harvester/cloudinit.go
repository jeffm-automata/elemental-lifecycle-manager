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

package harvester

import (
	_ "embed"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/buildstatus"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/recipe"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/target"
)

//go:embed builder/run-build.sh
var runBuild []byte

// Values that reach a shell in the builder are checked here as well as by the CRD schema.
var (
	safeRef   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]*$`)
	safeWord  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	safeURL   = regexp.MustCompile(`^https?://[A-Za-z0-9][A-Za-z0-9._:/@~-]*$`)
	safeChart = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,61}[a-z0-9])?$`)
)

func parseQuantity(s string) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return q, fmt.Errorf("builder disk size %q: %w", s, err)
	}
	return q, nil
}

type writeFile struct {
	Path        string `json:"path"`
	Permissions string `json:"permissions"`
	Encoding    string `json:"encoding,omitempty"`
	Content     string `json:"content"`
}

func b64File(path, perm string, data []byte) writeFile {
	return writeFile{Path: path, Permissions: perm, Encoding: "b64", Content: base64.StdEncoding.EncodeToString(data)}
}

const buildUnit = `[Unit]
Description=Elemental node image build
After=network-online.target
Wants=network-online.target
[Service]
Type=oneshot
ExecStart=/usr/local/bin/e3-run-build
`

const httpUnit = `[Unit]
Description=Serves the build result to the image build operator and to Harvester
After=network-online.target
[Service]
DynamicUser=yes
ExecStart=/usr/bin/python3 -m http.server 8080 --directory /var/lib/e3-build/out
Restart=always
`

// BuildEnv renders the shell variables a builder's run script reads (the request and the tooling),
// after checking every value that reaches a shell. Every builder backend uses it.
func BuildEnv(req target.Request, elementalRepo, elementalRef string) (string, error) {
	for name, check := range map[string]struct {
		re *regexp.Regexp
		v  string
	}{
		"release manifest": {safeRef, req.ManifestRef}, "manifest digest": {safeRef, req.ManifestDigest},
		"architecture": {safeWord, req.Arch}, "disk size": {safeWord, req.DiskSize}, "cache key": {safeWord, req.Key},
		"elemental repository": {safeURL, elementalRepo}, "elemental ref": {safeWord, elementalRef},
	} {
		if !check.re.MatchString(check.v) {
			return "", fmt.Errorf("%s %q has characters that are not allowed", name, check.v)
		}
	}
	for _, cv := range req.ChartValues {
		if !safeChart.MatchString(cv.Chart) {
			return "", fmt.Errorf("chart name %q has characters that are not allowed", cv.Chart)
		}
	}
	return strings.Join([]string{
		"MANIFEST='" + req.ManifestRef + "'", "EXPECT_DIGEST='" + req.ManifestDigest + "'", "TARGET='harvester'",
		"ARCH='" + req.Arch + "'", "DISK_SIZE='" + req.DiskSize + "'", "NAME='" + FileBase(req) + "'", "KEY='" + req.Key + "'",
		"ELEMENTAL_REPO='" + elementalRepo + "'", "ELEMENTAL_REF='" + elementalRef + "'", "",
	}, "\n"), nil
}

// userData renders the #cloud-config document of the builder VM. It contains no credential.
func (h *Harvester) userData(req target.Request) ([]byte, error) {
	env, err := BuildEnv(req, h.Builder.ElementalRepo, h.Builder.ElementalRef)
	if err != nil {
		return nil, err
	}
	tarball, err := recipe.Tarball()
	if err != nil {
		return nil, err
	}
	files := []writeFile{
		{Path: "/etc/e3-build/env", Permissions: "0644", Content: env},
		b64File("/etc/e3-build/recipe.tgz", "0644", tarball),
		// In /usr/local/bin on purpose: on an SELinux system a unit that starts a program from there runs
		// unconfined, while one started from /etc stays in the init domain and may not execute the recipe.
		b64File("/usr/local/bin/e3-run-build", "0755", runBuild),
		b64File("/etc/e3-build/status.py", "0644", buildstatus.Script()),
		{Path: "/etc/systemd/system/e3-build.service", Permissions: "0644", Content: buildUnit},
		{Path: "/etc/systemd/system/e3-build-http.service", Permissions: "0644", Content: httpUnit},
	}
	for _, cv := range req.ChartValues {
		files = append(files, b64File("/etc/e3-build/chart-values/"+cv.Chart+".yaml", "0644", []byte(cv.Values)))
	}
	doc := map[string]any{
		"growpart":       map[string]any{"mode": "auto", "devices": []string{"/"}},
		"package_update": true,
		"packages":       []string{"qemu-guest-agent", "podman", "git-core", "python3", "qemu-img", "yq", "tar", "gzip"},
		"write_files":    files,
		"runcmd": []string{
			"mkdir -p /var/lib/e3-build/out",
			"systemctl daemon-reload",
			"systemctl enable --now qemu-guest-agent.service",
			"systemctl start e3-build-http.service",
			"systemctl start --no-block e3-build.service",
		},
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return append([]byte("#cloud-config\n"), out...), nil
}
