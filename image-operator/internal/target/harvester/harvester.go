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

// Package harvester implements target.Target and target.Builder on a Harvester cluster, with
// the rights of a namespace tenant: the image is a VirtualMachineImage, the builder is a
// short-lived VM in the same namespace that serves its result over HTTP behind a Service.
package harvester

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/target"
)

const (
	// LabelKey marks every object the operator creates on the target (first 40 characters of the key;
	// a label value can not hold all 64).
	LabelKey = "image.lifecycle.suse.com/cache-key"
	// LabelRole is "image", "builder" or "builder-base".
	LabelRole = "image.lifecycle.suse.com/role"

	annKey        = "image.lifecycle.suse.com/cache-key"
	annManifest   = "image.lifecycle.suse.com/release-manifest"
	annDigest     = "image.lifecycle.suse.com/release-manifest-digest"
	annSHA256     = "image.lifecycle.suse.com/sha256"
	annKubernetes = "image.lifecycle.suse.com/kubernetes-version"
	annImageID    = "harvesterhci.io/imageId"
	labelVMName   = "harvesterhci.io/vmName"

	httpPort = 8080
	keyName  = "name"
	virtio   = "virtio"
)

var (
	gvkImage = schema.GroupVersionKind{Group: "harvesterhci.io", Version: "v1beta1", Kind: "VirtualMachineImage"}
	gvkVM    = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"}
)

// BuilderConfig is the operator-wide description of the builder VM and the tooling it builds with.
type BuilderConfig struct {
	// BaseImageURL is the cloud image the builder VM boots (cloud-init, zypper).
	BaseImageURL string
	CPU          int64
	Memory       string
	Disk         string
	// ElementalRepo and ElementalRef (a commit) are the elemental3 source the recipe builds.
	ElementalRepo string
	ElementalRef  string
}

// Fetch reads a file the builder serves, through the target's API (Service proxy).
type Fetch func(ctx context.Context, namespace, service, path string) ([]byte, error)

// Harvester is one namespace of one Harvester cluster.
type Harvester struct {
	Client       client.Client
	Fetch        Fetch
	Namespace    string
	StorageClass string
	Prefix       string
	Builder      BuilderConfig
}

var _ target.Target = (*Harvester)(nil)
var _ target.Builder = (*Harvester)(nil)

func short(key string) string {
	if len(key) > 12 {
		return key[:12]
	}
	return key
}

func labelValue(key string) string {
	if len(key) > 40 {
		return key[:40]
	}
	return key
}

// ImageName is the name of the VirtualMachineImage for a key.
func (h *Harvester) ImageName(key string) string { return h.Prefix + "img-" + short(key) }

// BuilderName is the name of the builder VM and its Service for a key.
func (h *Harvester) BuilderName(key string) string { return h.Prefix + "build-" + short(key) }

func (h *Harvester) baseImageName() string { return h.Prefix + "builder-base" }

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// FileBase is the base name of the image file: elemental-node-<release tag>-harvester.<arch>.
func FileBase(req target.Request) string {
	tag := req.ManifestRef
	if i := strings.LastIndexAny(tag, ":@"); i >= 0 {
		tag = tag[i+1:]
	}
	if strings.Contains(req.ManifestRef, "@") && len(tag) > 12 {
		tag = tag[:12]
	}
	return "elemental-node-" + unsafeName.ReplaceAllString(tag, "_") + "-harvester." + req.Arch
}

func (h *Harvester) labels(key, role string) map[string]any {
	l := map[string]any{LabelRole: role}
	if key != "" {
		l[LabelKey] = labelValue(key)
	}
	return l
}

func newObj(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u
}

func (h *Harvester) get(ctx context.Context, gvk schema.GroupVersionKind, name string) (*unstructured.Unstructured, bool, error) {
	u := newObj(gvk)
	err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.Namespace, Name: name}, u)
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	return u, err == nil, err
}

func (h *Harvester) createIgnoreExists(ctx context.Context, obj client.Object) error {
	if err := h.Client.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %s %s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
	}
	return nil
}

func (h *Harvester) deleteIgnoreMissing(ctx context.Context, obj client.Object) error {
	if err := h.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s: %w", obj.GetName(), err)
	}
	return nil
}

// imageState reads the conditions Harvester sets on a VirtualMachineImage.
func imageState(u *unstructured.Unstructured) (target.ImageState, string) {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	state, msg := target.ImageImporting, ""
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		status, _ := m["status"].(string)
		message, _ := m["message"].(string)
		switch {
		case typ == "Imported" && status == "True":
			return target.ImageReady, ""
		case typ == "RetryLimitExceeded" && status == "True":
			state, msg = target.ImageFailed, "Harvester gave up importing the image: "+message
		case typ == "Imported" && status == "False" && message != "" && state != target.ImageFailed:
			msg = message
		}
	}
	return state, msg
}

// FindImage implements target.Target.
func (h *Harvester) FindImage(ctx context.Context, req target.Request) (target.Image, error) {
	name := h.ImageName(req.Key)
	u, found, err := h.get(ctx, gvkImage, name)
	if err != nil {
		return target.Image{}, err
	}
	if !found {
		return target.Image{State: target.ImageAbsent, Name: name, Namespace: h.Namespace}, nil
	}
	ann := u.GetAnnotations()
	if ann[annKey] != req.Key {
		return target.Image{}, fmt.Errorf("image %s/%s exists but belongs to another cache key", h.Namespace, name)
	}
	display, _, _ := unstructured.NestedString(u.Object, "spec", "displayName")
	state, msg := imageState(u)
	return target.Image{
		State: state, Name: name, Namespace: h.Namespace, DisplayName: display, SHA256: ann[annSHA256],
		KubernetesVersion: ann[annKubernetes], Created: u.GetCreationTimestamp().Time, Message: msg,
	}, nil
}

// Deliver implements target.Target: Harvester downloads the file from the builder and checks its SHA-512.
func (h *Harvester) Deliver(ctx context.Context, req target.Request, a target.Artifact) error {
	if a.URL == "" || a.SHA512 == "" {
		return fmt.Errorf("artifact for %s has no URL or no SHA-512", short(req.Key))
	}
	u := newObj(gvkImage)
	u.SetNamespace(h.Namespace)
	u.SetName(h.ImageName(req.Key))
	u.Object["metadata"].(map[string]any)["labels"] = h.labels(req.Key, "image")
	u.SetAnnotations(map[string]string{
		annKey: req.Key, annManifest: req.ManifestRef, annDigest: req.ManifestDigest,
		annSHA256: a.SHA256, annKubernetes: a.KubernetesVersion,
	})
	u.Object["spec"] = map[string]any{
		"displayName":            h.Prefix + FileBase(req) + "-" + req.Key[:8] + ".qcow2",
		"description":            "Elemental node image of " + req.ManifestRef + ", built by the image build operator",
		"sourceType":             "download",
		"backend":                "backingimage",
		"targetStorageClassName": h.StorageClass,
		"url":                    a.URL,
		"checksum":               a.SHA512,
	}
	return h.createIgnoreExists(ctx, u)
}

// DeleteImage implements target.Target.
func (h *Harvester) DeleteImage(ctx context.Context, req target.Request) error {
	u := newObj(gvkImage)
	u.SetNamespace(h.Namespace)
	u.SetName(h.ImageName(req.Key))
	return h.deleteIgnoreMissing(ctx, u)
}

// builderStatus is status.json as run-build.sh writes it.
type builderStatus struct {
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

// Status implements target.Builder.
func (h *Harvester) Status(ctx context.Context, req target.Request) (target.Build, error) {
	name := h.BuilderName(req.Key)
	secret := &corev1.Secret{}
	err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.Namespace, Name: name + "-cloudinit"}, secret)
	claimed := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return target.Build{}, err
	}
	vm, found, err := h.get(ctx, gvkVM, name)
	if err != nil {
		return target.Build{}, err
	}
	if !found {
		if !claimed {
			return target.Build{State: target.BuildAbsent, Name: name}, nil
		}
		return target.Build{State: target.BuildRunning, Name: name, Created: secret.CreationTimestamp.Time, Step: "preparing the builder"}, nil
	}
	created := vm.GetCreationTimestamp().Time
	if claimed {
		created = secret.CreationTimestamp.Time
	}
	b := target.Build{State: target.BuildRunning, Name: name, Created: created}
	raw, err := h.Fetch(ctx, h.Namespace, name, "status.json")
	if err != nil {
		printable, _, _ := unstructured.NestedString(vm.Object, "status", "printableStatus")
		b.Step = "builder VM starting (" + printable + ")"
		return b, nil
	}
	var st builderStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		b.Step = "builder status unreadable"
		return b, nil
	}
	if st.Key != req.Key {
		b.State, b.Terminal = target.BuildFailed, true
		b.Message = fmt.Sprintf("builder %s/%s belongs to another cache key", h.Namespace, name)
		return b, nil
	}
	switch st.State {
	case "done":
		if st.File == "" || st.SHA512 == "" {
			b.State, b.Message = target.BuildFailed, "builder reported success without a file or checksum"
			return b, nil
		}
		b.State = target.BuildSucceeded
		b.Artifact = &target.Artifact{
			URL:  fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/%s", name, h.Namespace, httpPort, st.File),
			File: st.File, SHA256: st.SHA256, SHA512: st.SHA512, SizeBytes: st.SizeBytes, KubernetesVersion: st.KubernetesVersion,
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
	return b, nil
}

// Ensure implements target.Builder. Each call creates what is missing and can be created now:
// cloud-init Secret and Service first (the Secret marks the key as being built), then the base
// image, and, once Harvester has imported that, the disk and the VM.
func (h *Harvester) Ensure(ctx context.Context, req target.Request) error {
	name := h.BuilderName(req.Key)
	userdata, err := h.userData(req)
	if err != nil {
		return err
	}
	labels := map[string]string{LabelKey: labelValue(req.Key), LabelRole: "builder"}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: h.Namespace, Name: name + "-cloudinit", Labels: labels},
		Data:       map[string][]byte{"userdata": userdata},
	}
	if err := h.createIgnoreExists(ctx, secret); err != nil {
		return err
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: h.Namespace, Name: name, Labels: labels},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{labelVMName: name},
			Ports:    []corev1.ServicePort{{Name: "http", Port: httpPort, TargetPort: intstr.FromInt32(httpPort)}},
		},
	}
	if err := h.createIgnoreExists(ctx, svc); err != nil {
		return err
	}

	base, found, err := h.get(ctx, gvkImage, h.baseImageName())
	if err != nil {
		return err
	}
	if !found {
		base = newObj(gvkImage)
		base.SetNamespace(h.Namespace)
		base.SetName(h.baseImageName())
		base.Object["metadata"].(map[string]any)["labels"] = h.labels("", "builder-base")
		file := h.Builder.BaseImageURL[strings.LastIndex(h.Builder.BaseImageURL, "/")+1:]
		base.Object["spec"] = map[string]any{
			"displayName": h.Prefix + "builder-base-" + file, "description": "Base image of image build operator builder VMs",
			"sourceType": "download", "backend": "backingimage", "targetStorageClassName": h.StorageClass,
			"url": h.Builder.BaseImageURL,
		}
		return h.createIgnoreExists(ctx, base)
	}
	if state, _ := imageState(base); state != target.ImageReady {
		return nil // Harvester is still downloading the base image; the next call goes on.
	}
	baseSC, _, _ := unstructured.NestedString(base.Object, "status", "storageClassName")
	if baseSC == "" {
		return nil
	}

	block := corev1.PersistentVolumeBlock
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: h.Namespace, Name: name + "-disk", Labels: labels,
			Annotations: map[string]string{annImageID: h.Namespace + "/" + h.baseImageName()},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, VolumeMode: &block, StorageClassName: &baseSC,
		},
	}
	qty, err := parseQuantity(h.Builder.Disk)
	if err != nil {
		return err
	}
	pvc.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: qty}
	if err := h.createIgnoreExists(ctx, pvc); err != nil {
		return err
	}
	return h.createIgnoreExists(ctx, h.vm(req))
}

// vm is an ordinary VM: pod network only, no host device, nothing privileged on the Harvester side.
func (h *Harvester) vm(req target.Request) *unstructured.Unstructured {
	name := h.BuilderName(req.Key)
	u := newObj(gvkVM)
	u.SetNamespace(h.Namespace)
	u.SetName(name)
	labels := h.labels(req.Key, "builder")
	labels["harvesterhci.io/os"] = "linux"
	u.Object["metadata"].(map[string]any)["labels"] = labels
	tplLabels := h.labels(req.Key, "builder")
	tplLabels[labelVMName] = name
	u.Object["spec"] = map[string]any{
		"runStrategy": "RerunOnFailure",
		"template": map[string]any{
			"metadata": map[string]any{"labels": tplLabels},
			"spec": map[string]any{
				"hostname":                      name,
				"terminationGracePeriodSeconds": int64(30),
				"domain": map[string]any{
					"cpu":       map[string]any{"cores": h.Builder.CPU, "sockets": int64(1), "threads": int64(1)},
					"memory":    map[string]any{"guest": h.Builder.Memory},
					"resources": map[string]any{"limits": map[string]any{"cpu": fmt.Sprint(h.Builder.CPU), "memory": h.Builder.Memory}},
					"machine":   map[string]any{"type": "q35"},
					"features":  map[string]any{"acpi": map[string]any{}},
					"devices": map[string]any{
						"disks": []any{
							map[string]any{keyName: "disk-0", "bootOrder": int64(1), "disk": map[string]any{"bus": virtio}},
							map[string]any{keyName: "cloudinitdisk", "disk": map[string]any{"bus": virtio}},
						},
						"interfaces": []any{map[string]any{keyName: "default", "model": virtio, "masquerade": map[string]any{}}},
					},
				},
				"networks": []any{map[string]any{keyName: "default", "pod": map[string]any{}}},
				"volumes": []any{
					map[string]any{keyName: "disk-0", "persistentVolumeClaim": map[string]any{"claimName": name + "-disk"}},
					map[string]any{keyName: "cloudinitdisk", "cloudInitNoCloud": map[string]any{"secretRef": map[string]any{keyName: name + "-cloudinit"}}},
				},
			},
		},
	}
	return u
}

// Cleanup implements target.Builder.
func (h *Harvester) Cleanup(ctx context.Context, req target.Request) error {
	name := h.BuilderName(req.Key)
	vm := newObj(gvkVM)
	vm.SetNamespace(h.Namespace)
	vm.SetName(name)
	meta := func(n string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: h.Namespace, Name: n} }
	for _, obj := range []client.Object{
		vm,
		&corev1.Service{ObjectMeta: meta(name)},
		&corev1.PersistentVolumeClaim{ObjectMeta: meta(name + "-disk")},
		&corev1.Secret{ObjectMeta: meta(name + "-cloudinit")},
	} {
		if err := h.deleteIgnoreMissing(ctx, obj); err != nil {
			return err
		}
	}
	return nil
}
