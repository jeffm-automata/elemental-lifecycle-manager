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

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	imagev1 "github.com/suse/elemental-lifecycle-manager/image-operator/api/v1alpha1"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/release"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/target"
)

const testDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

// world is a fake target: images and builders by cache key, and counters of what was asked of it.
type world struct {
	images   map[string]*target.Image
	builders map[string]*target.Build
	created  int // builders created
	deleted  int // images deleted
	failWith string
	terminal bool
	err      error
	// deliverErr is returned by Deliver.
	deliverErr error
}

func newWorld() *world {
	return &world{images: map[string]*target.Image{}, builders: map[string]*target.Build{}}
}

func (w *world) For(context.Context, *imagev1.NodeImage) (target.Target, target.Builder, error) {
	return w, w, w.err
}

func (w *world) FindImage(_ context.Context, req target.Request) (target.Image, error) {
	if i, ok := w.images[req.Key]; ok {
		return *i, nil
	}
	return target.Image{State: target.ImageAbsent}, nil
}

func (w *world) Deliver(_ context.Context, req target.Request, a target.Artifact) error {
	if w.deliverErr != nil {
		return w.deliverErr
	}
	if _, ok := w.images[req.Key]; !ok {
		w.images[req.Key] = &target.Image{State: target.ImageImporting, Name: "img-" + req.Key[:12], Namespace: "tgt", SHA256: a.SHA256, KubernetesVersion: a.KubernetesVersion}
	}
	return nil
}

func (w *world) DeleteImage(_ context.Context, req target.Request) error {
	if _, ok := w.images[req.Key]; ok {
		w.deleted++
	}
	delete(w.images, req.Key)
	return nil
}

func (w *world) Status(_ context.Context, req target.Request) (target.Build, error) {
	if b, ok := w.builders[req.Key]; ok {
		return *b, nil
	}
	return target.Build{State: target.BuildAbsent, Name: "build-" + req.Key[:12]}, nil
}

func (w *world) Ensure(_ context.Context, req target.Request) error {
	if _, ok := w.builders[req.Key]; !ok {
		w.created++
		w.builders[req.Key] = &target.Build{State: target.BuildRunning, Name: "build-" + req.Key[:12]}
	}
	return nil
}

func (w *world) Cleanup(_ context.Context, req target.Request) error {
	delete(w.builders, req.Key)
	return nil
}

// finishBuilds lets every running builder end, as success or as the failure configured.
func (w *world) finishBuilds() {
	for _, b := range w.builders {
		if b.State != target.BuildRunning {
			continue
		}
		if w.failWith != "" {
			b.State, b.Message, b.Terminal = target.BuildFailed, w.failWith, w.terminal
			continue
		}
		b.State = target.BuildSucceeded
		b.Artifact = &target.Artifact{URL: "http://builder/x.qcow2", File: "x.qcow2", SHA256: "aa", SHA512: "bb", SizeBytes: 1, KubernetesVersion: "v1.2.3+rke2r1"}
	}
}

func (w *world) finishImports() {
	for _, i := range w.images {
		i.State = target.ImageReady
	}
}

type resolver struct {
	digest string
	err    error
	calls  int
}

func (r *resolver) Resolve(context.Context, string) (string, error) {
	r.calls++
	return r.digest, r.err
}

func nodeImage(name string) *imagev1.NodeImage {
	return &imagev1.NodeImage{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name, Generation: 1},
		Spec: imagev1.NodeImageSpec{
			Release: imagev1.ReleaseSpec{Manifest: "registry.example/rel/manifest:1.0"},
			Arch:    "x86_64",
			Target: imagev1.TargetSpec{
				Type: imagev1.TargetHarvester, CredentialsSecretRef: imagev1.SecretRef{Name: "creds"},
				Harvester: &imagev1.HarvesterTarget{Namespace: "tgt", StorageClassName: "sc", NamePrefix: "p-"},
			},
			Config:         imagev1.ConfigSpec{DiskSize: "20G"},
			DeletionPolicy: imagev1.DeletionRetain,
		},
	}
}

type harness struct {
	t *testing.T
	c client.Client
	r *NodeImageReconciler
	w *world
	s *resolver
}

func newHarness(t *testing.T, objs ...client.Object) *harness {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := imagev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(&imagev1.NodeImage{}).Build()
	w, s := newWorld(), &resolver{digest: testDigest}
	return &harness{t: t, c: c, w: w, s: s, r: &NodeImageReconciler{
		Client: c, Resolver: s, Backends: w,
		Tooling: Tooling{RecipeDigest: "sha256:r", ToolRepo: "https://example.invalid/e.git", ToolCommit: strings.Repeat("c", 40)},
		Options: Options{MaxAttempts: 2, BuildTimeout: time.Hour, DeliveryTimeout: time.Hour, PollBuild: time.Second, PollDelivery: time.Second},
	}}
}

// reconcile runs one reconcile and returns the object as stored afterwards. A new object first gets
// its finalizer in a reconcile of its own (as the update event would cause); that one is run here too.
func (h *harness) reconcile(name string) (*imagev1.NodeImage, ctrl.Result, error) {
	h.t.Helper()
	key := types.NamespacedName{Namespace: "ns", Name: name}
	fresh := &imagev1.NodeImage{}
	if err := h.c.Get(context.Background(), key, fresh); err == nil && len(fresh.Finalizers) == 0 {
		if _, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			h.t.Fatalf("first reconcile of %s: %v", name, err)
		}
	}
	res, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	ni := &imagev1.NodeImage{}
	if gerr := h.c.Get(context.Background(), key, ni); gerr != nil {
		ni = nil
	}
	return ni, res, err
}

func (h *harness) mustReconcile(name string) {
	h.t.Helper()
	if _, _, err := h.reconcile(name); err != nil {
		h.t.Fatalf("reconcile %s: %v", name, err)
	}
}

func (h *harness) wantPhase(ni *imagev1.NodeImage, p imagev1.Phase) {
	h.t.Helper()
	if ni.Status.Phase != p {
		h.t.Fatalf("phase %s, want %s (conditions %+v)", ni.Status.Phase, p, ni.Status.Conditions)
	}
}

func TestMissThenHit(t *testing.T) {
	h := newHarness(t, nodeImage("a"))

	ni, res, err := h.reconcile("a")
	if err != nil {
		t.Fatal(err)
	}
	h.wantPhase(ni, imagev1.PhaseBuilding)
	if h.w.created != 1 || res.RequeueAfter == 0 || ni.Status.CacheKey == "" || ni.Status.Release.ManifestDigest != testDigest {
		t.Fatalf("after first reconcile: builders %d, requeue %s, status %+v", h.w.created, res.RequeueAfter, ni.Status)
	}
	if ni, _, _ = h.reconcile("a"); ni.Status.Phase != imagev1.PhaseBuilding || h.w.created != 1 {
		t.Fatalf("a running builder must be waited for, not recreated (builders %d)", h.w.created)
	}

	h.w.finishBuilds()
	ni, _, _ = h.reconcile("a")
	h.wantPhase(ni, imagev1.PhaseDelivering)
	if !meta.IsStatusConditionTrue(ni.Status.Conditions, imagev1.ConditionBuilt) {
		t.Fatal("Built is not true after the build")
	}
	ni, _, _ = h.reconcile("a")
	h.wantPhase(ni, imagev1.PhaseDelivering)

	h.w.finishImports()
	ni, res, _ = h.reconcile("a")
	h.wantPhase(ni, imagev1.PhaseReady)
	if ni.Status.CacheHit == nil || *ni.Status.CacheHit {
		t.Fatal("the request that built the image must not count as a cache hit")
	}
	if len(h.w.builders) != 0 {
		t.Fatal("the builder was not removed after delivery")
	}
	if res.RequeueAfter != 0 || ni.Status.Image == nil || ni.Status.Image.KubernetesVersion != "v1.2.3+rke2r1" || ni.Status.ReadyAt == nil {
		t.Fatalf("ready status incomplete: %+v", ni.Status)
	}
	if ni, _, _ = h.reconcile("a"); *ni.Status.CacheHit {
		t.Fatal("a later reconcile turned the miss into a hit")
	}
	if c := meta.FindStatusCondition(ni.Status.Conditions, imagev1.ConditionBuilt); !strings.Contains(c.Message, "x.qcow2") {
		t.Fatalf("the Built condition lost what was built: %+v", c)
	}

	// A second, identical request under another name: ready at once, no builder.
	if err := h.c.Create(context.Background(), nodeImage("b")); err != nil {
		t.Fatal(err)
	}
	nb, _, err := h.reconcile("b")
	if err != nil {
		t.Fatal(err)
	}
	h.wantPhase(nb, imagev1.PhaseReady)
	if nb.Status.CacheHit == nil || !*nb.Status.CacheHit || nb.Status.Build != nil || h.w.created != 1 {
		t.Fatalf("second request: cacheHit %v, build %+v, builders created %d", nb.Status.CacheHit, nb.Status.Build, h.w.created)
	}
	if nb.Status.CacheKey != ni.Status.CacheKey || nb.Status.Image.Name != ni.Status.Image.Name {
		t.Fatal("identical requests got different keys or images")
	}

	// One changed input: a miss.
	c := nodeImage("c")
	c.Spec.Config.DiskSize = "21G"
	if err := h.c.Create(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	nc, _, _ := h.reconcile("c")
	h.wantPhase(nc, imagev1.PhaseBuilding)
	if nc.Status.CacheKey == ni.Status.CacheKey || h.w.created != 2 {
		t.Fatalf("changed input: same key %v, builders created %d", nc.Status.CacheKey == ni.Status.CacheKey, h.w.created)
	}
}

func TestMissingReleaseFailsOnce(t *testing.T) {
	h := newHarness(t, nodeImage("a"))
	h.s.err = &release.Error{Kind: release.KindNotFound, Ref: "x", Err: errors.New("MANIFEST_UNKNOWN")}
	ni, res, err := h.reconcile("a")
	if err != nil || res.RequeueAfter != 0 {
		t.Fatalf("a missing release must not be retried: err %v, requeue %s", err, res.RequeueAfter)
	}
	h.wantPhase(ni, imagev1.PhaseFailed)
	c := meta.FindStatusCondition(ni.Status.Conditions, imagev1.ConditionReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != imagev1.ReasonReleaseNotFound || !strings.Contains(c.Message, "MANIFEST_UNKNOWN") {
		t.Fatalf("Ready condition: %+v", c)
	}
	for range 3 {
		if _, res, err = h.reconcile("a"); err != nil || res.RequeueAfter != 0 {
			t.Fatal("a failed request asked to be reconciled again")
		}
	}
	if h.s.calls != 1 || h.w.created != 0 {
		t.Fatalf("registry asked %d times, builders created %d", h.s.calls, h.w.created)
	}
}

func TestRegistryOutageIsRetriedNotFailed(t *testing.T) {
	h := newHarness(t, nodeImage("a"))
	h.s.err = &release.Error{Kind: release.KindTransient, Ref: "x", Err: errors.New("connection refused")}
	ni, _, err := h.reconcile("a")
	if err == nil {
		t.Fatal("a transient error must be returned so that the controller backs off and retries")
	}
	h.wantPhase(ni, imagev1.PhasePending)
	h.s.err = nil
	ni, _, _ = h.reconcile("a")
	h.wantPhase(ni, imagev1.PhaseBuilding)
}

func TestBuildFailuresAreBounded(t *testing.T) {
	h := newHarness(t, nodeImage("a"))
	h.w.failWith = "customize failed\nlast log line"
	var ni *imagev1.NodeImage
	for range 20 {
		ni, _, _ = h.reconcile("a")
		h.w.finishBuilds()
	}
	h.wantPhase(ni, imagev1.PhaseFailed)
	if h.w.created != 2 || ni.Status.Build.Attempts != 2 || len(h.w.builders) != 0 {
		t.Fatalf("builders created %d, attempts %d, builders left %d", h.w.created, ni.Status.Build.Attempts, len(h.w.builders))
	}
	c := meta.FindStatusCondition(ni.Status.Conditions, imagev1.ConditionReady)
	if c.Reason != imagev1.ReasonBuildFailed || !strings.Contains(c.Message, "last log line") {
		t.Fatalf("Ready condition: %+v", c)
	}
}

func TestRejectedImageIsNotRetried(t *testing.T) {
	h := newHarness(t, nodeImage("a"))
	h.w.failWith, h.w.terminal = "inspection found a token", true
	h.mustReconcile("a")
	h.w.finishBuilds()
	ni, _, _ := h.reconcile("a")
	h.wantPhase(ni, imagev1.PhaseFailed)
	if c := meta.FindStatusCondition(ni.Status.Conditions, imagev1.ConditionReady); c.Reason != imagev1.ReasonImageRejected {
		t.Fatalf("reason %s", c.Reason)
	}
	if h.w.created != 1 {
		t.Fatalf("builders created %d, want 1", h.w.created)
	}
}

func TestBuildDeadline(t *testing.T) {
	h := newHarness(t, nodeImage("a"))
	now := time.Now()
	h.r.Now = func() time.Time { return now }
	h.mustReconcile("a")
	for _, b := range h.w.builders {
		b.Created = now
	}
	now = now.Add(2 * time.Hour)
	ni, _, _ := h.reconcile("a")
	if ni.Status.Build.Attempts != 1 || !strings.Contains(ni.Status.Build.LastError, "did not finish within") || len(h.w.builders) != 0 {
		t.Fatalf("overdue build not counted: %+v", ni.Status.Build)
	}
}

func TestCredentialErrors(t *testing.T) {
	h := newHarness(t, nodeImage("a"))
	h.w.err = &BackendError{Reason: imagev1.ReasonCredentialsInvalid, Terminal: true, Err: errors.New("no key kubeconfig")}
	ni, _, err := h.reconcile("a")
	if err != nil {
		t.Fatal(err)
	}
	h.wantPhase(ni, imagev1.PhaseFailed)

	h = newHarness(t, nodeImage("a"))
	h.w.err = &BackendError{Reason: imagev1.ReasonCredentialsInvalid, Err: errors.New("secret does not exist")}
	ni, _, err = h.reconcile("a")
	if err == nil || ni.Status.Phase == imagev1.PhaseFailed {
		t.Fatal("a Secret that does not exist yet must be waited for")
	}
}

func TestUnsupportedTarget(t *testing.T) {
	ni := nodeImage("a")
	ni.Spec.Target.Type, ni.Spec.Target.Harvester = "vsphere", nil
	h := newHarness(t, ni)
	got, _, _ := h.reconcile("a")
	h.wantPhase(got, imagev1.PhaseFailed)
	if c := meta.FindStatusCondition(got.Status.Conditions, imagev1.ConditionReady); c.Reason != imagev1.ReasonUnsupportedTarget {
		t.Fatalf("reason %s", c.Reason)
	}
	if h.s.calls != 0 {
		t.Fatal("the registry was asked for an unsupported target")
	}
}

func TestDeletion(t *testing.T) {
	a, b := nodeImage("a"), nodeImage("b")
	b.Spec.DeletionPolicy = imagev1.DeletionDelete
	h := newHarness(t, a, b)
	h.mustReconcile("a")
	h.w.finishBuilds()
	h.mustReconcile("a")
	h.w.finishImports()
	h.mustReconcile("a")
	h.mustReconcile("b")

	// b asks for deletion of the image, but a still wants it.
	ctx := context.Background()
	if err := h.c.Delete(ctx, b); err != nil {
		t.Fatal(err)
	}
	if got, _, err := h.reconcile("b"); err != nil || got != nil {
		t.Fatalf("b not gone: %v %v", got, err)
	}
	if len(h.w.images) != 1 || h.w.deleted != 0 {
		t.Fatal("the image was deleted although another request shares it")
	}

	// a is Retain: the image stays when the last request goes.
	if err := h.c.Delete(ctx, a); err != nil {
		t.Fatal(err)
	}
	if got, _, err := h.reconcile("a"); err != nil || got != nil {
		t.Fatalf("a not gone: %v %v", got, err)
	}
	if len(h.w.images) != 1 {
		t.Fatal("deletionPolicy Retain deleted the image")
	}

	// A lone request with Delete takes the image (and a running builder) with it.
	c := nodeImage("c")
	c.Spec.DeletionPolicy = imagev1.DeletionDelete
	c.Spec.Config.DiskSize = "30G"
	if err := h.c.Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile("c")
	h.w.finishBuilds()
	h.mustReconcile("c")
	nc, _, _ := h.reconcile("c")
	h.wantPhase(nc, imagev1.PhaseDelivering)
	if err := h.c.Delete(ctx, nc); err != nil {
		t.Fatal(err)
	}
	if got, _, err := h.reconcile("c"); err != nil || got != nil {
		t.Fatalf("c not gone: %v %v", got, err)
	}
	if len(h.w.images) != 1 || len(h.w.builders) != 0 || h.w.deleted != 1 {
		t.Fatalf("after deleting c: images %d, builders %d, deleted %d", len(h.w.images), len(h.w.builders), h.w.deleted)
	}
}

// An object the target refuses as invalid will be refused again: the request must end, not retry
// for ever, and must not leave its builder running. Other target errors are retried.
func TestTargetRefusal(t *testing.T) {
	h := newHarness(t, nodeImage("a"))
	h.mustReconcile("a")
	h.w.finishBuilds()
	h.w.deliverErr = fmt.Errorf("create image: %w",
		apierrors.NewInvalid(schema.GroupKind{Group: "example", Kind: "Image"}, "x", nil))
	ni, res, err := h.reconcile("a")
	if err != nil || res.RequeueAfter != 0 {
		t.Fatalf("an invalid object must not be retried: %v %s", err, res.RequeueAfter)
	}
	h.wantPhase(ni, imagev1.PhaseFailed)
	if c := meta.FindStatusCondition(ni.Status.Conditions, imagev1.ConditionReady); c.Reason != imagev1.ReasonTargetRejected {
		t.Fatalf("reason %s", c.Reason)
	}
	if len(h.w.builders) != 0 {
		t.Fatal("the builder was left running")
	}

	h = newHarness(t, nodeImage("a"))
	h.mustReconcile("a")
	h.w.finishBuilds()
	h.w.deliverErr = apierrors.NewServiceUnavailable("try later")
	ni, _, err = h.reconcile("a")
	if err == nil || ni.Status.Phase == imagev1.PhaseFailed || len(h.w.builders) != 1 {
		t.Fatalf("an unavailable target must be retried with the builder kept: %v, phase %s", err, ni.Status.Phase)
	}
	h.w.deliverErr = nil
	ni, _, _ = h.reconcile("a")
	h.wantPhase(ni, imagev1.PhaseDelivering)
}

// The first reconcile of a new object only adds the finalizer; it must not ask the registry or the
// target, or everything would be done twice (seen in the lab as a conflict and a second registry request).
func TestFirstReconcileOnlyAddsTheFinalizer(t *testing.T) {
	h := newHarness(t, nodeImage("a"))
	key := types.NamespacedName{Namespace: "ns", Name: "a"}
	res, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil || res.RequeueAfter != 0 {
		t.Fatalf("%v %s", err, res.RequeueAfter)
	}
	ni := &imagev1.NodeImage{}
	if err := h.c.Get(context.Background(), key, ni); err != nil {
		t.Fatal(err)
	}
	if len(ni.Finalizers) != 1 || ni.Status.Phase != "" || h.s.calls != 0 || h.w.created != 0 {
		t.Fatalf("finalizers %v, phase %q, registry calls %d, builders %d", ni.Finalizers, ni.Status.Phase, h.s.calls, h.w.created)
	}
	if _, _, err := h.reconcile("a"); err != nil || h.s.calls != 1 {
		t.Fatalf("second reconcile: %v, registry calls %d", err, h.s.calls)
	}
}
