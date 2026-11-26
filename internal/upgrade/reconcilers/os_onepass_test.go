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
package reconcilers_test

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	upgradecattlev1 "github.com/rancher/system-upgrade-controller/pkg/apis/upgrade.cattle.io/v1"
	lifecyclev1alpha1 "github.com/suse/elemental-lifecycle-manager/api/v1alpha1"
	"github.com/suse/elemental-lifecycle-manager/internal/plan"
	"github.com/suse/elemental-lifecycle-manager/internal/upgrade"
	"github.com/suse/elemental-lifecycle-manager/internal/upgrade/reconcilers"
	"github.com/suse/elemental-lifecycle-manager/internal/upgrade/reconcilers/testutil"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("OSReconciler, one pass per node", func() {
	const (
		oldKubernetes = "v1.37.1+rke2r1"
		newKubernetes = "v1.37.2+rke2r1"
	)

	var (
		ctx         context.Context
		reconciler  *reconcilers.OSReconciler
		fakeClient  client.Client
		mockPlanRec *testutil.MockPlanReconciler
		components  *testutil.MockPackagedComponentsHandler
		config      *upgrade.Config
		plans       map[string]*upgradecattlev1.Plan
		order       []string
		states      map[string]string
		snapshots   int
	)

	// createNodes creates three control plane nodes and two worker nodes with the given kubelet versions.
	createNodes := func(controlPlaneVersion, workerVersion string) {
		for _, name := range []string{"cp1", "cp2", "cp3"} {
			Expect(fakeClient.Create(ctx, testutil.NewTestNode(name, true, testutil.WithVersion(controlPlaneVersion)))).To(Succeed())
		}
		for _, name := range []string{"worker1", "worker2"} {
			Expect(fakeClient.Create(ctx, testutil.NewTestNode(name, false, testutil.WithVersion(workerVersion)))).To(Succeed())
		}
	}

	isOnePass := func(p *upgradecattlev1.Plan) bool {
		return p != nil && p.Annotations[plan.OnePassAnnotation] != ""
	}

	BeforeEach(func() {
		ctx = context.Background()
		fakeClient = testutil.NewFakeClient(testutil.NewTestScheme())
		mockPlanRec = testutil.NewMockPlanReconciler()
		components = testutil.NewMockPackagedComponentsHandler()
		reconciler = reconcilers.NewOSReconciler(fakeClient, mockPlanRec)
		reconciler.PackagedComponents = components

		config = testutil.NewTestConfig(
			testutil.WithOS("registry.example.com/os:v1.0.0", "v1.0.0"),
			testutil.WithKubernetes("registry.example.com/rke2:v1.37.2", newKubernetes),
		)
		config.OnePass = true

		plans = map[string]*upgradecattlev1.Plan{}
		order = nil
		states = map[string]string{}
		snapshots = 0

		components.GenerateSnapshotFn = func(_ context.Context, _ *upgrade.Config) (*reconcilers.PackagedComponentsSnapshot, error) {
			// The snapshot has to exist before the first plan is handed to SUC.
			Expect(order).To(BeEmpty())
			snapshots++
			return &reconcilers.PackagedComponentsSnapshot{}, nil
		}

		mockPlanRec.ReconcileFn = func(_ context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
			plans[desired.Name] = desired
			order = append(order, desired.Name)

			kind := "control-plane"
			if strings.Contains(desired.Name, "worker") {
				kind = "worker"
			}

			state := lifecyclev1alpha1.UpgradeInProgress
			if states[kind] == testComplete {
				state = lifecyclev1alpha1.PlanComplete
			}
			return &reconcilers.PlanResult{Status: &upgrade.PhaseStatus{State: state, Message: desired.Name}}, nil
		}
	})

	Context("when the release changes the Kubernetes version", func() {
		BeforeEach(func() {
			createNodes(oldKubernetes, oldKubernetes)
		})

		It("upgrades the control plane nodes in one pass and holds the worker plan back, also with several control plane nodes", func() {
			status, err := reconciler.Reconcile(ctx, config)
			Expect(err).NotTo(HaveOccurred())
			Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))

			Expect(order).To(Equal([]string{"elemental-os-control-plane-v1-0-0"}))
			cp := plans["elemental-os-control-plane-v1-0-0"]
			Expect(isOnePass(cp)).To(BeTrue())
			Expect(cp.Annotations).To(HaveKeyWithValue(plan.OnePassAnnotation, newKubernetes))
			Expect(cp.Spec.Prepare.Image).To(Equal("rancher/rke2-upgrade:v1.37.2-rke2r1"))
			Expect(cp.Spec.Concurrency).To(Equal(int64(1)))
		})

		It("takes the packaged components snapshot before any node is upgraded", func() {
			_, err := reconciler.Reconcile(ctx, config)
			Expect(err).NotTo(HaveOccurred())
			Expect(snapshots).To(Equal(1))
		})

		It("starts the worker plan, in one pass as well, when the control plane plan has completed", func() {
			states["control-plane"] = testComplete
			for _, name := range []string{"cp1", "cp2", "cp3"} {
				Expect(fakeClient.Delete(ctx, testutil.NewTestNode(name, true))).To(Succeed())
				Expect(fakeClient.Create(ctx, testutil.NewTestNode(name, true, testutil.WithVersion(newKubernetes)))).To(Succeed())
			}

			status, err := reconciler.Reconcile(ctx, config)
			Expect(err).NotTo(HaveOccurred())
			Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))

			Expect(order).To(Equal([]string{"elemental-os-control-plane-v1-0-0", "elemental-os-worker-v1-0-0"}))
			worker := plans["elemental-os-worker-v1-0-0"]
			Expect(isOnePass(worker)).To(BeTrue())
			Expect(worker.Spec.Concurrency).To(Equal(int64(1)))
			Expect(worker.Spec.Prepare.Args[0]).To(ContainSubstring("get nodes --selector='node-role.kubernetes.io/control-plane'"))
		})

		It("still pulls the OS image and the image of the Kubernetes upgrade on all nodes in advance", func() {
			_, err := reconciler.Reconcile(ctx, config)
			Expect(err).NotTo(HaveOccurred())

			prePull := &upgradecattlev1.Plan{}
			Expect(fakeClient.Get(ctx, client.ObjectKey{Namespace: plan.Namespace, Name: "elemental-os-prepull-v1-0-0"}, prePull)).To(Succeed())
			Expect(prePull.Spec.Prepare.Image).To(Equal("rancher/rke2-upgrade:v1.37.2-rke2r1"))
			Expect(prePull.Spec.Upgrade.Image).To(Equal(config.OS.Image))
		})

		It("succeeds when both plans have completed", func() {
			states["control-plane"] = testComplete
			states["worker"] = testComplete

			status, err := reconciler.Reconcile(ctx, config)
			Expect(err).NotTo(HaveOccurred())
			Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeSucceeded))
		})
	})

	Context("when all nodes run the Kubernetes version of the release", func() {
		BeforeEach(func() {
			createNodes(newKubernetes, newKubernetes)
		})

		It("creates the plans of an OS only upgrade, both at the same time", func() {
			_, err := reconciler.Reconcile(ctx, config)
			Expect(err).NotTo(HaveOccurred())

			Expect(order).To(Equal([]string{"elemental-os-control-plane-v1-0-0", "elemental-os-worker-v1-0-0"}))
			Expect(isOnePass(plans["elemental-os-control-plane-v1-0-0"])).To(BeFalse())
			Expect(isOnePass(plans["elemental-os-worker-v1-0-0"])).To(BeFalse())
			Expect(plans["elemental-os-control-plane-v1-0-0"].Spec.Prepare.Image).To(Equal(config.OS.Image))
			Expect(snapshots).To(BeZero())
		})
	})

	Context("without the option", func() {
		BeforeEach(func() {
			config.OnePass = false
			createNodes(oldKubernetes, oldKubernetes)
		})

		It("creates exactly the plans it created before, both at the same time", func() {
			_, err := reconciler.Reconcile(ctx, config)
			Expect(err).NotTo(HaveOccurred())

			Expect(order).To(Equal([]string{"elemental-os-control-plane-v1-0-0", "elemental-os-worker-v1-0-0"}))

			wantCP, err := plan.OSControlPlane(config.ReleaseNamespacedName.Name, config.OS.Image, config.OS.Version, config.ReleaseVersion, false)
			Expect(err).NotTo(HaveOccurred())
			wantWorker, err := plan.OSWorker(config.ReleaseNamespacedName.Name, config.OS.Image, config.OS.Version, config.ReleaseVersion, false, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(plans["elemental-os-control-plane-v1-0-0"]).To(Equal(wantCP))
			Expect(plans["elemental-os-worker-v1-0-0"]).To(Equal(wantWorker))
			Expect(snapshots).To(BeZero())
		})
	})

	Context("with upgrade strategy Replace", func() {
		It("creates no plan", func() {
			config.UpgradeStrategy = lifecyclev1alpha1.UpgradeStrategyReplace
			createNodes(oldKubernetes, oldKubernetes)

			status, err := reconciler.Reconcile(ctx, config)
			Expect(err).NotTo(HaveOccurred())
			Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeSkipped))
			Expect(order).To(BeEmpty())
			Expect(snapshots).To(BeZero())
		})
	})
})
