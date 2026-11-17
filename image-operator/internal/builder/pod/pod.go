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

// Package pod is the second builder backend: the build recipe runs in a short-lived pod of a
// Kubernetes cluster (the one the operator runs in, or another one), next to the VM builder of the
// Harvester package and behind the same interface. The pod carries the least privilege the build
// was measured to need (T1.6): root inside the pod's own user namespace with two capabilities
// (CHOWN, DAC_OVERRIDE), no device, no host mount, no host network, a read-only root file system.
package pod

import (
	"context"
	_ "embed"
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/buildstatus"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/recipe"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/target"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/target/harvester"
)

//go:embed files/run-build-pod.sh
var runBuild string

const (
	// Backend is how this builder is named in a request and its status.
	Backend = "pod"

	buildContainer = "build"
	serveContainer = "serve"
	workVolume     = "work"
	filesVolume    = "files"
	filesDir       = "/etc/e3-build"
	workDir        = "/work"
	// serveUser runs the file server: any unprivileged user, the files are world-readable.
	serveUser = int64(65532)
)

// BuildCapabilities is everything the build container is given beyond an unprivileged process.
// Measured (T1.6): elemental3 unpacks container images keeping the owners of their files, which
// needs CHOWN, and then writes below directories it has given away, which needs DAC_OVERRIDE.
// Nothing else is needed: no SYS_ADMIN, no MKNOD, no loop device (systemd-repart builds the file
// systems without mounting them when it has no loop device).
var BuildCapabilities = []corev1.Capability{"CHOWN", "DAC_OVERRIDE"}

// Config is the operator-wide description of builder pods.
type Config struct {
	// Namespace the builder pods run in.
	Namespace string
	// Image holds elemental3 at ElementalRef and the tools of the recipe (builder-image/Containerfile).
	Image           string
	ImagePullPolicy corev1.PullPolicy
	// Requests and limits of the build container.
	CPURequest, CPULimit       string
	MemoryRequest, MemoryLimit string
	// WorkSize is the size of the work volume. With WorkStorageClass it is an ephemeral volume
	// claim of that class, deleted with the pod; without, an emptyDir on the node with this limit.
	WorkSize         string
	WorkStorageClass string
	// HostUsers false (the default) gives the pod its own user namespace: root in the pod is an
	// unprivileged user of the node. True is for clusters without user namespace support.
	HostUsers bool
	// ServiceType of the Service in front of the pod: ClusterIP, or NodePort when the target
	// fetches the image from outside the cluster.
	ServiceType corev1.ServiceType
	// DownloadHost is the address the target reaches the node port at. Empty: the Service's
	// cluster-internal name is used, which only works when the target is this cluster.
	DownloadHost string
	// ElementalRepo and ElementalRef (a commit) say which elemental3 Image holds; part of the cache key.
	ElementalRepo string
	ElementalRef  string
}

var safeHost = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// Validate reports what is wrong with a configuration, before any pod is built from it.
func (c Config) Validate() error {
	if c.Namespace == "" || c.Image == "" {
		return fmt.Errorf("pod builder: namespace and image are required")
	}
	for name, q := range map[string]string{
		"cpu request": c.CPURequest, "cpu limit": c.CPULimit, "memory request": c.MemoryRequest,
		"memory limit": c.MemoryLimit, "work volume size": c.WorkSize,
	} {
		if _, err := resource.ParseQuantity(q); err != nil {
			return fmt.Errorf("pod builder: %s %q: %w", name, q, err)
		}
	}
	switch c.ServiceType {
	case corev1.ServiceTypeClusterIP:
		if c.DownloadHost != "" {
			return fmt.Errorf("pod builder: a download host needs service type NodePort")
		}
	case corev1.ServiceTypeNodePort:
		if !safeHost.MatchString(c.DownloadHost) {
			return fmt.Errorf("pod builder: service type NodePort needs a download host (name or address)")
		}
	default:
		return fmt.Errorf("pod builder: service type %q is not supported", c.ServiceType)
	}
	return nil
}

// Pod builds images in pods of one namespace of one cluster.
type Pod struct {
	Client client.Client
	// Fetch reads a file the builder serves, through the cluster's API (Service proxy).
	Fetch  harvester.Fetch
	Prefix string
	Config Config
}

var _ target.Builder = (*Pod)(nil)

// BuilderName is the name of the pod, its Service and its ConfigMap for a key: the same name the VM
// builder would use, so a key has one builder name whatever the backend.
func (p *Pod) BuilderName(key string) string {
	if len(key) > 12 {
		key = key[:12]
	}
	return p.Prefix + "build-" + key
}

func (p *Pod) labels(key string) map[string]string {
	if len(key) > 40 {
		key = key[:40]
	}
	return map[string]string{harvester.LabelKey: key, harvester.LabelRole: "builder"}
}

func (p *Pod) meta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: p.Config.Namespace, Name: name}
}

// files is the ConfigMap the pod reads: the request, the recipe and the two scripts. No credential.
func (p *Pod) files(req target.Request) (*corev1.ConfigMap, error) {
	env, err := harvester.BuildEnv(req, p.Config.ElementalRepo, p.Config.ElementalRef)
	if err != nil {
		return nil, err
	}
	tarball, err := recipe.Tarball()
	if err != nil {
		return nil, err
	}
	name := p.BuilderName(req.Key)
	cm := &corev1.ConfigMap{
		ObjectMeta: p.meta(name),
		Data:       map[string]string{"env": env, "run-build-pod.sh": runBuild, "status.py": string(buildstatus.Script())},
		BinaryData: map[string][]byte{"recipe.tgz": tarball},
	}
	cm.Labels = p.labels(req.Key)
	for _, cv := range req.ChartValues {
		cm.Data["chart-values-"+cv.Chart+".yaml"] = cv.Values
	}
	return cm, nil
}

func (p *Pod) service(req target.Request) *corev1.Service {
	name := p.BuilderName(req.Key)
	svc := &corev1.Service{
		ObjectMeta: p.meta(name),
		Spec: corev1.ServiceSpec{
			Type:     p.Config.ServiceType,
			Selector: p.labels(req.Key),
			// The build container ends when the image is built, and a pod with an ended container is
			// not ready. The result is fetched exactly then, so the Service must keep routing to it.
			PublishNotReadyAddresses: true,
			Ports:                    []corev1.ServicePort{{Name: "http", Port: buildstatus.Port, TargetPort: intstr.FromInt32(buildstatus.Port)}},
		},
	}
	svc.Labels = p.labels(req.Key)
	return svc
}

// pod is the builder pod. Everything about its privileges is in this function.
func (p *Pod) pod(req target.Request) (*corev1.Pod, error) {
	if err := p.Config.Validate(); err != nil {
		return nil, err
	}
	name := p.BuilderName(req.Key)
	no, yes := false, true
	root, user := int64(0), serveUser
	hostUsers := p.Config.HostUsers
	size := resource.MustParse(p.Config.WorkSize)

	work := corev1.Volume{Name: workVolume}
	buildResources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(p.Config.CPURequest), corev1.ResourceMemory: resource.MustParse(p.Config.MemoryRequest),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(p.Config.CPULimit), corev1.ResourceMemory: resource.MustParse(p.Config.MemoryLimit),
		},
	}
	if p.Config.WorkStorageClass != "" {
		sc := p.Config.WorkStorageClass
		work.Ephemeral = &corev1.EphemeralVolumeSource{VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{
			ObjectMeta: metav1.ObjectMeta{Labels: p.labels(req.Key)},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, StorageClassName: &sc,
				Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
			},
		}}
	} else {
		// On the node's disk: bounded, and counted by the scheduler through the ephemeral-storage request.
		work.EmptyDir = &corev1.EmptyDirVolumeSource{SizeLimit: &size}
		buildResources.Requests[corev1.ResourceEphemeralStorage] = size
		buildResources.Limits[corev1.ResourceEphemeralStorage] = size
	}

	pod := &corev1.Pod{
		ObjectMeta: p.meta(name),
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  &no,
			EnableServiceLinks:            &no,
			HostUsers:                     &hostUsers,
			TerminationGracePeriodSeconds: new(int64(10)),
			SecurityContext:               &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Volumes: []corev1.Volume{
				work,
				{Name: filesVolume, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: name}}}},
			},
			Containers: []corev1.Container{
				{
					Name: buildContainer, Image: p.Config.Image, ImagePullPolicy: p.Config.ImagePullPolicy,
					Command:    []string{"/bin/bash", filesDir + "/run-build-pod.sh"},
					WorkingDir: workDir,
					Resources:  buildResources,
					SecurityContext: &corev1.SecurityContext{
						// Root, because the image must hold files owned by root and other system users.
						// With HostUsers false this root is an unprivileged user of the node.
						RunAsUser: &root, RunAsNonRoot: &no,
						Privileged: &no, AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes,
						Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: BuildCapabilities},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: workVolume, MountPath: workDir},
						// elemental3 and systemd-repart write their temporary files here; the root file system is read-only.
						{Name: workVolume, MountPath: "/tmp", SubPath: "tmp"},
						{Name: workVolume, MountPath: "/var/tmp", SubPath: "var-tmp"},
						{Name: filesVolume, MountPath: filesDir, ReadOnly: true},
					},
				},
				{
					Name: serveContainer, Image: p.Config.Image, ImagePullPolicy: p.Config.ImagePullPolicy,
					// Serves the output directory once the build container has created it.
					Command: []string{"/bin/bash", "-c",
						fmt.Sprintf("until [ -d /work/out ]; do sleep 1; done; exec python3 -I -m http.server %d --directory /work/out", buildstatus.Port)},
					Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: buildstatus.Port}},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
						Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("256Mi")},
					},
					SecurityContext: &corev1.SecurityContext{
						RunAsUser: &user, RunAsGroup: &user, RunAsNonRoot: &yes,
						Privileged: &no, AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes,
						Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					},
					VolumeMounts: []corev1.VolumeMount{{Name: workVolume, MountPath: workDir, ReadOnly: true}},
				},
			},
		},
	}
	pod.Labels = p.labels(req.Key)
	return pod, nil
}

func (p *Pod) createIgnoreExists(ctx context.Context, obj client.Object, kind string) error {
	if err := p.Client.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %s %s: %w", kind, obj.GetName(), err)
	}
	return nil
}

// Ensure implements target.Builder: ConfigMap first (it marks the key as being built), then the
// Service and the pod. While a pod of an earlier attempt is still going away, nothing is created.
func (p *Pod) Ensure(ctx context.Context, req target.Request) error {
	name := p.BuilderName(req.Key)
	old := &corev1.Pod{}
	err := p.Client.Get(ctx, client.ObjectKey{Namespace: p.Config.Namespace, Name: name}, old)
	if err == nil && old.DeletionTimestamp != nil {
		return nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	pod, err := p.pod(req)
	if err != nil {
		return err
	}
	cm, err := p.files(req)
	if err != nil {
		return err
	}
	if err := p.createIgnoreExists(ctx, cm, "ConfigMap"); err != nil {
		return err
	}
	if err := p.createIgnoreExists(ctx, p.service(req), "Service"); err != nil {
		return err
	}
	return p.createIgnoreExists(ctx, pod, "Pod")
}

// url gives the address the target fetches a file of the builder from. It fails while the Service
// has no node port.
func (p *Pod) url(ctx context.Context, name string) (func(file string) string, error) {
	if p.Config.ServiceType != corev1.ServiceTypeNodePort {
		return func(file string) string {
			return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/%s", name, p.Config.Namespace, buildstatus.Port, file)
		}, nil
	}
	svc := &corev1.Service{}
	if err := p.Client.Get(ctx, client.ObjectKey{Namespace: p.Config.Namespace, Name: name}, svc); err != nil {
		return nil, err
	}
	if len(svc.Spec.Ports) == 0 || svc.Spec.Ports[0].NodePort == 0 {
		return nil, fmt.Errorf("service %s/%s has no node port yet", p.Config.Namespace, name)
	}
	port := svc.Spec.Ports[0].NodePort
	return func(file string) string { return fmt.Sprintf("http://%s:%d/%s", p.Config.DownloadHost, port, file) }, nil
}

// podProblem describes a pod that will not finish on its own: failed as a whole (evicted, over its
// deadline) or with a build container that ended without success, for example killed for memory.
func podProblem(pod *corev1.Pod) string {
	if pod.Status.Phase == corev1.PodFailed {
		return strings.TrimSpace(fmt.Sprintf("builder pod failed: %s %s", pod.Status.Reason, pod.Status.Message))
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			return strings.TrimSpace(fmt.Sprintf("container %s of the builder pod ended: %s (exit status %d) %s", cs.Name, t.Reason, t.ExitCode, t.Message))
		}
	}
	return ""
}

// podStep describes a pod that is not serving yet, so a request shows why it waits.
func podStep(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" {
			return fmt.Sprintf("builder pod waiting: container %s: %s", cs.Name, w.Reason)
		}
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return strings.TrimSpace(fmt.Sprintf("builder pod not scheduled: %s %s", c.Reason, c.Message))
		}
	}
	return fmt.Sprintf("builder pod starting (%s)", pod.Status.Phase)
}

// Status implements target.Builder.
func (p *Pod) Status(ctx context.Context, req target.Request) (target.Build, error) {
	name := p.BuilderName(req.Key)
	cm := &corev1.ConfigMap{}
	err := p.Client.Get(ctx, client.ObjectKey{Namespace: p.Config.Namespace, Name: name}, cm)
	claimed := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return target.Build{}, err
	}
	pod := &corev1.Pod{}
	err = p.Client.Get(ctx, client.ObjectKey{Namespace: p.Config.Namespace, Name: name}, pod)
	if apierrors.IsNotFound(err) {
		if !claimed {
			return target.Build{State: target.BuildAbsent, Name: name, Backend: Backend}, nil
		}
		return target.Build{State: target.BuildRunning, Name: name, Backend: Backend, Created: cm.CreationTimestamp.Time, Step: "preparing the builder"}, nil
	}
	if err != nil {
		return target.Build{}, err
	}
	b := target.Build{State: target.BuildRunning, Name: name, Backend: Backend, Created: pod.CreationTimestamp.Time}
	if pod.DeletionTimestamp != nil {
		// The pod of an attempt that was already counted; Ensure waits for it to be gone.
		b.Step = "earlier builder pod terminating"
		return b, nil
	}
	raw, err := p.Fetch(ctx, p.Config.Namespace, name, buildstatus.File)
	if err == nil {
		url, uerr := p.url(ctx, name)
		if uerr != nil {
			url = func(string) string { return "" }
		}
		buildstatus.Apply(&b, raw, req, p.Config.Namespace+"/"+name, url)
		if b.State == target.BuildSucceeded && uerr != nil {
			// Built, but there is no address yet the target could fetch the image from.
			b.State, b.Artifact, b.Step = target.BuildRunning, nil, uerr.Error()
			return b, nil
		}
		if b.State != target.BuildRunning {
			return b, nil
		}
	}
	// The builder has not reported an end. A pod that can not go on is a failed attempt.
	if msg := podProblem(pod); msg != "" {
		b.State, b.Message = target.BuildFailed, msg
		return b, nil
	}
	if err != nil {
		b.Step = podStep(pod)
	}
	return b, nil
}

// Cleanup implements target.Builder. The work volume goes with the pod.
func (p *Pod) Cleanup(ctx context.Context, req target.Request) error {
	name := p.BuilderName(req.Key)
	for _, obj := range []client.Object{
		&corev1.Pod{ObjectMeta: p.meta(name)},
		&corev1.Service{ObjectMeta: p.meta(name)},
		&corev1.ConfigMap{ObjectMeta: p.meta(name)},
	} {
		if err := p.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete %s: %w", obj.GetName(), err)
		}
	}
	return nil
}
