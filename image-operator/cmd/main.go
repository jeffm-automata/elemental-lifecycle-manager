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

// The image build operator: NodeImage objects in, node images on a target out.
package main

import (
	"context"
	"flag"
	"os"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	imagev1 "github.com/suse/elemental-lifecycle-manager/image-operator/api/v1alpha1"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/builder/pod"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/buildstatus"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/controller"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/recipe"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/release"
	"github.com/suse/elemental-lifecycle-manager/image-operator/internal/target/harvester"
)

func main() {
	var (
		namespace, probeAddr, metricsAddr string
		leaderElect                       bool
		opts                              controller.Options
		builder                           harvester.BuilderConfig
		defaultBuilder, podKubeconfig     string
		podPull, podService               string
		podCfg                            pod.Config
	)
	flag.StringVar(&namespace, "namespace", "",
		"Only watch NodeImage objects (and read credentials Secrets) in this namespace. Empty: all namespaces.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address of the health probes.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "Address of the metrics endpoint; 0 disables it.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Use leader election (needed with more than one replica).")
	flag.IntVar(&opts.MaxAttempts, "max-attempts", 2,
		"Failed builds or deliveries after which a request ends in Failed.")
	flag.DurationVar(&opts.BuildTimeout, "build-timeout", 60*time.Minute,
		"Deadline of one build, counted from the creation of the builder.")
	flag.DurationVar(&opts.DeliveryTimeout, "delivery-timeout", 20*time.Minute,
		"Deadline of one import on the target.")
	flag.DurationVar(&opts.PollBuild, "poll-build", 20*time.Second, "How often a running builder is looked at.")
	flag.DurationVar(&opts.PollDelivery, "poll-delivery", 5*time.Second, "How often a running import is looked at.")
	flag.StringVar(&builder.BaseImageURL, "builder-base-image-url",
		"https://download.opensuse.org/distribution/leap/16.0/appliances/Leap-16.0-Minimal-VM.x86_64-Cloud.qcow2",
		"Cloud image the builder VM boots (needs cloud-init and zypper).")
	flag.Int64Var(&builder.CPU, "builder-cpu", 8, "vCPUs of a builder VM.")
	flag.StringVar(&builder.Memory, "builder-memory", "12Gi", "Memory of a builder VM.")
	flag.StringVar(&builder.Disk, "builder-disk", "40Gi", "Disk of a builder VM.")
	flag.StringVar(&builder.ElementalRepo, "elemental-repo", "",
		"Git repository of the elemental3 source the recipe builds. Part of the cache key.")
	flag.StringVar(&builder.ElementalRef, "elemental-commit", "",
		"Commit of --elemental-repo, as a full SHA (a branch would make the cache key meaningless). Part of the cache key.")
	flag.StringVar(&defaultBuilder, "builder", string(imagev1.BuilderVM),
		"Where builds run when a request does not say: vm (a short-lived VM on the target) or pod "+
			"(needs --pod-builder-image).")
	flag.StringVar(&podCfg.Image, "pod-builder-image", "",
		"Image of builder pods: elemental3 at --elemental-commit plus the recipe's tools "+
			"(builder-image/Containerfile). Empty: no pod builder.")
	flag.StringVar(&podPull, "pod-builder-image-pull-policy", string(corev1.PullIfNotPresent),
		"Pull policy of the builder image.")
	flag.StringVar(&podKubeconfig, "pod-builder-kubeconfig", "",
		"File with a kubeconfig of the cluster builder pods run in. Empty: the cluster the operator runs in.")
	flag.StringVar(&podCfg.Namespace, "pod-builder-namespace", "",
		"Namespace of builder pods. Empty: the value of --namespace.")
	flag.StringVar(&podCfg.CPURequest, "pod-builder-cpu-request", "1", "CPU request of the build container.")
	flag.StringVar(&podCfg.CPULimit, "pod-builder-cpu-limit", "4", "CPU limit of the build container.")
	flag.StringVar(&podCfg.MemoryRequest, "pod-builder-memory-request", "2Gi", "Memory request of the build container.")
	flag.StringVar(&podCfg.MemoryLimit, "pod-builder-memory-limit", "4Gi", "Memory limit of the build container.")
	flag.StringVar(&podCfg.WorkSize, "pod-builder-work-size", "30Gi", "Size of the work volume of a builder pod.")
	flag.StringVar(&podCfg.WorkStorageClass, "pod-builder-work-storage-class", "",
		"Storage class of the work volume (an ephemeral claim, deleted with the pod). "+
			"Empty: an emptyDir on the node, limited to the size.")
	flag.BoolVar(&podCfg.HostUsers, "pod-builder-host-users", false,
		"Run builder pods in the node's user namespace. Default false: the pod gets its own, "+
			"and its root is an unprivileged user of the node.")
	flag.StringVar(&podService, "pod-builder-service-type", string(corev1.ServiceTypeClusterIP),
		"Service in front of a builder pod: ClusterIP, or NodePort when the target fetches the image "+
			"from outside the cluster.")
	flag.StringVar(&podCfg.DownloadHost, "pod-builder-download-host", "",
		"With NodePort: name or address at which the target reaches the node port.")
	zapOpts := zap.Options{}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
	log := ctrl.Log.WithName("setup")

	if opts.MaxAttempts < 1 || len(builder.ElementalRef) != 40 || builder.ElementalRepo == "" {
		log.Error(nil, "--max-attempts must be at least 1; "+
			"--elemental-repo and --elemental-commit (40 hex characters) are required")
		os.Exit(2)
	}
	recipeDigest, err := recipe.Digest()
	if err != nil {
		log.Error(err, "cannot read the embedded recipe")
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(imagev1.AddToScheme(scheme))
	mgrOpts := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "image-operator.image.lifecycle.suse.com",
	}
	if namespace != "" {
		mgrOpts.Cache = cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}}
	}
	restCfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(restCfg, mgrOpts)
	if err != nil {
		log.Error(err, "cannot start the manager")
		os.Exit(1)
	}
	var podBuilder *controller.PodBuilder
	if podCfg.Image != "" {
		podCfg.ImagePullPolicy, podCfg.ServiceType = corev1.PullPolicy(podPull), corev1.ServiceType(podService)
		podCfg.ElementalRepo, podCfg.ElementalRef = builder.ElementalRepo, builder.ElementalRef
		if podCfg.Namespace == "" {
			podCfg.Namespace = namespace
		}
		if podBuilder, err = newPodBuilder(restCfg, podKubeconfig, podCfg, scheme); err != nil {
			log.Error(err, "cannot set the pod builder up")
			os.Exit(2)
		}
	}
	if _, err := controller.ChooseBuilder("", imagev1.Builder(defaultBuilder), podBuilder != nil); err != nil {
		log.Error(err, "--builder")
		os.Exit(2)
	}
	r := &controller.NodeImageReconciler{
		Client: mgr.GetClient(),
		// The core/v1 recorder on purpose: the events.k8s.io one needs more RBAC for no gain here.
		Recorder: mgr.GetEventRecorderFor("image-operator"), //nolint:staticcheck // see above
		Resolver: release.Registry{},
		Backends: &controller.SecretBackends{
			Reader: mgr.GetAPIReader(), Builder: builder,
			DefaultBuilder: imagev1.Builder(defaultBuilder), Pod: podBuilder,
		},
		Tooling: controller.Tooling{
			RecipeDigest: recipeDigest, ToolRepo: builder.ElementalRepo, ToolCommit: builder.ElementalRef,
		},
		Options: opts,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		log.Error(err, "cannot set the controller up")
		os.Exit(1)
	}
	utilruntime.Must(mgr.AddHealthzCheck("healthz", healthz.Ping))
	utilruntime.Must(mgr.AddReadyzCheck("readyz", healthz.Ping))
	log.Info("starting", "namespace", namespace, "recipe", recipeDigest,
		"elementalRepo", builder.ElementalRepo, "elementalCommit", builder.ElementalRef,
		"builder", defaultBuilder, "podBuilder", podBuilder != nil)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager stopped")
		os.Exit(1)
	}
}

// newPodBuilder makes the clients of the cluster builder pods run in: the operator's own cluster,
// or the one a kubeconfig file names.
func newPodBuilder(
	own *rest.Config, kubeconfig string, cfg pod.Config, scheme *runtime.Scheme,
) (*controller.PodBuilder, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	restCfg := rest.CopyConfig(own)
	if kubeconfig != "" {
		var err error
		if restCfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig); err != nil {
			return nil, err
		}
	}
	restCfg.UserAgent = "elemental-image-operator"
	c, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	core, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, err
	}
	return &controller.PodBuilder{
		Client: c, Config: cfg,
		Fetch: func(ctx context.Context, namespace, service, path string) ([]byte, error) {
			port := strconv.Itoa(buildstatus.Port)
			return core.CoreV1().Services(namespace).ProxyGet("http", service, port, path, nil).DoRaw(ctx)
		},
	}, nil
}
