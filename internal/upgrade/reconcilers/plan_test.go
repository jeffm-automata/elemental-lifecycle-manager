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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	upgradecattlev1 "github.com/rancher/system-upgrade-controller/pkg/apis/upgrade.cattle.io/v1"
	"github.com/rancher/wrangler/v3/pkg/genericcondition"
	lifecyclev1alpha1 "github.com/suse/elemental-lifecycle-manager/api/v1alpha1"
	"github.com/suse/elemental-lifecycle-manager/internal/upgrade/reconcilers"
	"github.com/suse/elemental-lifecycle-manager/internal/upgrade/reconcilers/testutil"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// newPlanJob returns a job that SUC would have created to apply the given plan,
// in the given final state, or still running for an empty state.
func newPlanJob(plan *upgradecattlev1.Plan, name string, state batchv1.JobConditionType) *batchv1.Job {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apply-" + plan.Name + "-" + name,
			Namespace: plan.Namespace,
			Labels:    map[string]string{"upgrade.cattle.io/plan": plan.Name},
		},
	}
	if state != "" {
		job.Status.Conditions = []batchv1.JobCondition{{Type: state, Status: corev1.ConditionTrue}}
	}
	return job
}

const (
	testLabelKey    = "test"
	testLabelNode   = "node"
	testLabelRole   = "role"
	testLabelWorker = "worker"
)

var _ = Describe("SUCPlanReconciler", func() {
	var (
		ctx        context.Context
		reconciler *reconcilers.SUCPlanReconciler
		fakeClient client.Client
		scheme     *runtime.Scheme
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = testutil.NewTestScheme()
		fakeClient = testutil.NewFakeClient(scheme)
		reconciler = reconcilers.NewSUCPlanReconciler(fakeClient)
	})

	Describe("Reconcile", func() {
		var desiredPlan *upgradecattlev1.Plan

		BeforeEach(func() {
			desiredPlan = testutil.NewTestSUCPlan("test-plan", "default")
		})

		It("should create the plan and return in-progress status", func() {
			desiredPlan.Spec.NodeSelector.MatchLabels["key"] = "value"
			result, err := reconciler.Reconcile(ctx, desiredPlan)

			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())
			Expect(result.Status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
			Expect(result.Status.Message).To(ContainSubstring("test-plan"))
			Expect(result.Nodes).To(BeEmpty())

			// Verify plan was created in the cluster
			created := &upgradecattlev1.Plan{}
			err = fakeClient.Get(ctx, types.NamespacedName{
				Name:      desiredPlan.Name,
				Namespace: desiredPlan.Namespace,
			}, created)
			Expect(err).NotTo(HaveOccurred())
			Expect(created.Spec).To(Equal(desiredPlan.Spec))
		})

		It("should return in-progress status with applying nodes empty and then getting populated", func() {
			Expect(fakeClient.Create(ctx, desiredPlan)).To(Succeed())
			desiredPlan.Status.Applying = []string{}
			Expect(fakeClient.Update(ctx, desiredPlan)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, desiredPlan)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
			Expect(result.Status.Message).To(ContainSubstring("execution in progress"))

			// now populate the Applying field in Status
			desiredPlan.Status.Applying = []string{"node1", "node2"}
			Expect(fakeClient.Update(ctx, desiredPlan)).To(Succeed())

			result, err = reconciler.Reconcile(ctx, desiredPlan)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))

			Expect(result.Status.Message).To(SatisfyAll(
				ContainSubstring("applying on"),
				ContainSubstring("node1"),
				ContainSubstring("node2"),
			))
			Expect(result.Nodes).To(BeEmpty())
		})

		It("should return in-progress status", func() {
			desiredPlan.Status.Conditions = []genericcondition.GenericCondition{
				{
					Type:   string(upgradecattlev1.PlanComplete),
					Status: corev1.ConditionFalse,
					Reason: "",
				},
			}
			Expect(fakeClient.Create(ctx, desiredPlan)).To(Succeed())
			result, err := reconciler.Reconcile(ctx, desiredPlan)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
		})

		DescribeTable("should return in-progress status for a plan that SUC is still working on",
			func(reason, message string) {
				desiredPlan.Status.Conditions = []genericcondition.GenericCondition{
					{
						Type:    string(upgradecattlev1.PlanComplete),
						Status:  corev1.ConditionFalse,
						Reason:  reason,
						Message: message,
					},
				}
				Expect(fakeClient.Create(ctx, desiredPlan)).To(Succeed())
				result, err := reconciler.Reconcile(ctx, desiredPlan)

				Expect(err).NotTo(HaveOccurred())
				Expect(result.Status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
				Expect(result.Status.Message).NotTo(ContainSubstring("failed"))
			},
			Entry("whose version has just been resolved", "Resolved", ""),
			Entry("whose jobs have just been synced", "SyncJob", ""),
			Entry("that is not validated or resolved yet", "NotReady", "plan is not valid and resolved"),
			Entry("that waits for its time window", "Waiting", "plan is outside of set time window"),
			Entry("whose nodes could not be selected", "SelectNodesFailed", "cache is not ready"),
		)

		It("should return failed status with message", func() {
			desiredPlan.Status.Conditions = []genericcondition.GenericCondition{
				{
					Type:    string(upgradecattlev1.PlanComplete),
					Status:  corev1.ConditionFalse,
					Reason:  "JobFailed",
					Message: "upgrade script exited with code 1",
				},
			}
			Expect(fakeClient.Create(ctx, desiredPlan)).To(Succeed())
			Expect(fakeClient.Create(ctx, newPlanJob(desiredPlan, "failed", batchv1.JobFailed))).To(Succeed())
			result, err := reconciler.Reconcile(ctx, desiredPlan)

			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(lifecyclev1alpha1.UpgradeFailed))
			Expect(result.Status.Message).To(ContainSubstring("failed"))
			Expect(result.Status.Message).To(ContainSubstring("upgrade script exited with code 1"))
		})

		Context("when a job of the plan has failed", func() {
			const (
				retryPending     = ". Retry 1 of 3 follows once the failed job has been removed"
				failedNode       = "failed-node"
				jobFailedMessage = "Job cattle-system/apply-test-plan-on-failed-node failed on Node failed-node: DeadlineExceeded: Job was active longer than specified deadline"
			)

			var (
				failedJob *batchv1.Job
				now       time.Time
			)

			// planRetries returns the retries recorded on the plan, and whether one has been requested at all.
			planRetries := func() (string, bool) {
				stored := &upgradecattlev1.Plan{}
				Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(desiredPlan), stored)).To(Succeed())
				retries, ok := stored.Annotations[reconcilers.PlanRetriesAnnotation]
				return retries, ok
			}

			BeforeEach(func() {
				now = time.Date(2026, 11, 13, 9, 15, 0, 0, time.UTC)
				reconciler.Now = func() time.Time { return now }

				// SUC keeps the node of the failed job in the list of nodes the plan is applied on.
				desiredPlan.Status.Applying = []string{failedNode}
				desiredPlan.Status.Conditions = []genericcondition.GenericCondition{
					{
						Type:    string(upgradecattlev1.PlanComplete),
						Status:  corev1.ConditionFalse,
						Reason:  "JobFailed",
						Message: jobFailedMessage,
					},
				}
				Expect(fakeClient.Create(ctx, desiredPlan)).To(Succeed())

				// SUC keeps the failed job for some time as well.
				failedJob = newPlanJob(desiredPlan, "failed", batchv1.JobFailed)
				Expect(fakeClient.Create(ctx, failedJob)).To(Succeed())
			})

			It("should return failed status although the plan is still applying on the node", func() {
				node := testutil.NewTestNode(failedNode, false)
				Expect(fakeClient.Create(ctx, node)).To(Succeed())

				result, err := reconciler.Reconcile(ctx, desiredPlan)

				Expect(err).NotTo(HaveOccurred())
				Expect(result.Status.State).To(Equal(lifecyclev1alpha1.UpgradeFailed))
				Expect(result.Status.Message).To(Equal("Plan test-plan failed: " + jobFailedMessage + retryPending))
				Expect(result.Nodes).To(BeEmpty())
			})

			It("should report that the node of the failed job is still cordoned", func() {
				node := testutil.NewTestNode(failedNode, false)
				node.Spec.Unschedulable = true
				Expect(fakeClient.Create(ctx, node)).To(Succeed())
				Expect(fakeClient.Create(ctx, testutil.NewTestNode("node2", false))).To(Succeed())

				result, err := reconciler.Reconcile(ctx, desiredPlan)

				Expect(err).NotTo(HaveOccurred())
				Expect(result.Status.State).To(Equal(lifecyclev1alpha1.UpgradeFailed))
				Expect(result.Status.Message).To(Equal("Plan test-plan failed: " + jobFailedMessage + ". Still cordoned: " + failedNode + retryPending))
			})

			It("should find the node of the failed job by its hostname", func() {
				node := testutil.NewTestNode(failedNode+".example.com", false)
				node.Labels[corev1.LabelHostname] = failedNode
				node.Spec.Unschedulable = true
				Expect(fakeClient.Create(ctx, node)).To(Succeed())

				result, err := reconciler.Reconcile(ctx, desiredPlan)

				Expect(err).NotTo(HaveOccurred())
				Expect(result.Status.Message).To(ContainSubstring(". Still cordoned: " + failedNode))
			})

			It("should report that the node of the failed job is gone", func() {
				Expect(fakeClient.Create(ctx, testutil.NewTestNode("node2", false))).To(Succeed())

				result, err := reconciler.Reconcile(ctx, desiredPlan)

				Expect(err).NotTo(HaveOccurred())
				Expect(result.Status.State).To(Equal(lifecyclev1alpha1.UpgradeFailed))
				Expect(result.Status.Message).To(ContainSubstring(". No longer part of the cluster: " + failedNode))
			})

			It("should not request a retry while the failed job exists", func() {
				_, err := reconciler.Reconcile(ctx, desiredPlan)
				Expect(err).NotTo(HaveOccurred())

				_, requested := planRetries()
				Expect(requested).To(BeFalse())
			})

			It("should request a retry once the failed job has been removed", func() {
				Expect(fakeClient.Delete(ctx, failedJob)).To(Succeed())

				result, err := reconciler.Reconcile(ctx, desiredPlan)

				Expect(err).NotTo(HaveOccurred())
				Expect(result.Status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
				Expect(result.Status.Message).To(Equal("Retry 1 of 3 in progress after: Plan test-plan failed: " + jobFailedMessage + ". No longer part of the cluster: " + failedNode))

				retries, requested := planRetries()
				Expect(requested).To(BeTrue())
				Expect(retries).To(Equal("1"))

				stored := &upgradecattlev1.Plan{}
				Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(desiredPlan), stored)).To(Succeed())
				Expect(stored.Annotations).To(HaveKeyWithValue(reconcilers.PlanRetryRequestedAtAnnotation, "2026-11-13T09:15:00Z"))
			})

			It("should request a retry only once while SUC has not started its job yet", func() {
				Expect(fakeClient.Delete(ctx, failedJob)).To(Succeed())

				for range 3 {
					result, err := reconciler.Reconcile(ctx, desiredPlan)
					Expect(err).NotTo(HaveOccurred())
					Expect(result.Status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
					now = now.Add(30 * time.Second)
				}

				retries, _ := planRetries()
				Expect(retries).To(Equal("1"))
			})

			It("should report the retry as in progress while its job runs, however long that takes", func() {
				Expect(fakeClient.Delete(ctx, failedJob)).To(Succeed())
				_, err := reconciler.Reconcile(ctx, desiredPlan)
				Expect(err).NotTo(HaveOccurred())

				Expect(fakeClient.Create(ctx, newPlanJob(desiredPlan, "retry", ""))).To(Succeed())
				now = now.Add(40 * time.Minute)

				result, err := reconciler.Reconcile(ctx, desiredPlan)

				Expect(err).NotTo(HaveOccurred())
				Expect(result.Status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
				Expect(result.Status.Message).To(HavePrefix("Retry 1 of 3 in progress after: Plan test-plan failed: "))

				retries, _ := planRetries()
				Expect(retries).To(Equal("1"))
			})

			It("should request another retry when SUC did not start a job for the previous one", func() {
				Expect(fakeClient.Delete(ctx, failedJob)).To(Succeed())
				_, err := reconciler.Reconcile(ctx, desiredPlan)
				Expect(err).NotTo(HaveOccurred())

				now = now.Add(3 * time.Minute)
				result, err := reconciler.Reconcile(ctx, desiredPlan)

				Expect(err).NotTo(HaveOccurred())
				Expect(result.Status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
				retries, _ := planRetries()
				Expect(retries).To(Equal("2"))
			})

			It("should report the failure of a retry and retry again, up to the limit", func() {
				for retry := 1; retry <= reconcilers.MaxPlanRetries; retry++ {
					By(fmt.Sprintf("removing the failed job and requesting retry %d", retry))
					Expect(fakeClient.Delete(ctx, failedJob)).To(Succeed())
					result, err := reconciler.Reconcile(ctx, desiredPlan)
					Expect(err).NotTo(HaveOccurred())
					Expect(result.Status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
					Expect(result.Status.Message).To(HavePrefix(fmt.Sprintf("Retry %d of 3 in progress", retry)))

					By("failing the job of the retry")
					failedJob = newPlanJob(desiredPlan, fmt.Sprintf("failed-%d", retry), batchv1.JobFailed)
					Expect(fakeClient.Create(ctx, failedJob)).To(Succeed())
					now = now.Add(45 * time.Minute)

					result, err = reconciler.Reconcile(ctx, desiredPlan)
					Expect(err).NotTo(HaveOccurred())
					Expect(result.Status.State).To(Equal(lifecyclev1alpha1.UpgradeFailed))
				}

				By("giving up while the last failed job exists")
				result, err := reconciler.Reconcile(ctx, desiredPlan)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.Status.State).To(Equal(lifecyclev1alpha1.UpgradeFailed))
				Expect(result.Status.Message).To(HaveSuffix(". Giving up after 3 retries"))

				By("giving up after the last failed job has been removed")
				Expect(fakeClient.Delete(ctx, failedJob)).To(Succeed())
				now = now.Add(15 * time.Minute)
				result, err = reconciler.Reconcile(ctx, desiredPlan)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.Status.State).To(Equal(lifecyclev1alpha1.UpgradeFailed))
				Expect(result.Status.Message).To(HaveSuffix(". Giving up after 3 retries"))

				retries, _ := planRetries()
				Expect(retries).To(Equal("3"))

				By("retrying again after the record of the retries has been removed from the plan")
				stored := &upgradecattlev1.Plan{}
				Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(desiredPlan), stored)).To(Succeed())
				delete(stored.Annotations, reconcilers.PlanRetriesAnnotation)
				Expect(fakeClient.Update(ctx, stored)).To(Succeed())

				result, err = reconciler.Reconcile(ctx, desiredPlan)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.Status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
				Expect(result.Status.Message).To(HavePrefix("Retry 1 of 3 in progress"))
			})

			It("should ignore the jobs of other plans", func() {
				Expect(fakeClient.Delete(ctx, failedJob)).To(Succeed())
				other := testutil.NewTestSUCPlan("other-plan", "default")
				Expect(fakeClient.Create(ctx, newPlanJob(other, "failed", batchv1.JobFailed))).To(Succeed())

				result, err := reconciler.Reconcile(ctx, desiredPlan)

				Expect(err).NotTo(HaveOccurred())
				Expect(result.Status.State).To(Equal(lifecyclev1alpha1.UpgradeInProgress))
				retries, _ := planRetries()
				Expect(retries).To(Equal("1"))
			})
		})

		Context("when a plan that has been retried completes", func() {
			It("should forget the retries", func() {
				desiredPlan.Annotations = map[string]string{
					reconcilers.PlanRetriesAnnotation:          "2",
					reconcilers.PlanRetryRequestedAtAnnotation: "2026-11-13T09:15:00Z",
					"example.com/keep":                         "me",
				}
				desiredPlan.Status.Conditions = []genericcondition.GenericCondition{
					{
						Type:   string(upgradecattlev1.PlanComplete),
						Status: corev1.ConditionTrue,
						Reason: "Complete",
					},
				}
				Expect(fakeClient.Create(ctx, desiredPlan)).To(Succeed())

				result, err := reconciler.Reconcile(ctx, desiredPlan)

				Expect(err).NotTo(HaveOccurred())
				Expect(result.Status.State).To(Equal(lifecyclev1alpha1.PlanComplete))

				stored := &upgradecattlev1.Plan{}
				Expect(fakeClient.Get(ctx, client.ObjectKeyFromObject(desiredPlan), stored)).To(Succeed())
				Expect(stored.Annotations).To(Equal(map[string]string{"example.com/keep": "me"}))
			})
		})

		Context("when plan is complete", func() {
			BeforeEach(func() {
				desiredPlan.Status.Conditions = []genericcondition.GenericCondition{
					{
						Type:   string(upgradecattlev1.PlanComplete),
						Status: corev1.ConditionTrue,
					},
				}
				desiredPlan.Spec.NodeSelector = &metav1.LabelSelector{
					MatchLabels: map[string]string{testLabelKey: testLabelNode},
				}
				Expect(fakeClient.Create(ctx, desiredPlan)).To(Succeed())
			})

			It("should return complete status when no nodes match", func() {
				result, err := reconciler.Reconcile(ctx, desiredPlan)

				Expect(err).NotTo(HaveOccurred())
				Expect(result.Status.State).To(Equal(lifecyclev1alpha1.PlanComplete))
				Expect(result.Status.Message).To(Equal("Plan test-plan execution completed successfully"))
				Expect(result.Nodes).To(BeEmpty())
			})

			It("should list nodes matching the plan selector", func() {
				// Create matching nodes
				node1 := testutil.NewTestNode("node1", false)
				node1.Labels = map[string]string{testLabelKey: testLabelNode}
				Expect(fakeClient.Create(ctx, node1)).To(Succeed())

				node2 := testutil.NewTestNode("node2", false)
				node2.Labels = map[string]string{testLabelKey: testLabelNode}
				Expect(fakeClient.Create(ctx, node2)).To(Succeed())

				// Create non-matching node
				node3 := testutil.NewTestNode("node3", false)
				node3.Labels = map[string]string{"different": "label"}
				Expect(fakeClient.Create(ctx, node3)).To(Succeed())

				result, err := reconciler.Reconcile(ctx, desiredPlan)

				Expect(err).NotTo(HaveOccurred())
				Expect(result.Status.State).To(Equal(lifecyclev1alpha1.PlanComplete))
				Expect(result.Nodes).To(HaveLen(2))

				nodeNames := []string{result.Nodes[0].Name, result.Nodes[1].Name}
				Expect(nodeNames).To(ConsistOf("node1", "node2"))
			})
		})
	})
})
