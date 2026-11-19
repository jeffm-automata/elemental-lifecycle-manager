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

package reconcilers

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	upgradeapi "github.com/rancher/system-upgrade-controller/pkg/apis/upgrade.cattle.io"
	upgradecattlev1 "github.com/rancher/system-upgrade-controller/pkg/apis/upgrade.cattle.io/v1"
	"github.com/rancher/wrangler/v3/pkg/genericcondition"
	lifecyclev1alpha1 "github.com/suse/elemental-lifecycle-manager/api/v1alpha1"
	"github.com/suse/elemental-lifecycle-manager/internal/upgrade"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// planReasonJobFailed is the reason that SUC sets on the 'Complete' condition
// of a Plan when one of the jobs applying it has failed.
const planReasonJobFailed = "JobFailed"

const (
	// MaxPlanRetries is the number of times that a SUC Plan with a failed job is retried before it is given up.
	MaxPlanRetries = 3

	// PlanRetriesAnnotation holds the number of times that a SUC Plan has been retried after a failed job.
	// Removing the annotation from a Plan that has been given up makes it eligible for retries again.
	PlanRetriesAnnotation = "lifecycle.suse.com/retries"

	// PlanRetryRequestedAtAnnotation holds the time at which the last retry of a SUC Plan was requested.
	PlanRetryRequestedAtAnnotation = "lifecycle.suse.com/retry-requested-at"

	// planRetryGracePeriod is the time that SUC is given to start a job for a retry that has been requested.
	planRetryGracePeriod = 2 * time.Minute
)

// PlanResult provides information about the current status of the SUC Plan as seen
// in the upgrade process and the nodes that this plan relates to.
type PlanResult struct {
	Status *upgrade.PhaseStatus
	Nodes  []corev1.Node
}

// PlanReconciler ensures that a specific SUC Plan reflects the desired release state.
type PlanReconciler interface {
	Reconcile(ctx context.Context, desired *upgradecattlev1.Plan) (*PlanResult, error)
}

type SUCPlanReconciler struct {
	client.Client

	// Now returns the current time.
	Now func() time.Time
}

func NewSUCPlanReconciler(c client.Client) *SUCPlanReconciler {
	return &SUCPlanReconciler{Client: c, Now: time.Now}
}

// Reconcile attempts to retrieve the desired SUC plan, or create it if it is missing.
// It then provides a report on for the plan as seen in the upgrade process.
func (p *SUCPlanReconciler) Reconcile(ctx context.Context, desired *upgradecattlev1.Plan) (*PlanResult, error) {
	plan, err := p.getOrCreatePlan(ctx, desired)
	if err != nil {
		return nil, fmt.Errorf("parsing plan: %w", err)
	}

	result := &PlanResult{Status: parsePhaseStatusFromPlan(plan)}

	switch result.Status.State {
	case lifecyclev1alpha1.PlanComplete:
		if err := p.forgetRetries(ctx, plan); err != nil {
			return nil, fmt.Errorf("resetting retries of plan %q: %w", plan.Name, err)
		}

		nodes, err := p.listNodesForPlan(ctx, plan)
		if err != nil {
			return nil, fmt.Errorf("listing plan %q nodes: %w", plan.Name, err)
		}
		result.Nodes = nodes.Items
	case lifecyclev1alpha1.UpgradeFailed:
		// SUC cordons a node before the upgrade and only makes it schedulable again when the job succeeds,
		// so the nodes of a failed job stay cordoned. Say so, as nothing else on the cluster does.
		note, err := p.describeFailedNodes(ctx, plan)
		if err != nil {
			return nil, fmt.Errorf("inspecting nodes of failed plan %q: %w", plan.Name, err)
		}
		result.Status.Message += note

		if err := p.retryFailedPlan(ctx, plan, result.Status); err != nil {
			return nil, fmt.Errorf("retrying failed plan %q: %w", plan.Name, err)
		}
	}

	return result, nil
}

// getOrCreatePlan attempts to retrieve a SUC Plan, or create it if it is missing.
func (p *SUCPlanReconciler) getOrCreatePlan(ctx context.Context, desired *upgradecattlev1.Plan) (plan *upgradecattlev1.Plan, err error) {
	logger := log.FromContext(ctx)

	plan = &upgradecattlev1.Plan{}
	err = p.Get(ctx, types.NamespacedName{
		Name:      desired.Name,
		Namespace: desired.Namespace,
	}, plan)

	if apierrors.IsNotFound(err) {
		logger.Info("Creating SUC Plan", "name", desired.Name)
		if err = p.Create(ctx, desired); err != nil {
			return nil, err
		}
		return desired, nil
	}

	if err != nil {
		return nil, err
	}

	return plan, nil
}

// listNodesForPlan lists the nodes that the specified SUC Plan is responsible for.
func (p *SUCPlanReconciler) listNodesForPlan(ctx context.Context, plan *upgradecattlev1.Plan) (nodes *corev1.NodeList, err error) {
	selector, err := metav1.LabelSelectorAsSelector(plan.Spec.NodeSelector)
	if err != nil {
		return nil, fmt.Errorf("parsing node selector: %w", err)
	}

	nodes = &corev1.NodeList{}
	if err := p.List(ctx, nodes, client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, fmt.Errorf("listing nodes with selector %s: %w", selector, err)
	}

	return nodes, nil
}

// retryFailedPlan decides what happens to a SUC Plan with a failed job and updates the given status of the Plan accordingly.
//
// SUC does not start another job for the node of a failed job by itself: it only looks at a Plan again when the Plan
// changes. A retry is therefore requested by changing an annotation of the Plan, which also counts the retries. SUC keeps
// a failed job for some time before it removes it, and a new job cannot be created while the failed one exists,
// so the retry is requested once the failed job is gone. That leaves time to inspect the job and its pods.
//
// A Plan is retried MaxPlanRetries times. A job that failed for a passing reason, e.g. a node that took too long to reboot,
// succeeds on a retry. A job that keeps failing needs a person, and retrying it for ever would only hide that.
func (p *SUCPlanReconciler) retryFailedPlan(ctx context.Context, plan *upgradecattlev1.Plan, status *upgrade.PhaseStatus) error {
	jobs := &batchv1.JobList{}
	if err := p.List(ctx, jobs, client.InNamespace(plan.Namespace), client.MatchingLabels{upgradeapi.LabelPlan: plan.Name}); err != nil {
		return fmt.Errorf("listing jobs: %w", err)
	}

	var failedJobs, otherJobs int
	for i := range jobs.Items {
		if isJobFailed(&jobs.Items[i]) {
			failedJobs++
		} else {
			otherJobs++
		}
	}

	retries, _ := strconv.Atoi(plan.Annotations[PlanRetriesAnnotation])
	requestedAt, _ := time.Parse(time.RFC3339, plan.Annotations[PlanRetryRequestedAtAnnotation])
	requested := !requestedAt.IsZero() && p.Now().Sub(requestedAt) < planRetryGracePeriod

	switch {
	case otherJobs > 0 || (failedJobs == 0 && requested):
		// SUC only updates the Plan when the job of the retry finishes, until then the Plan shows the previous failure.
		status.State = lifecyclev1alpha1.UpgradeInProgress
		status.Message = fmt.Sprintf("Retry %d of %d in progress after: %s", retries, MaxPlanRetries, status.Message)
	case retries >= MaxPlanRetries:
		status.Message += fmt.Sprintf(". Giving up after %d retries", MaxPlanRetries)
	case failedJobs > 0:
		status.Message += fmt.Sprintf(". Retry %d of %d follows once the failed job has been removed", retries+1, MaxPlanRetries)
	default:
		retries++

		patch := client.MergeFrom(plan.DeepCopy())
		if plan.Annotations == nil {
			plan.Annotations = map[string]string{}
		}
		plan.Annotations[PlanRetriesAnnotation] = strconv.Itoa(retries)
		plan.Annotations[PlanRetryRequestedAtAnnotation] = p.Now().UTC().Format(time.RFC3339)
		if err := p.Patch(ctx, plan, patch); err != nil {
			return fmt.Errorf("requesting retry: %w", err)
		}

		log.FromContext(ctx).Info("Retrying SUC Plan after a failed job", "name", plan.Name, "retry", retries, "failure", status.Message)

		status.State = lifecyclev1alpha1.UpgradeInProgress
		status.Message = fmt.Sprintf("Retry %d of %d in progress after: %s", retries, MaxPlanRetries, status.Message)
	}

	return nil
}

// forgetRetries removes the record of the retries from a SUC Plan that has completed,
// so that a failure on a node that joins the cluster later is retried in full again.
func (p *SUCPlanReconciler) forgetRetries(ctx context.Context, plan *upgradecattlev1.Plan) error {
	_, retried := plan.Annotations[PlanRetriesAnnotation]
	_, requested := plan.Annotations[PlanRetryRequestedAtAnnotation]
	if !retried && !requested {
		return nil
	}

	patch := client.MergeFrom(plan.DeepCopy())
	delete(plan.Annotations, PlanRetriesAnnotation)
	delete(plan.Annotations, PlanRetryRequestedAtAnnotation)

	return p.Patch(ctx, plan, patch)
}

// isJobFailed returns true if the given job has failed.
func isJobFailed(job *batchv1.Job) bool {
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// describeFailedNodes describes the state in which the failed jobs of the given SUC Plan left their nodes.
func (p *SUCPlanReconciler) describeFailedNodes(ctx context.Context, plan *upgradecattlev1.Plan) (string, error) {
	if len(plan.Status.Applying) == 0 {
		return "", nil
	}

	nodes := &corev1.NodeList{}
	if err := p.List(ctx, nodes); err != nil {
		return "", fmt.Errorf("listing nodes: %w", err)
	}

	var cordoned, gone []string
	for _, applying := range plan.Status.Applying {
		// SUC lists the nodes that a Plan is applied on by their hostname.
		i := slices.IndexFunc(nodes.Items, func(node corev1.Node) bool {
			return node.Name == applying || node.Labels[corev1.LabelHostname] == applying
		})

		switch {
		case i < 0:
			gone = append(gone, applying)
		case nodes.Items[i].Spec.Unschedulable:
			cordoned = append(cordoned, applying)
		}
	}

	var note string
	if len(cordoned) > 0 {
		note += fmt.Sprintf(". Still cordoned: %s", strings.Join(cordoned, ", "))
	}
	if len(gone) > 0 {
		note += fmt.Sprintf(". No longer part of the cluster: %s", strings.Join(gone, ", "))
	}

	return note, nil
}

// parsePhaseStatusFromPlan parses the SUC Plan's status to a PhaseStatus.
func parsePhaseStatusFromPlan(p *upgradecattlev1.Plan) *upgrade.PhaseStatus {
	var complete *genericcondition.GenericCondition
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == string(upgradecattlev1.PlanComplete) {
			complete = &p.Status.Conditions[i]
			break
		}
	}

	// SUC also uses the reason of an incomplete Plan to tell how far it got with it,
	// e.g. "Resolved" or "SyncJob". Only a failed job is a failure of the Plan.
	//
	// SUC keeps the nodes of a failed job in the list of nodes that the Plan is applied on
	// and does not start another job for them, so the failure has to be looked at first.
	if complete != nil && complete.Status == corev1.ConditionFalse && complete.Reason == planReasonJobFailed {
		return &upgrade.PhaseStatus{
			State:   lifecyclev1alpha1.UpgradeFailed,
			Message: fmt.Sprintf("Plan %s failed: %s", p.Name, complete.Message),
		}
	}

	if len(p.Status.Applying) > 0 {
		return &upgrade.PhaseStatus{
			State:   lifecyclev1alpha1.UpgradeInProgress,
			Message: fmt.Sprintf("Plan %s is currently applying on: %s", p.Name, p.Status.Applying),
		}
	}

	if complete != nil && complete.Status == corev1.ConditionTrue {
		return &upgrade.PhaseStatus{
			State:   lifecyclev1alpha1.PlanComplete,
			Message: fmt.Sprintf("Plan %s execution completed successfully", p.Name),
		}
	}

	return &upgrade.PhaseStatus{
		State:   lifecyclev1alpha1.UpgradeInProgress,
		Message: fmt.Sprintf("Plan %s execution in progress", p.Name),
	}
}
