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

// The release catalog controller: it queries the OCI repositories named by ReleaseSource objects
// and publishes the releases found there as AvailableRelease objects. It runs on any Kubernetes
// cluster and needs nothing but its two custom resource definitions.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	corev1 "k8s.io/api/core/v1"

	lifecyclev1alpha1 "github.com/suse/elemental-lifecycle-manager/api/v1alpha1"
	"github.com/suse/elemental-lifecycle-manager/internal/catalog"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(lifecyclev1alpha1.AddToScheme(scheme))
}

func main() {
	var probeAddr, metricsAddr, namespace string
	var reachableFrom, lineage, registry string
	var leaderElect bool
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to; 0 disables it.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election.")
	flag.StringVar(&namespace, "namespace", os.Getenv("POD_NAMESPACE"),
		"The namespace pull secrets are read from (default: POD_NAMESPACE).")
	flag.StringVar(&reachableFrom, "reachable-from", "",
		"Do not run the controller: print the catalog as seen from a cluster that runs this release version, and exit.")
	flag.StringVar(&lineage, "lineage", "", "With --reachable-from: the lineage of the cluster's release, if known.")
	flag.StringVar(&registry, "registry", "", "With --reachable-from: the repository the cluster's release came from.")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if reachableFrom != "" {
		if err := printReachable(catalog.Current{Version: reachableFrom, Lineage: lineage, Registry: registry}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if namespace == "" {
		setupLog.Error(nil, "no namespace: set --namespace or POD_NAMESPACE")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          leaderElect,
		LeaderElectionID:        "release-catalog.lifecycle.suse.com",
		LeaderElectionNamespace: namespace,
		// Secrets are read directly and only by name, in the controller's namespace: no cache,
		// so the controller needs no list or watch on Secrets.
		Client: client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}},
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	if err = (&catalog.SourceReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Registry:  catalog.RemoteRegistry{},
		Namespace: namespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "ReleaseSource")
		os.Exit(1)
	}

	if err = mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err = mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting release catalog", "namespace", namespace)
	if err = mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// printReachable lists the catalog with the verdict for a cluster on the given release.
func printReachable(current catalog.Current) error {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	list := &lifecyclev1alpha1.AvailableReleaseList{}
	if err = c.List(context.Background(), list); err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "VERSION\tLINEAGE\tKUBERNETES\tREGISTRY\tSTALE\tREACHABLE\tREASON")
	for _, cand := range catalog.Evaluate(current, list.Items) {
		s := cand.Release.Spec
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%t\t%t\t%s\n", s.Version, s.Lineage, s.Kubernetes.Version, s.Registry,
			cand.Release.Status.Stale, cand.Reach.Reachable, cand.Reach.Reason)
	}
	return w.Flush()
}
