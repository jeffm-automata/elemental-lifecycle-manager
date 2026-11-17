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
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/target"
)

const ns = "agents"

var req = target.Request{
	Key:            strings.Repeat("ab", 32),
	ManifestRef:    "registry.example/rel/rke2-manifest:1.37.2-20.3",
	ManifestDigest: "sha256:" + strings.Repeat("1", 64),
	Arch:           "x86_64", DiskSize: "20G",
	ChartValues: []target.ChartValues{{Chart: "some-chart", Values: "image:\n  repository: \"x'y; rm -rf /\"\n"}},
}

func newTarget(t *testing.T) (*Harvester, *string) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(gvkImage, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvkVM, &unstructured.Unstructured{})
	status := ""
	h := &Harvester{
		Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Namespace: ns, StorageClass: "sc", Prefix: "p-",
		Builder: BuilderConfig{BaseImageURL: "https://example.invalid/leap.qcow2", CPU: 4, Memory: "8Gi", Disk: "40Gi",
			ElementalRepo: "https://example.invalid/e.git", ElementalRef: strings.Repeat("c", 40)},
		Fetch: func(context.Context, string, string, string) ([]byte, error) {
			if status == "" {
				return nil, errors.New("service unavailable")
			}
			return []byte(status), nil
		},
	}
	return h, &status
}

func cond(typ, status, msg string) map[string]any {
	return map[string]any{"type": typ, "status": status, "message": msg}
}

func setImported(t *testing.T, h *Harvester, name string) {
	t.Helper()
	u := newObj(gvkImage)
	if err := h.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, u); err != nil {
		t.Fatal(err)
	}
	u.Object["status"] = map[string]any{"storageClassName": "longhorn-" + name, "conditions": []any{
		cond("Initialized", "True", ""), cond("Imported", "True", "")}}
	if err := h.Client.Update(context.Background(), u); err != nil {
		t.Fatal(err)
	}
}

func TestBuilderLifecycle(t *testing.T) {
	ctx := context.Background()
	h, _ := newTarget(t)
	name := h.BuilderName(req.Key)
	if name != "p-build-abababababab" || h.ImageName(req.Key) != "p-img-abababababab" {
		t.Fatalf("names %s %s", name, h.ImageName(req.Key))
	}

	if b, err := h.Status(ctx, req); err != nil || b.State != target.BuildAbsent {
		t.Fatalf("before Ensure: %+v %v", b, err)
	}
	// First Ensure: Secret, Service and the base image; no VM while the base image is not imported.
	if err := h.Ensure(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := h.get(ctx, gvkVM, name); found {
		t.Fatal("VM created before its base image was imported")
	}
	if b, _ := h.Status(ctx, req); b.State != target.BuildRunning {
		t.Fatalf("a builder being prepared must count as running: %+v", b)
	}
	if err := h.Ensure(ctx, req); err != nil {
		t.Fatal(err)
	}
	setImported(t, h, "p-builder-base")
	if err := h.Ensure(ctx, req); err != nil {
		t.Fatal(err)
	}
	vm, found, _ := h.get(ctx, gvkVM, name)
	if !found {
		t.Fatal("no VM after the base image was imported")
	}
	// Nothing privileged is asked of Harvester: pod network only, two virtio disks.
	spec, _, _ := unstructured.NestedMap(vm.Object, "spec", "template", "spec")
	raw, _ := yaml.Marshal(spec)
	for _, banned := range []string{"hostDevices", "gpus", "hostDisk", "multus", "privileged"} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("VM spec contains %s", banned)
		}
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name + "-disk"}, pvc); err != nil {
		t.Fatal(err)
	}
	if pvc.Annotations[annImageID] != "agents/p-builder-base" || *pvc.Spec.StorageClassName != "longhorn-p-builder-base" {
		t.Fatalf("disk is not cloned from the base image: %+v", pvc)
	}
	if err := h.Ensure(ctx, req); err != nil {
		t.Fatalf("Ensure is not idempotent: %v", err)
	}

}

// readyBuilder returns a target whose builder VM exists.
func readyBuilder(t *testing.T) (*Harvester, *string) {
	t.Helper()
	ctx := context.Background()
	h, status := newTarget(t)
	if err := h.Ensure(ctx, req); err != nil {
		t.Fatal(err)
	}
	setImported(t, h, "p-builder-base")
	if err := h.Ensure(ctx, req); err != nil {
		t.Fatal(err)
	}
	return h, status
}

func statusJSON(fields string) string { return `{"key":"` + req.Key + `",` + fields + `}` }

func TestBuilderStatus(t *testing.T) {
	ctx := context.Background()
	h, status := readyBuilder(t)
	if b, _ := h.Status(ctx, req); b.State != target.BuildRunning || !strings.Contains(b.Step, "starting") {
		t.Fatalf("VM without a status yet: %+v", b)
	}
	*status = statusJSON(`"state":"building","step":"build"`)
	if b, _ := h.Status(ctx, req); b.State != target.BuildRunning || b.Step != "build" {
		t.Fatalf("building: %+v", b)
	}
	*status = statusJSON(`"state":"failed","step":"build","exitCode":1,"error":"the recipe ended with exit status 1","logTail":"boom"`)
	if b, _ := h.Status(ctx, req); b.State != target.BuildFailed || b.Terminal || !strings.Contains(b.Message, "boom") {
		t.Fatalf("failed: %+v", b)
	}
	*status = statusJSON(`"state":"failed","step":"build","exitCode":3,"error":"rejected"`)
	if b, _ := h.Status(ctx, req); b.State != target.BuildFailed || !b.Terminal {
		t.Fatalf("exit status 3 must be terminal: %+v", b)
	}
	*status = `{"key":"another","state":"done","file":"f.qcow2","sha512":"x"}`
	if b, _ := h.Status(ctx, req); b.State != target.BuildFailed || !b.Terminal {
		t.Fatalf("a builder of another key must not be used: %+v", b)
	}
	*status = statusJSON(`"state":"done","file":"f.qcow2","sha256":"s256","sha512":"s512","sizeBytes":5,"kubernetesVersion":"v1"`)
	b, _ := h.Status(ctx, req)
	if b.State != target.BuildSucceeded || b.Artifact == nil || b.Artifact.URL != "http://p-build-abababababab.agents.svc.cluster.local:8080/f.qcow2" {
		t.Fatalf("done: %+v %+v", b, b.Artifact)
	}

}

func TestDeliverFindCleanup(t *testing.T) {
	ctx := context.Background()
	h, status := readyBuilder(t)
	*status = statusJSON(`"state":"done","file":"f.qcow2","sha256":"s256","sha512":"s512","kubernetesVersion":"v1"`)
	b, _ := h.Status(ctx, req)
	if b.Artifact == nil {
		t.Fatalf("no artifact: %+v", b)
	}
	if i, _ := h.FindImage(ctx, req); i.State != target.ImageAbsent {
		t.Fatalf("image before delivery: %+v", i)
	}
	if err := h.Deliver(ctx, req, *b.Artifact); err != nil {
		t.Fatal(err)
	}
	if err := h.Deliver(ctx, req, *b.Artifact); err != nil {
		t.Fatalf("Deliver is not idempotent: %v", err)
	}
	i, _ := h.FindImage(ctx, req)
	if i.State != target.ImageImporting || i.SHA256 != "s256" || i.KubernetesVersion != "v1" {
		t.Fatalf("image after delivery: %+v", i)
	}
	img, _, _ := h.get(ctx, gvkImage, i.Name)
	if sum, _, _ := unstructured.NestedString(img.Object, "spec", "checksum"); sum != "s512" {
		t.Fatalf("Harvester is not given the SHA-512: %q", sum)
	}
	setImported(t, h, i.Name)
	if i, _ = h.FindImage(ctx, req); i.State != target.ImageReady {
		t.Fatalf("image after import: %+v", i)
	}
	other := req
	other.Key = strings.Repeat("ab", 6) + strings.Repeat("0", 52) // same name, different key
	if _, err := h.FindImage(ctx, other); err == nil {
		t.Fatal("an image of another key with the same short name was accepted")
	}

	// Cleanup removes the builder and leaves the image and the base image.
	if err := h.Cleanup(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := h.Cleanup(ctx, req); err != nil {
		t.Fatalf("Cleanup is not idempotent: %v", err)
	}
	if b, _ := h.Status(ctx, req); b.State != target.BuildAbsent {
		t.Fatalf("builder left after cleanup: %+v", b)
	}
	if i, _ = h.FindImage(ctx, req); i.State != target.ImageReady {
		t.Fatal("cleanup removed the image")
	}
	if err := h.DeleteImage(ctx, req); err != nil {
		t.Fatal(err)
	}
	if i, _ = h.FindImage(ctx, req); i.State != target.ImageAbsent {
		t.Fatal("image not deleted")
	}
}

func TestImageStates(t *testing.T) {
	mk := func(conds ...any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"conditions": conds}}}
	}
	c := cond
	if s, _ := imageState(mk()); s != target.ImageImporting {
		t.Errorf("no conditions: %s", s)
	}
	if s, _ := imageState(mk(c("Initialized", "True", ""), c("Imported", "Unknown", ""))); s != target.ImageImporting {
		t.Errorf("importing: %s", s)
	}
	if s, _ := imageState(mk(c("Imported", "True", ""))); s != target.ImageReady {
		t.Errorf("imported: %s", s)
	}
	if s, m := imageState(mk(c("Imported", "False", "checksum mismatch"), c("RetryLimitExceeded", "True", "gave up"))); s != target.ImageFailed || !strings.Contains(m, "gave up") {
		t.Errorf("retry limit: %s %q", s, m)
	}
}

func TestUserDataHoldsNoShellInjection(t *testing.T) {
	h, _ := newTarget(t)
	raw, err := h.userData(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "#cloud-config\n") {
		t.Fatal("not a cloud-config document")
	}
	var doc struct {
		WriteFiles []writeFile `json:"write_files"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	files := map[string]writeFile{}
	for _, f := range doc.WriteFiles {
		files[f.Path] = f
	}
	env := files["/etc/e3-build/env"].Content
	for _, want := range []string{"MANIFEST='" + req.ManifestRef + "'", "EXPECT_DIGEST='" + req.ManifestDigest + "'", "NAME='elemental-node-1.37.2-20.3-harvester.x86_64'", "KEY='" + req.Key + "'"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s", want)
		}
	}
	// Chart values are free text; they must travel as a file, never through the shell.
	cv := files["/etc/e3-build/chart-values/some-chart.yaml"]
	got, _ := base64.StdEncoding.DecodeString(cv.Content)
	if cv.Encoding != "b64" || string(got) != req.ChartValues[0].Values || strings.Contains(env, "rm -rf") {
		t.Fatal("chart values are not delivered verbatim as a file")
	}
	if len(raw) > 512*1024 {
		t.Fatalf("user-data is %d bytes; a Secret holds 1 MiB", len(raw))
	}

	for name, change := range map[string]func(*target.Request){
		"manifest": func(r *target.Request) { r.ManifestRef = "x'; reboot; '" },
		"arch":     func(r *target.Request) { r.Arch = "$(id)" },
		"disk":     func(r *target.Request) { r.DiskSize = "20G; id" },
		"chart":    func(r *target.Request) { r.ChartValues = []target.ChartValues{{Chart: "../../etc/passwd", Values: ""}} },
	} {
		bad := req
		change(&bad)
		if _, err := h.userData(bad); err == nil {
			t.Errorf("%s: unsafe value accepted", name)
		}
	}
}

func TestFileBase(t *testing.T) {
	r := target.Request{ManifestRef: "reg.example:5000/a/b:1.2.3", Arch: "x86_64"}
	if got := FileBase(r); got != "elemental-node-1.2.3-harvester.x86_64" {
		t.Error(got)
	}
	r.ManifestRef = "reg.example/a/b@sha256:" + strings.Repeat("9", 64)
	if got := FileBase(r); got != "elemental-node-999999999999-harvester.x86_64" {
		t.Error(got)
	}
}

func TestDisplayNameFitsALabel(t *testing.T) {
	h, _ := newTarget(t)
	h.Prefix = "e3p-t1-5-"
	if got := h.DisplayName(req); got != "e3p-t1-5-e3-1.37.2-20.3-harvester-x86_64-abababab.qcow2" {
		t.Error(got)
	}
	long := req
	long.ManifestRef = "registry.example/rel/manifest:" + strings.Repeat("9.", 40) + "0"
	long.Arch = "aarch64"
	h.Prefix = "a-long-prefix-of-20-"
	got := h.DisplayName(long)
	if len(got) > 63 || !strings.HasSuffix(got, "-harvester-aarch64-abababab.qcow2") || !strings.HasPrefix(got, h.Prefix) {
		t.Errorf("%d bytes: %s", len(got), got)
	}
	other := long
	other.Key = strings.Repeat("cd", 32)
	if h.DisplayName(other) == got {
		t.Error("two keys share a display name")
	}
}
