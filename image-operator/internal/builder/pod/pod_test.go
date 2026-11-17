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

package pod

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/target"
)

const (
	ns       = "builders"
	workSize = "30Gi"
)

var req = target.Request{
	Key:            strings.Repeat("ab", 32),
	ManifestRef:    "registry.example/rel/rke2-manifest:1.37.2-20.3",
	ManifestDigest: "sha256:" + strings.Repeat("1", 64),
	Arch:           "x86_64", DiskSize: "20G",
	ChartValues: []target.ChartValues{{Chart: "some-chart", Values: "image:\n  repository: \"x'y; rm -rf /\"\n"}},
}

func config() Config {
	return Config{
		Namespace: ns, Image: "registry.example/builder@sha256:" + strings.Repeat("2", 64), ImagePullPolicy: corev1.PullIfNotPresent,
		CPURequest: "1", CPULimit: "4", MemoryRequest: "2Gi", MemoryLimit: "6Gi", WorkSize: workSize,
		ServiceType: corev1.ServiceTypeNodePort, DownloadHost: "192.0.2.10",
		ElementalRepo: "https://example.invalid/e.git", ElementalRef: strings.Repeat("c", 40),
	}
}

func newBuilder(t *testing.T) (*Pod, *string) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	status := ""
	p := &Pod{
		Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Prefix: "p-", Config: config(),
		Fetch: func(context.Context, string, string, string) ([]byte, error) {
			if status == "" {
				return nil, errors.New("service unavailable")
			}
			return []byte(status), nil
		},
	}
	return p, &status
}

func getPod(t *testing.T, p *Pod) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{}
	if err := p.Client.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: p.BuilderName(req.Key)}, pod); err != nil {
		t.Fatal(err)
	}
	return pod
}

// The pod spec is the privilege statement of this backend. Any widening must fail here first.
func TestPodPrivileges(t *testing.T) {
	p, _ := newBuilder(t)
	pod, err := p.pod(req)
	if err != nil {
		t.Fatal(err)
	}
	s := pod.Spec
	if s.HostNetwork || s.HostPID || s.HostIPC || s.HostUsers == nil || *s.HostUsers {
		t.Errorf("host namespaces: network %v pid %v ipc %v users %v", s.HostNetwork, s.HostPID, s.HostIPC, s.HostUsers)
	}
	if s.AutomountServiceAccountToken == nil || *s.AutomountServiceAccountToken {
		t.Error("the builder must not get a service account token")
	}
	if s.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restart policy %s", s.RestartPolicy)
	}
	if s.SecurityContext == nil || s.SecurityContext.SeccompProfile == nil || s.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("seccomp must be RuntimeDefault")
	}
	for _, v := range s.Volumes {
		if v.HostPath != nil || (v.EmptyDir == nil && v.ConfigMap == nil && v.Ephemeral == nil) {
			t.Errorf("volume %s: only emptyDir, ephemeral claim and ConfigMap are allowed", v.Name)
		}
	}
	if len(s.Containers) != 2 || len(s.InitContainers) != 0 || len(s.EphemeralContainers) != 0 {
		t.Fatalf("containers: %d, init %d", len(s.Containers), len(s.InitContainers))
	}
	for _, c := range s.Containers {
		if !confined(t, c) {
			continue
		}
		switch c.Name {
		case buildContainer:
			checkBuildContainer(t, c)
		case serveContainer:
			checkServeContainer(t, c)
		default:
			t.Errorf("unexpected container %s", c.Name)
		}
	}
}

// confined checks what holds for every container of the pod.
func confined(t *testing.T, c corev1.Container) bool {
	t.Helper()
	sc := c.SecurityContext
	if sc == nil || sc.Privileged == nil || *sc.Privileged || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Errorf("container %s: privileged, privilege escalation or a writable root file system", c.Name)
		return false
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("container %s does not drop all capabilities", c.Name)
		return false
	}
	if len(c.VolumeDevices) != 0 {
		t.Errorf("container %s has a device", c.Name)
	}
	if c.Resources.Requests.Memory().IsZero() || c.Resources.Limits.Memory().IsZero() || c.Resources.Limits.Cpu().IsZero() {
		t.Errorf("container %s has no requests or limits", c.Name)
	}
	return true
}

func checkBuildContainer(t *testing.T, c corev1.Container) {
	t.Helper()
	sc := c.SecurityContext
	if got := sc.Capabilities.Add; len(got) != 2 || got[0] != "CHOWN" || got[1] != "DAC_OVERRIDE" {
		t.Errorf("build container capabilities %v, measured minimum is CHOWN and DAC_OVERRIDE", got)
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 0 {
		t.Error("the build container runs as root of the pod's user namespace")
	}
	if c.Resources.Limits.Memory().String() != "6Gi" || c.Resources.Requests.Cpu().String() != "1" {
		t.Errorf("build resources %v", c.Resources)
	}
}

func checkServeContainer(t *testing.T, c corev1.Container) {
	t.Helper()
	sc := c.SecurityContext
	if len(sc.Capabilities.Add) != 0 || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.RunAsUser == nil || *sc.RunAsUser == 0 {
		t.Error("the serve container must be unprivileged and not root")
	}
	for _, m := range c.VolumeMounts {
		if !m.ReadOnly {
			t.Errorf("serve container mount %s is writable", m.MountPath)
		}
	}
}

func TestWorkVolume(t *testing.T) {
	p, _ := newBuilder(t)
	pod, _ := p.pod(req)
	v := pod.Spec.Volumes[0]
	if v.EmptyDir == nil || v.EmptyDir.SizeLimit.String() != workSize || v.EmptyDir.Medium != "" {
		t.Fatalf("default work volume must be a disk emptyDir with the size as limit: %+v", v)
	}
	if got := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceEphemeralStorage]; got.String() != workSize {
		t.Errorf("ephemeral-storage request %s", got.String())
	}
	p.Config.WorkStorageClass = "fast"
	pod, _ = p.pod(req)
	v = pod.Spec.Volumes[0]
	if v.Ephemeral == nil || *v.Ephemeral.VolumeClaimTemplate.Spec.StorageClassName != "fast" ||
		v.Ephemeral.VolumeClaimTemplate.Spec.Resources.Requests.Storage().String() != workSize {
		t.Fatalf("with a storage class the work volume must be an ephemeral claim: %+v", v)
	}
	if _, ok := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceEphemeralStorage]; ok {
		t.Error("no ephemeral-storage request with a claim")
	}
	p.Config.HostUsers = true
	if pod, _ = p.pod(req); !*pod.Spec.HostUsers {
		t.Error("HostUsers true must reach the pod")
	}
}

func TestValidate(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"no image":               func(c *Config) { c.Image = "" },
		"bad memory":             func(c *Config) { c.MemoryLimit = "lots" },
		"bad size":               func(c *Config) { c.WorkSize = "" },
		"node port without host": func(c *Config) { c.DownloadHost = "" },
		"host with a path":       func(c *Config) { c.DownloadHost = "host/x?y" },
		"cluster ip with host":   func(c *Config) { c.ServiceType = corev1.ServiceTypeClusterIP },
		"load balancer":          func(c *Config) { c.ServiceType = corev1.ServiceTypeLoadBalancer },
	} {
		c := config()
		change(&c)
		if c.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := config()
	c.ServiceType, c.DownloadHost = corev1.ServiceTypeClusterIP, ""
	if err := c.Validate(); err != nil {
		t.Error(err)
	}
	p, _ := newBuilder(t)
	p.Config.MemoryLimit = "lots"
	if err := p.Ensure(context.Background(), req); err == nil {
		t.Error("Ensure with an invalid configuration must fail")
	}
	cms := &corev1.ConfigMapList{}
	if _ = p.Client.List(context.Background(), cms); len(cms.Items) != 0 {
		t.Error("an invalid configuration must create nothing")
	}
}

func TestFiles(t *testing.T) {
	p, _ := newBuilder(t)
	cm, err := p.files(req)
	if err != nil {
		t.Fatal(err)
	}
	env := cm.Data["env"]
	for _, want := range []string{"MANIFEST='" + req.ManifestRef + "'", "EXPECT_DIGEST='" + req.ManifestDigest + "'", "KEY='" + req.Key + "'",
		"ELEMENTAL_REF='" + strings.Repeat("c", 40) + "'", "NAME='elemental-node-1.37.2-20.3-harvester.x86_64'"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s:\n%s", want, env)
		}
	}
	// Chart values are data in a file of their own; they never reach a shell.
	if cm.Data["chart-values-some-chart.yaml"] != req.ChartValues[0].Values || strings.Contains(env, "rm -rf") {
		t.Error("chart values must be a file, verbatim")
	}
	if len(cm.BinaryData["recipe.tgz"]) == 0 || cm.Data["run-build-pod.sh"] == "" || cm.Data["status.py"] == "" {
		t.Error("recipe or scripts missing")
	}
	bad := req
	bad.ManifestRef = "x'; touch /tmp/pwned; '"
	if _, err := p.files(bad); err == nil {
		t.Error("a manifest reference with shell characters must be refused")
	}
}

func TestLifecycle(t *testing.T) {
	ctx := context.Background()
	p, status := newBuilder(t)
	name := p.BuilderName(req.Key)
	if name != "p-build-abababababab" {
		t.Fatalf("name %s", name)
	}
	if b, err := p.Status(ctx, req); err != nil || b.State != target.BuildAbsent {
		t.Fatalf("before Ensure: %+v %v", b, err)
	}
	for range 2 { // idempotent
		if err := p.Ensure(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	pod := getPod(t, p)
	svc := &corev1.Service{}
	if err := p.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, svc); err != nil {
		t.Fatal(err)
	}
	if svc.Spec.Type != corev1.ServiceTypeNodePort || svc.Spec.Selector["image.lifecycle.suse.com/role"] != "builder" || pod.Labels["image.lifecycle.suse.com/role"] != "builder" {
		t.Errorf("service %+v does not select the pod %v", svc.Spec, pod.Labels)
	}
	if !svc.Spec.PublishNotReadyAddresses {
		t.Error("the Service must route to the pod after the build container has ended (pod not ready)")
	}
	b, _ := p.Status(ctx, req)
	if b.State != target.BuildRunning || !strings.Contains(b.Step, "starting") {
		t.Fatalf("a pod that does not serve yet is running: %+v", b)
	}
	*status = `{"key":"` + req.Key + `","state":"building","step":"build"}`
	if b, _ = p.Status(ctx, req); b.State != target.BuildRunning || b.Step != "build" {
		t.Fatalf("building: %+v", b)
	}
	// Done, but the API server has not given the Service a node port yet: not deliverable.
	*status = `{"key":"` + req.Key + `","state":"done","file":"f.qcow2","sha512":"s5","sha256":"s2"}`
	if b, _ = p.Status(ctx, req); b.State != target.BuildRunning || !strings.Contains(b.Step, "node port") {
		t.Fatalf("done without a node port: %+v", b)
	}
	svc.Spec.Ports[0].NodePort = 31234
	if err := p.Client.Update(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if b, _ = p.Status(ctx, req); b.State != target.BuildSucceeded || b.Artifact.URL != "http://192.0.2.10:31234/f.qcow2" {
		t.Fatalf("done: %+v %+v", b, b.Artifact)
	}
	p.Config.ServiceType, p.Config.DownloadHost = corev1.ServiceTypeClusterIP, ""
	if b, _ = p.Status(ctx, req); b.Artifact == nil || b.Artifact.URL != "http://p-build-abababababab.builders.svc.cluster.local:8080/f.qcow2" {
		t.Fatalf("cluster-internal URL: %+v", b.Artifact)
	}
	for range 2 {
		if err := p.Cleanup(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	*status = ""
	if b, _ = p.Status(ctx, req); b.State != target.BuildAbsent {
		t.Fatalf("after Cleanup: %+v", b)
	}
	for _, obj := range []client.Object{&corev1.Pod{}, &corev1.Service{}, &corev1.ConfigMap{}} {
		if err := p.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, obj); !apierrors.IsNotFound(err) {
			t.Errorf("%T left behind: %v", obj, err)
		}
	}
}

func TestPodFailures(t *testing.T) {
	ctx := context.Background()
	p, status := newBuilder(t)
	if err := p.Ensure(ctx, req); err != nil {
		t.Fatal(err)
	}
	pod := getPod(t, p)

	// The image can not be pulled: still running (the build deadline ends it), but the request says why.
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: buildContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}
	if err := p.Client.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if b, _ := p.Status(ctx, req); b.State != target.BuildRunning || !strings.Contains(b.Step, "ImagePullBackOff") {
		t.Fatalf("image pull: %+v", b)
	}

	// The build container is killed for memory: status.json still says building, the pod knows better.
	*status = `{"key":"` + req.Key + `","state":"building","step":"build"}`
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: buildContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}}}}
	if err := p.Client.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	b, _ := p.Status(ctx, req)
	if b.State != target.BuildFailed || b.Terminal || !strings.Contains(b.Message, "OOMKilled") {
		t.Fatalf("killed build container: %+v", b)
	}

	// The recipe's own verdict wins over the container's exit status, and exit 3 is terminal.
	*status = `{"key":"` + req.Key + `","state":"failed","exitCode":3,"error":"rejected"}`
	if b, _ = p.Status(ctx, req); b.State != target.BuildFailed || !b.Terminal || !strings.Contains(b.Message, "rejected") {
		t.Fatalf("rejected image: %+v", b)
	}

	// An evicted pod serves nothing.
	*status = ""
	pod.Status.ContainerStatuses = nil
	pod.Status.Phase, pod.Status.Reason = corev1.PodFailed, "Evicted"
	if err := p.Client.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if b, _ = p.Status(ctx, req); b.State != target.BuildFailed || !strings.Contains(b.Message, "Evicted") {
		t.Fatalf("evicted: %+v", b)
	}
}

// A failed attempt is counted once: while its pod is going away, the builder is "running" and
// Ensure creates nothing new.
func TestTerminatingPodIsNotASecondFailure(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	now := metav1.Now()
	p, _ := newBuilder(t)
	old := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: p.BuilderName(req.Key), DeletionTimestamp: &now, Finalizers: []string{"test/hold"}},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"},
	}
	p.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(old).Build()
	if b, _ := p.Status(ctx, req); b.State != target.BuildRunning || !strings.Contains(b.Step, "terminating") {
		t.Fatalf("terminating pod: %+v", b)
	}
	if err := p.Ensure(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := p.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: old.Name}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Errorf("Ensure created objects next to a terminating pod: %v", err)
	}
}
