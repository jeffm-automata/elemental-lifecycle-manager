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
	"time"

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
	testK8sVersion          = "v1.30.0"
	testOldK8sVersion       = "v1.29.0"
	testCompleteMsg         = "Complete"
	testComponentsAvailable = "Components available"
)

var _ = Describe("KubernetesReconciler", func() {
	var (
		ctx                 context.Context
		reconciler          *reconcilers.KubernetesReconciler
		fakeClient          client.Client
		mockPlanRec         *testutil.MockPlanReconciler
		mockPackagedHandler *testutil.MockPackagedComponentsHandler
		scheme              *runtime.Scheme
		config              *upgrade.Config
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = testutil.NewTestScheme()
		fakeClient = testutil.NewFakeClient(scheme)
		mockPlanRec = testutil.NewMockPlanReconciler()
		mockPackagedHandler = testutil.NewMockPackagedComponentsHandler()
		reconciler = reconcilers.NewKubernetesReconciler(fakeClient, mockPlanRec, mockPackagedHandler)

		config = testutil.NewTestConfig(testutil.WithKubernetes("registry.example.com/k8s:v1.30.0", "v1.30.0"))
	})

	Describe("Phase", func() {
		It("should return PhaseKubernetes", func() {
			Expect(reconciler.Phase()).To(Equal(upgrade.PhaseKubernetes))
		})
	})

	Describe("Reconcile", func() {
		Context("when Kubernetes config is nil", func() {
			It("should skip the phase", func() {
				config.Kubernetes = nil

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status).NotTo(BeNil())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeSkipped))
				Expect(status.Message).To(Equal("Upgrade for phase \"Kubernetes\" skipped"))
			})
		})

		Context("snapshot generation", func() {
			BeforeEach(func() {
				node := testutil.NewTestNode("cp1", true, testutil.WithVersion("v1.30.0"))
				Expect(fakeClient.Create(ctx, node)).To(Succeed())
			})

			It("should generate snapshot before upgrading", func() {
				snapshotGenerated := false
				mockPackagedHandler.GenerateSnapshotFn = func(ctx context.Context, config *upgrade.Config) (*reconcilers.PackagedComponentsSnapshot, error) {
					snapshotGenerated = true
					return &reconcilers.PackagedComponentsSnapshot{
						CreationTime:            time.Now(),
						SourceKubernetesVersion: testK8sVersion,
						Charts:                  []*reconcilers.PackagedComponentChartSnapshot{},
					}, nil
				}

				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					return &reconcilers.PlanResult{
						Status: &upgrade.PhaseStatus{
							State:   lifecyclev1alpha1.PlanComplete,
							Message: testCompleteMsg,
						},
						Nodes: []corev1.Node{*testutil.NewTestNode("cp1", true, testutil.WithVersion("v1.30.0"))},
					}, nil
				}

				mockPackagedHandler.ReconcileAvailabilityFn = func(ctx context.Context, targetVersion string, snapshot *reconcilers.PackagedComponentsSnapshot) (*upgrade.PhaseStatus, error) {
					return &upgrade.PhaseStatus{
						State:   lifecyclev1alpha1.K8sPackagedComponentsAvailable,
						Message: testComponentsAvailable,
					}, nil
				}

				_, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(snapshotGenerated).To(BeTrue())
			})

			It("should return error if snapshot generation fails", func() {
				mockPackagedHandler.GenerateSnapshotFn = func(ctx context.Context, config *upgrade.Config) (*reconcilers.PackagedComponentsSnapshot, error) {
					return nil, fmt.Errorf("snapshot generation failed")
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("snapshot generation failed"))
				Expect(status).To(BeNil())
			})
		})

		Context("plan execution and node version verification", func() {
			var planNames []string

			BeforeEach(func() {
				planNames = []string{}

				mockPackagedHandler.GenerateSnapshotFn = func(ctx context.Context, config *upgrade.Config) (*reconcilers.PackagedComponentsSnapshot, error) {
					return &reconcilers.PackagedComponentsSnapshot{
						CreationTime:            time.Now(),
						SourceKubernetesVersion: testK8sVersion,
						Charts:                  []*reconcilers.PackagedComponentChartSnapshot{},
					}, nil
				}
			})

			It("should execute control-plane then worker plans in order", func() {
				mockPackagedHandler.ReconcileAvailabilityFn = func(ctx context.Context, targetVersion string, snapshot *reconcilers.PackagedComponentsSnapshot) (*upgrade.PhaseStatus, error) {
					return &upgrade.PhaseStatus{
						State:   lifecyclev1alpha1.K8sPackagedComponentsAvailable,
						Message: testComponentsAvailable,
					}, nil
				}

				// The cluster starts on an older version, the plans report the upgraded nodes.
				Expect(fakeClient.Create(ctx, testutil.NewTestNode("cp1", true, testutil.WithVersion(testOldK8sVersion)))).To(Succeed())
				Expect(fakeClient.Create(ctx, testutil.NewTestNode("worker1", false, testutil.WithVersion(testOldK8sVersion)))).To(Succeed())

				cp := testutil.NewTestNode("cp1", true, testutil.WithVersion("v1.30.0"))
				worker := testutil.NewTestNode("worker1", false, testutil.WithVersion("v1.30.0"))

				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					planNames = append(planNames, desired.Name)
					var nodes []corev1.Node
					if len(planNames) == 1 {
						nodes = []corev1.Node{*cp}
					} else {
						nodes = []corev1.Node{*worker}
					}
					return &reconcilers.PlanResult{
						Status: &upgrade.PhaseStatus{
							State:   lifecyclev1alpha1.PlanComplete,
							Message: testCompleteMsg,
						},
						Nodes: nodes,
					}, nil
				}

				_, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(planNames).To(HaveLen(2))
				Expect(planNames[0]).To(ContainSubstring("control-plane"))
				Expect(planNames[1]).To(ContainSubstring("worker"))
			})

			It("should apply the worker concurrency to the worker plan only", func() {
				config.Kubernetes.WorkerConcurrency = 2

				mockPackagedHandler.ReconcileAvailabilityFn = func(ctx context.Context, targetVersion string, snapshot *reconcilers.PackagedComponentsSnapshot) (*upgrade.PhaseStatus, error) {
					return &upgrade.PhaseStatus{
						State:   lifecyclev1alpha1.K8sPackagedComponentsAvailable,
						Message: testComponentsAvailable,
					}, nil
				}

				Expect(fakeClient.Create(ctx, testutil.NewTestNode("cp1", true, testutil.WithVersion(testOldK8sVersion)))).To(Succeed())
				Expect(fakeClient.Create(ctx, testutil.NewTestNode("worker1", false, testutil.WithVersion(testOldK8sVersion)))).To(Succeed())

				concurrency := map[string]int64{}
				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					concurrency[desired.Name] = desired.Spec.Concurrency
					return &reconcilers.PlanResult{
						Status: &upgrade.PhaseStatus{
							State:   lifecyclev1alpha1.PlanComplete,
							Message: testCompleteMsg,
						},
						Nodes: []corev1.Node{*testutil.NewTestNode("upgraded", true, testutil.WithVersion(testK8sVersion))},
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

			It("should wait for control-plane before starting worker", func() {
				mockPackagedHandler.ReconcileAvailabilityFn = func(ctx context.Context, targetVersion string, snapshot *reconcilers.PackagedComponentsSnapshot) (*upgrade.PhaseStatus, error) {
					return &upgrade.PhaseStatus{
						State:   lifecyclev1alpha1.K8sPackagedComponentsAvailable,
						Message: testComponentsAvailable,
					}, nil
				}

				cp := testutil.NewTestNode("cp1", true, testutil.WithVersion(testOldK8sVersion))
				Expect(fakeClient.Create(ctx, cp)).To(Succeed())

				worker := testutil.NewTestNode("worker1", false, testutil.WithVersion(testOldK8sVersion))
				Expect(fakeClient.Create(ctx, worker)).To(Succeed())

				callCount := 0
				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					callCount++
					if callCount == 1 {
						return &reconcilers.PlanResult{
							Status: &upgrade.PhaseStatus{
								State:   lifecyclev1alpha1.UpgradeInProgress,
								Message: "Control-plane in progress",
							},
							Nodes: []corev1.Node{},
						}, nil
					}
					Fail("Worker plan should not be called")
					return nil, nil
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
				Expect(callCount).To(Equal(1))
			})

			It("should verify all nodes are at target version", func() {
				node := testutil.NewTestNode("cp1", true, testutil.WithVersion("v1.30.0"))
				Expect(fakeClient.Create(ctx, node)).To(Succeed())

				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					return &reconcilers.PlanResult{
						Status: &upgrade.PhaseStatus{
							State:   lifecyclev1alpha1.PlanComplete,
							Message: testCompleteMsg,
						},
						Nodes: []corev1.Node{*node},
					}, nil
				}

				mockPackagedHandler.ReconcileAvailabilityFn = func(ctx context.Context, targetVersion string, snapshot *reconcilers.PackagedComponentsSnapshot) (*upgrade.PhaseStatus, error) {
					return &upgrade.PhaseStatus{
						State:   lifecyclev1alpha1.K8sPackagedComponentsAvailable,
						Message: testComponentsAvailable,
					}, nil
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeSucceeded))
			})

			It("should return in-progress when node version doesn't match", func() {
				node := testutil.NewTestNode("cp1", true, testutil.WithVersion("v1.29.0"))
				Expect(fakeClient.Create(ctx, node)).To(Succeed())

				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					return &reconcilers.PlanResult{
						Status: &upgrade.PhaseStatus{
							State:   lifecyclev1alpha1.PlanComplete,
							Message: testCompleteMsg,
						},
						Nodes: []corev1.Node{*node},
					}, nil
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
				Expect(status.Message).To(ContainSubstring("waiting for node upgrade verification"))
			})

			It("should return in-progress when node is not ready", func() {
				node := testutil.NewTestNode("cp1", true, testutil.WithVersion("v1.30.0"))
				// Make node not ready
				node.Status.Conditions = []corev1.NodeCondition{
					{
						Type:   corev1.NodeReady,
						Status: corev1.ConditionFalse,
					},
				}
				Expect(fakeClient.Create(ctx, node)).To(Succeed())

				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					return &reconcilers.PlanResult{
						Status: &upgrade.PhaseStatus{
							State:   lifecyclev1alpha1.PlanComplete,
							Message: testCompleteMsg,
						},
						Nodes: []corev1.Node{*node},
					}, nil
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
			})

			It("should return in-progress when node is unschedulable", func() {
				node := testutil.NewTestNode("cp1", true, testutil.WithVersion("v1.30.0"))
				node.Spec.Unschedulable = true
				Expect(fakeClient.Create(ctx, node)).To(Succeed())

				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					return &reconcilers.PlanResult{
						Status: &upgrade.PhaseStatus{
							State:   lifecyclev1alpha1.PlanComplete,
							Message: testCompleteMsg,
						},
						Nodes: []corev1.Node{*node},
					}, nil
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
			})
		})

		Context("with a worker plan whose worker nodes are gone", func() {
			var planNames []string

			BeforeEach(func() {
				planNames = []string{}

				mockPackagedHandler.GenerateSnapshotFn = func(ctx context.Context, config *upgrade.Config) (*reconcilers.PackagedComponentsSnapshot, error) {
					return &reconcilers.PackagedComponentsSnapshot{
						CreationTime:            time.Now(),
						SourceKubernetesVersion: testK8sVersion,
						Charts:                  []*reconcilers.PackagedComponentChartSnapshot{},
					}, nil
				}
				mockPackagedHandler.ReconcileAvailabilityFn = func(ctx context.Context, targetVersion string, snapshot *reconcilers.PackagedComponentsSnapshot) (*upgrade.PhaseStatus, error) {
					return &upgrade.PhaseStatus{
						State:   lifecyclev1alpha1.K8sPackagedComponentsAvailable,
						Message: testComponentsAvailable,
					}, nil
				}

				Expect(fakeClient.Create(ctx, testutil.NewTestNode("cp1", true, testutil.WithVersion(testK8sVersion)))).To(Succeed())
				workerPlan := plan.KubernetesWorker(config.ReleaseNamespacedName.Name, config.ReleaseVersion, config.Kubernetes.Version, false, 1)
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
						Status: &upgrade.PhaseStatus{State: lifecyclev1alpha1.PlanComplete, Message: testCompleteMsg},
						Nodes:  []corev1.Node{*testutil.NewTestNode("cp1", true, testutil.WithVersion(testK8sVersion))},
					}, nil
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(planNames).To(HaveLen(2))
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeFailed))
			})

			It("should succeed once the worker plan has completed without nodes", func() {
				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					planNames = append(planNames, desired.Name)
					result := &reconcilers.PlanResult{
						Status: &upgrade.PhaseStatus{State: lifecyclev1alpha1.PlanComplete, Message: testCompleteMsg},
					}
					if !strings.Contains(desired.Name, "worker") {
						result.Nodes = []corev1.Node{*testutil.NewTestNode("cp1", true, testutil.WithVersion(testK8sVersion))}
					}
					return result, nil
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(planNames).To(HaveLen(2))
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeSucceeded))
			})

			It("should still wait for the nodes of a completed control plane plan", func() {
				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					planNames = append(planNames, desired.Name)
					return &reconcilers.PlanResult{
						Status: &upgrade.PhaseStatus{State: lifecyclev1alpha1.PlanComplete, Message: testCompleteMsg},
					}, nil
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(planNames).To(HaveLen(1))
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
				Expect(status.Message).To(ContainSubstring("waiting for node upgrade verification"))
			})
		})

		Context("nodes already at the target version", func() {
			var planNames []string

			BeforeEach(func() {
				planNames = []string{}

				mockPackagedHandler.GenerateSnapshotFn = func(ctx context.Context, config *upgrade.Config) (*reconcilers.PackagedComponentsSnapshot, error) {
					return &reconcilers.PackagedComponentsSnapshot{
						CreationTime:            time.Now(),
						SourceKubernetesVersion: testK8sVersion,
						Charts:                  []*reconcilers.PackagedComponentChartSnapshot{},
					}, nil
				}

				mockPackagedHandler.ReconcileAvailabilityFn = func(ctx context.Context, targetVersion string, snapshot *reconcilers.PackagedComponentsSnapshot) (*upgrade.PhaseStatus, error) {
					return &upgrade.PhaseStatus{
						State:   lifecyclev1alpha1.K8sPackagedComponentsAvailable,
						Message: testComponentsAvailable,
					}, nil
				}

				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					planNames = append(planNames, desired.Name)

					return &reconcilers.PlanResult{
						Status: &upgrade.PhaseStatus{
							State:   lifecyclev1alpha1.PlanComplete,
							Message: testCompleteMsg,
						},
						Nodes: []corev1.Node{*testutil.NewTestNode("upgraded", true, testutil.WithVersion(testK8sVersion))},
					}, nil
				}
			})

			It("should not reconcile any plan when all nodes run the target version", func() {
				Expect(fakeClient.Create(ctx, testutil.NewTestNode("cp1", true, testutil.WithVersion(testK8sVersion)))).To(Succeed())
				Expect(fakeClient.Create(ctx, testutil.NewTestNode("worker1", false, testutil.WithVersion(testK8sVersion)))).To(Succeed())

				availabilityChecked := false
				mockPackagedHandler.ReconcileAvailabilityFn = func(ctx context.Context, targetVersion string, snapshot *reconcilers.PackagedComponentsSnapshot) (*upgrade.PhaseStatus, error) {
					availabilityChecked = true
					return &upgrade.PhaseStatus{
						State:   lifecyclev1alpha1.K8sPackagedComponentsAvailable,
						Message: testComponentsAvailable,
					}, nil
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeSucceeded))
				Expect(planNames).To(BeEmpty())
				Expect(availabilityChecked).To(BeTrue())
			})

			It("should match the target version regardless of the 'v' prefix", func() {
				config.Kubernetes.Version = "1.30.0"
				Expect(fakeClient.Create(ctx, testutil.NewTestNode("cp1", true, testutil.WithVersion(testK8sVersion)))).To(Succeed())

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeSucceeded))
				Expect(planNames).To(BeEmpty())
			})

			It("should reconcile all plans when a single node runs another version", func() {
				Expect(fakeClient.Create(ctx, testutil.NewTestNode("cp1", true, testutil.WithVersion(testK8sVersion)))).To(Succeed())
				Expect(fakeClient.Create(ctx, testutil.NewTestNode("worker1", false, testutil.WithVersion(testK8sVersion)))).To(Succeed())
				Expect(fakeClient.Create(ctx, testutil.NewTestNode("worker2", false, testutil.WithVersion(testOldK8sVersion)))).To(Succeed())

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeSucceeded))
				Expect(planNames).To(HaveLen(2))
				Expect(planNames[0]).To(ContainSubstring("control-plane"))
				Expect(planNames[1]).To(ContainSubstring("worker"))
			})

			It("should wait without plans for a node at the target version that is not ready", func() {
				node := testutil.NewTestNode("cp1", true, testutil.WithVersion(testK8sVersion))
				node.Status.Conditions = []corev1.NodeCondition{
					{
						Type:   corev1.NodeReady,
						Status: corev1.ConditionFalse,
					},
				}
				Expect(fakeClient.Create(ctx, node)).To(Succeed())

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
				Expect(status.Message).To(ContainSubstring("waiting for them to be ready and schedulable"))
				Expect(planNames).To(BeEmpty())
			})

			It("should wait without plans for a node at the target version that is still cordoned", func() {
				// The node cache can lag behind the OS plan that has just uncordoned its last node.
				Expect(fakeClient.Create(ctx, testutil.NewTestNode("cp1", true, testutil.WithVersion(testK8sVersion)))).To(Succeed())
				worker := testutil.NewTestNode("worker1", false, testutil.WithVersion(testK8sVersion))
				worker.Spec.Unschedulable = true
				Expect(fakeClient.Create(ctx, worker)).To(Succeed())

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
				Expect(planNames).To(BeEmpty())

				worker.Spec.Unschedulable = false
				Expect(fakeClient.Update(ctx, worker)).To(Succeed())

				status, err = reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeSucceeded))
				Expect(planNames).To(BeEmpty())
			})

			It("should keep reconciling plans that already exist", func() {
				Expect(fakeClient.Create(ctx, testutil.NewTestNode("cp1", true, testutil.WithVersion(testK8sVersion)))).To(Succeed())
				Expect(fakeClient.Create(ctx, testutil.NewTestNode("worker1", false, testutil.WithVersion(testK8sVersion)))).To(Succeed())

				existing := plan.KubernetesWorker(config.ReleaseNamespacedName.Name, config.ReleaseVersion, config.Kubernetes.Version, false, 1)
				Expect(fakeClient.Create(ctx, existing)).To(Succeed())

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeSucceeded))
				Expect(planNames).To(HaveLen(2))
			})
		})

		Context("packaged components availability", func() {
			BeforeEach(func() {
				node := testutil.NewTestNode("cp1", true, testutil.WithVersion("v1.30.0"))
				Expect(fakeClient.Create(ctx, node)).To(Succeed())

				mockPackagedHandler.GenerateSnapshotFn = func(ctx context.Context, config *upgrade.Config) (*reconcilers.PackagedComponentsSnapshot, error) {
					return &reconcilers.PackagedComponentsSnapshot{
						CreationTime:            time.Now(),
						SourceKubernetesVersion: testK8sVersion,
						Charts:                  []*reconcilers.PackagedComponentChartSnapshot{},
					}, nil
				}

				mockPlanRec.ReconcileFn = func(ctx context.Context, desired *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					return &reconcilers.PlanResult{
						Status: &upgrade.PhaseStatus{
							State:   lifecyclev1alpha1.PlanComplete,
							Message: testCompleteMsg,
						},
						Nodes: []corev1.Node{*testutil.NewTestNode("cp1", true, testutil.WithVersion("v1.30.0"))},
					}, nil
				}
			})

			It("should wait for packaged components to become available", func() {
				mockPackagedHandler.ReconcileAvailabilityFn = func(ctx context.Context, targetVersion string, snapshot *reconcilers.PackagedComponentsSnapshot) (*upgrade.PhaseStatus, error) {
					return &upgrade.PhaseStatus{
						State:   lifecyclev1alpha1.UpgradeInProgress,
						Message: "Waiting for components",
					}, nil
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
				Expect(status.Message).To(ContainSubstring("Waiting for components"))
			})

			It("should succeed when components are available", func() {
				mockPackagedHandler.ReconcileAvailabilityFn = func(ctx context.Context, targetVersion string, snapshot *reconcilers.PackagedComponentsSnapshot) (*upgrade.PhaseStatus, error) {
					return &upgrade.PhaseStatus{
						State:   lifecyclev1alpha1.K8sPackagedComponentsAvailable,
						Message: testComponentsAvailable,
					}, nil
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeSucceeded))
				Expect(status.Message).To(Equal("All nodes upgraded successfully"))
			})

			It("should return error if availability check fails", func() {
				mockPackagedHandler.ReconcileAvailabilityFn = func(ctx context.Context, targetVersion string, snapshot *reconcilers.PackagedComponentsSnapshot) (*upgrade.PhaseStatus, error) {
					return nil, fmt.Errorf("availability check failed")
				}

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("availability check failed"))
				Expect(status).To(BeNil())
			})
		})

		Context("with upgrade strategy Replace", func() {
			var osPlan *upgradecattlev1.Plan

			planNames := func() []string {
				plans := &upgradecattlev1.PlanList{}
				Expect(fakeClient.List(ctx, plans)).To(Succeed())
				names := make([]string, 0, len(plans.Items))
				for _, p := range plans.Items {
					names = append(names, p.Name)
				}
				return names
			}

			BeforeEach(func() {
				config.UpgradeStrategy = lifecyclev1alpha1.UpgradeStrategyReplace
				release := config.ReleaseNamespacedName.Name

				// Kubernetes plans of the target and of an older version, and an OS plan that is left alone.
				var err error
				osPlan, err = plan.OSControlPlane(release, "registry.example.com/os:v1.0.0", "v1.0.0", config.ReleaseVersion, false)
				Expect(err).NotTo(HaveOccurred())
				for _, p := range []*upgradecattlev1.Plan{
					plan.KubernetesControlPlane(release, config.ReleaseVersion, testK8sVersion, false),
					plan.KubernetesWorker(release, "v0.9.0", testOldK8sVersion, false, 1),
					osPlan,
				} {
					Expect(fakeClient.Create(ctx, p)).To(Succeed())
				}

				fail := fmt.Errorf("must not be called for the Replace strategy")
				mockPlanRec.ReconcileFn = func(context.Context, *upgradecattlev1.Plan) (*reconcilers.PlanResult, error) {
					return nil, fail
				}
				mockPackagedHandler.GenerateSnapshotFn = func(context.Context, *upgrade.Config) (*reconcilers.PackagedComponentsSnapshot, error) {
					return nil, fail
				}
			})

			It("should wait without nodes", func() {
				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
				Expect(status.Message).To(Equal("Upgrade strategy Replace: waiting for nodes to be replaced, 0 of 0 run v1.30.0"))
			})

			It("should count the replaced nodes, then succeed once all nodes run the target version", func() {
				Expect(fakeClient.Create(ctx, testutil.NewTestNode("cp1", true, testutil.WithVersion("1.30.0")))).To(Succeed())
				Expect(fakeClient.Create(ctx, testutil.NewTestNode("wk1", false, testutil.WithVersion(testK8sVersion)))).To(Succeed())
				stale := testutil.NewTestNode("wk2", false, testutil.WithVersion(testOldK8sVersion))
				Expect(fakeClient.Create(ctx, stale)).To(Succeed())

				status, err := reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
				Expect(status.Message).To(Equal("Upgrade strategy Replace: waiting for nodes to be replaced, 2 of 3 run v1.30.0"))
				Expect(planNames()).To(ConsistOf(osPlan.Name))

				Expect(fakeClient.Delete(ctx, stale)).To(Succeed())
				Expect(fakeClient.Create(ctx, testutil.NewTestNode("wk3", false, testutil.WithVersion(testK8sVersion)))).To(Succeed())

				status, err = reconciler.Reconcile(ctx, config)

				Expect(err).NotTo(HaveOccurred())
				Expect(status.State).To(Equal(lifecyclev1alpha1.UpgradeSucceeded))
				Expect(status.Message).To(Equal("All 3 nodes run v1.30.0 (replaced)"))
				Expect(planNames()).To(ConsistOf(osPlan.Name))
			})
		})
	})

})
