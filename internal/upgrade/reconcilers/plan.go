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
	"strings"

	upgradecattlev1 "github.com/rancher/system-upgrade-controller/pkg/apis/upgrade.cattle.io/v1"
	"github.com/rancher/wrangler/v3/pkg/genericcondition"
	lifecyclev1alpha1 "github.com/suse/elemental-lifecycle-manager/api/v1alpha1"
	"github.com/suse/elemental-lifecycle-manager/internal/upgrade"
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
}

func NewSUCPlanReconciler(c client.Client) *SUCPlanReconciler {
	return &SUCPlanReconciler{Client: c}
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
