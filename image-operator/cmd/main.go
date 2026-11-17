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
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	imagev1 "github.com/suse/elemental-lifecycle-manager/image-operator/api/v1alpha1"
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
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		log.Error(err, "cannot start the manager")
		os.Exit(1)
	}
	r := &controller.NodeImageReconciler{
		Client: mgr.GetClient(),
		// The core/v1 recorder on purpose: the events.k8s.io one needs more RBAC for no gain here.
		Recorder: mgr.GetEventRecorderFor("image-operator"), //nolint:staticcheck // see above
		Resolver: release.Registry{},
		Backends: &controller.SecretBackends{Reader: mgr.GetAPIReader(), Builder: builder},
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
		"elementalRepo", builder.ElementalRepo, "elementalCommit", builder.ElementalRef)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager stopped")
		os.Exit(1)
	}
}
