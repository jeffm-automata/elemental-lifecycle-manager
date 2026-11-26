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
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	upgradecattlev1 "github.com/rancher/system-upgrade-controller/pkg/apis/upgrade.cattle.io/v1"
	lifecyclev1alpha1 "github.com/suse/elemental-lifecycle-manager/api/v1alpha1"
	"github.com/suse/elemental-lifecycle-manager/internal/plan"
	"github.com/suse/elemental-lifecycle-manager/internal/upgrade"
	"github.com/suse/elemental-lifecycle-manager/internal/upgrade/reconcilers"
	"github.com/suse/elemental-lifecycle-manager/internal/upgrade/reconcilers/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	testPlanCompleted = "Plan completed"
	testComplete      = "Complete"
)

var _ = Describe("OSReconciler", func() {
	var (
		ctx              context.Context
		reconciler       *reconcilers.OSReconciler
		fakeClient       client.Client
		mockPlanRec      *testutil.MockPlanReconciler
		scheme           *runtime.Scheme
		config           *upgrade.Config
		planReconcileCnt int
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = testutil.NewTestScheme()
		fakeClient = testutil.NewFakeClient(scheme)
		mockPlanRec = testutil.NewMockPlanReconciler()
		reconciler = reconcilers.NewOSReconciler(fakeClient, mockPlanRec)
		planReconcileCnt = 0

		config = testutil.NewTestConfig(testutil.WithOS("registry.example.com/os:v1.0.0", "v1.0.0"))
	})

	Describe("Phase", func() {
		It("should return PhaseOS", func() {
			Expect(reconciler.Phase()).To(Equal(upgrade.PhaseOS))
		})
	})

	Describe("Reconcile", func() {
		Context("when OS config is nil", func() {
			It("should skip the phase", func() {
				config.OS = nil

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status).NotTo(BeNil())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeSkipped))
				Expect(status.Message).To(ContainSubstring("OS"))
			})
		})

		Context("with control-plane only cluster", func() {
			BeforeEach(func() {
				// Create only control-plane nodes
				node1 := testutil.NewTestNode("cp1", true)
				Expect(fakeClient.Create(ctx, node1)).To(Succeed())

				node2 := testutil.NewTestNode("cp2", true)
				Expect(fakeClient.Create(ctx, node2)).To(Succeed())

				// Mock plan reconciler to return complete status
				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					planReconcileCnt++
					return &reconcilers.PlanResult{
						Status: &upgrade.PhaseStatus{
							State:   lifecyclev1alpha1.PlanComplete,
							Message: testPlanCompleted,
						},
						Nodes: []corev1.Node{},
					}, nil
				}
			})

			It("should create only control-plane plan and succeed", func() {
				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status).NotTo(BeNil())
				Expect(planReconcileCnt).To(Equal(1)) // Only control-plane plan
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeSucceeded))
				Expect(status.Message).To(Equal("All nodes upgraded successfully"))
			})
		})

		Context("with a worker plan whose worker nodes are gone", func() {
			var planNames []string

			BeforeEach(func() {
				planNames = []string{}
				Expect(fakeClient.Create(ctx, testutil.NewTestNode("cp1", true))).To(Succeed())

				workerPlan, err := plan.OSWorker(config.ReleaseNamespacedName.Name, config.OS.Image, config.OS.Version, config.ReleaseVersion, false, 1)
				Expect(err).NotTo(HaveOccurred())
				Expect(fakeClient.Create(ctx, workerPlan)).To(Succeed())
			})

			It("should not report success while the worker plan has not completed", func() {
				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					planNames = append(planNames, desired.Name)
					if strings.Contains(desired.Name, "worker") {
						return &reconcilers.PlanResult{
							Status: &upgrade.PhaseStatus{
								State:   lifecyclev1alpha1.UpgradeFailed,
								Message: "Plan failed: Job failed on Node worker1: DeadlineExceeded",
							},
						}, nil
					}
					return &reconcilers.PlanResult{
						Status: &upgrade.PhaseStatus{State: lifecyclev1alpha1.PlanComplete, Message: testPlanCompleted},
					}, nil
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(planNames).To(HaveLen(2))
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeFailed))
				Expect(status.Message).To(ContainSubstring("worker1"))
			})

			It("should succeed once the worker plan has completed", func() {
				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					planNames = append(planNames, desired.Name)
					return &reconcilers.PlanResult{
						Status: &upgrade.PhaseStatus{State: lifecyclev1alpha1.PlanComplete, Message: testPlanCompleted},
					}, nil
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(planNames).To(HaveLen(2))
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeSucceeded))
			})
		})

		Context("with control-plane and worker nodes", func() {
			BeforeEach(func() {
				// Create control-plane nodes
				cp1 := testutil.NewTestNode("cp1", true)
				Expect(fakeClient.Create(ctx, cp1)).To(Succeed())

				// Create worker nodes
				worker1 := testutil.NewTestNode("worker1", false)
				Expect(fakeClient.Create(ctx, worker1)).To(Succeed())

				worker2 := testutil.NewTestNode("worker2", false)
				Expect(fakeClient.Create(ctx, worker2)).To(Succeed())
			})

			It("should wait for control-plane plan before starting worker plan", func() {
				callCount := 0
				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					callCount++
					if callCount == 1 {
						// First call (control-plane) returns in-progress
						return &reconcilers.PlanResult{
							Status: &upgrade.PhaseStatus{
								State:   lifecyclev1alpha1.UpgradeInProgress,
								Message: "Control-plane upgrade in progress",
							},
							Nodes: []corev1.Node{},
						}, nil
					}
					// Should not reach here in this test
					Fail("Worker plan should not be reconciled while control-plane is in progress")
					return nil, nil
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
				Expect(status.Message).To(ContainSubstring("Control-plane"))
				Expect(callCount).To(Equal(1)) // Only control-plane plan should be called
			})

			It("should proceed to worker plan after control-plane completes", func() {
				callCount := 0
				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					callCount++
					if callCount == 1 {
						// Control-plane plan completes
						return &reconcilers.PlanResult{
							Status: &upgrade.PhaseStatus{
								State:   lifecyclev1alpha1.PlanComplete,
								Message: "Control-plane complete",
							},
							Nodes: []corev1.Node{},
						}, nil
					}
					// Worker plan in progress
					return &reconcilers.PlanResult{
						Status: &upgrade.PhaseStatus{
							State:   lifecyclev1alpha1.UpgradeInProgress,
							Message: "Worker upgrade in progress",
						},
						Nodes: []corev1.Node{},
					}, nil
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
				Expect(status.Message).To(ContainSubstring("Worker"))
				Expect(callCount).To(Equal(2))
			})

			It("should apply the worker concurrency to the worker plan only", func() {
				config.OS.WorkerConcurrency = 2

				concurrency := map[string]int64{}
				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					concurrency[desired.Name] = desired.Spec.Concurrency
					return &reconcilers.PlanResult{
						Status: &upgrade.PhaseStatus{
							State:   lifecyclev1alpha1.PlanComplete,
							Message: testPlanCompleted,
						},
						Nodes: []corev1.Node{},
					}, nil
				}

				_, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(concurrency).To(HaveLen(2))
				for name, value := range concurrency {
					if strings.Contains(name, "worker") {
						Expect(value).To(Equal(int64(2)), name)
					} else {
						Expect(value).To(Equal(int64(1)), name)
					}
				}
			})

			It("should upgrade one worker at a time when no concurrency is configured", func() {
				concurrency := map[string]int64{}
				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					concurrency[desired.Name] = desired.Spec.Concurrency
					return &reconcilers.PlanResult{
						Status: &upgrade.PhaseStatus{
							State:   lifecyclev1alpha1.PlanComplete,
							Message: testPlanCompleted,
						},
						Nodes: []corev1.Node{},
					}, nil
				}

				_, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(concurrency).To(HaveLen(2))
				for name, value := range concurrency {
					Expect(value).To(Equal(int64(1)), name)
				}
			})

			It("should return succeeded when all plans complete", func() {
				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					return &reconcilers.PlanResult{
						Status: &upgrade.PhaseStatus{
							State:   lifecyclev1alpha1.PlanComplete,
							Message: testPlanCompleted,
						},
						Nodes: []corev1.Node{},
					}, nil
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeSucceeded))
				Expect(status.Message).To(ContainSubstring("All nodes upgraded successfully"))
			})
		})

		Context("with several control-plane nodes and worker nodes", func() {
			BeforeEach(func() {
				for _, name := range []string{"cp1", "cp2", "cp3"} {
					Expect(fakeClient.Create(ctx, testutil.NewTestNode(name, true))).To(Succeed())
				}
				for _, name := range []string{"worker1", "worker2"} {
					Expect(fakeClient.Create(ctx, testutil.NewTestNode(name, false))).To(Succeed())
				}
			})

			planStatus := func(state, message string) *reconcilers.PlanResult {
				return &reconcilers.PlanResult{
					Status: &upgrade.PhaseStatus{State: state, Message: message},
					Nodes:  []corev1.Node{},
				}
			}

			It("should start the worker plan while the control-plane plan is in progress", func() {
				var planNames []string
				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					planNames = append(planNames, desired.Name)
					if strings.Contains(desired.Name, "control-plane") {
						return planStatus(lifecyclev1alpha1.UpgradeInProgress, "Control-plane upgrade in progress"), nil
					}
					return planStatus(lifecyclev1alpha1.UpgradeInProgress, "Worker upgrade in progress"), nil
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(planNames).To(HaveLen(2))
				Expect(planNames[0]).To(ContainSubstring("control-plane"))
				Expect(planNames[1]).To(ContainSubstring("worker"))
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
				Expect(status.Message).To(Equal("Control-plane upgrade in progress; Worker upgrade in progress"))
			})

			It("should keep upgrading one control-plane node and one worker node at a time", func() {
				concurrency := map[string]int64{}
				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					concurrency[desired.Name] = desired.Spec.Concurrency
					return planStatus(lifecyclev1alpha1.UpgradeInProgress, "in progress"), nil
				}

				_, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(concurrency).To(HaveLen(2))
				for name, value := range concurrency {
					Expect(value).To(Equal(int64(1)), name)
				}
			})

			It("should not report success while only the worker plan has completed", func() {
				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					if strings.Contains(desired.Name, "control-plane") {
						return planStatus(lifecyclev1alpha1.UpgradeInProgress, "Control-plane upgrade in progress"), nil
					}
					return planStatus(lifecyclev1alpha1.PlanComplete, testPlanCompleted), nil
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
				Expect(status.Message).To(Equal("Control-plane upgrade in progress"))
			})

			It("should not report success while only the control-plane plan has completed", func() {
				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					if strings.Contains(desired.Name, "control-plane") {
						return planStatus(lifecyclev1alpha1.PlanComplete, testPlanCompleted), nil
					}
					return planStatus(lifecyclev1alpha1.UpgradeInProgress, "Worker upgrade in progress"), nil
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
				Expect(status.Message).To(Equal("Worker upgrade in progress"))
			})

			It("should report a failed plan while the other plan is in progress", func() {
				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					if strings.Contains(desired.Name, "control-plane") {
						return planStatus(lifecyclev1alpha1.UpgradeInProgress, "Control-plane upgrade in progress"), nil
					}
					return planStatus(lifecyclev1alpha1.UpgradeFailed, "Worker upgrade failed"), nil
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeFailed))
				Expect(status.Message).To(ContainSubstring("Control-plane upgrade in progress"))
				Expect(status.Message).To(ContainSubstring("Worker upgrade failed"))
			})

			It("should return succeeded when all plans complete", func() {
				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					return planStatus(lifecyclev1alpha1.PlanComplete, testPlanCompleted), nil
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeSucceeded))
			})
		})

		Context("when plan reconciliation fails", func() {
			BeforeEach(func() {
				node := testutil.NewTestNode("node1", true)
				Expect(fakeClient.Create(ctx, node)).To(Succeed())

				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					return nil, fmt.Errorf("plan reconciliation failed")
				}
			})

			It("should return the error", func() {
				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("plan reconciliation failed"))
				Expect(status).To(BeNil())
			})
		})

		Context("when plan returns failed status", func() {
			BeforeEach(func() {
				node := testutil.NewTestNode("node1", true)
				Expect(fakeClient.Create(ctx, node)).To(Succeed())

				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					return &reconcilers.PlanResult{
						Status: &upgrade.PhaseStatus{
							State:   lifecyclev1alpha1.UpgradeFailed,
							Message: "Upgrade script failed",
						},
						Nodes: []corev1.Node{},
					}, nil
				}
			})

			It("should return the failed status", func() {
				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeFailed))
				Expect(status.Message).To(ContainSubstring("Upgrade script failed"))
			})
		})

		Context("with upgrade strategy Replace", func() {
			It("should create no plan, delete the OS plans of the release and skip the phase", func() {
				config.UpgradeStrategy = lifecyclev1alpha1.UpgradeStrategyReplace
				release := config.ReleaseNamespacedName.Name
				Expect(fakeClient.Create(ctx, testutil.NewTestNode("cp1", true))).To(Succeed())

				// OS plans of the target and of an older version, a Kubernetes plan and a plan of another release.
				current, err := plan.OSControlPlane(release, config.OS.Image, config.OS.Version, config.ReleaseVersion, false)
				Expect(err).NotTo(HaveOccurred())
				old, err := plan.OSWorker(release, "registry.example.com/os:v0.9.0", "v0.9.0", "v0.9.0", false, 1)
				Expect(err).NotTo(HaveOccurred())
				k8s := plan.KubernetesControlPlane(release, config.ReleaseVersion, "v1.30.0", false)
				foreign, err := plan.OSControlPlane("other", config.OS.Image, "v0.8.0", "v0.8.0", false)
				Expect(err).NotTo(HaveOccurred())
				for _, p := range []*upgradecattlev1.Plan{current, old, k8s, foreign} {
					Expect(fakeClient.Create(ctx, p)).To(Succeed())
				}

				mockPlanRec.ReconcileFn = func(context.Context, *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					planReconcileCnt++
					return nil, fmt.Errorf("no plan must be reconciled")
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeSkipped))
				Expect(status.Message).To(Equal("Upgrade strategy Replace: the operating system comes with the node image of the release"))
				Expect(planReconcileCnt).To(BeZero())

				plans := &upgradecattlev1.PlanList{}
				Expect(fakeClient.List(ctx, plans)).To(Succeed())
				names := make([]string, 0, len(plans.Items))
				for _, p := range plans.Items {
					names = append(names, p.Name)
				}
				Expect(names).To(ConsistOf(k8s.Name, foreign.Name))
			})
		})
	})
})
