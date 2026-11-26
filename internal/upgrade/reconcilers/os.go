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
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	upgradecattlev1 "github.com/rancher/system-upgrade-controller/pkg/apis/upgrade.cattle.io/v1"
	lifecyclev1alpha1 "github.com/suse/elemental-lifecycle-manager/api/v1alpha1"
	"github.com/suse/elemental-lifecycle-manager/internal/plan"
	"github.com/suse/elemental-lifecycle-manager/internal/upgrade"
)

// OSReconciler ensures that the operating system of the cluster nodes reflects the desired release state.
type OSReconciler struct {
	client.Client
	sucReconciler PlanReconciler
}

func NewOSReconciler(c client.Client, sucReconciler PlanReconciler) *OSReconciler {
	return &OSReconciler{Client: c, sucReconciler: sucReconciler}
}

func (r *OSReconciler) Phase() upgrade.Phase {
	return upgrade.PhaseOS
}

func (r *OSReconciler) Reconcile(ctx context.Context, config *upgrade.Config) (*upgrade.PhaseStatus, error) {
	if config == nil || config.OS == nil {
		return r.Phase().SkippedStatus(), nil
	}

	if config.ReplaceNodes() {
		// The replacement nodes must not be upgraded in place by a Plan of this or of an older release.
		if err := deleteReleasePlans(ctx, r.Client, config.ReleaseNamespacedName.Name, plan.IsOS); err != nil {
			return nil, fmt.Errorf("deleting OS upgrade plans: %w", err)
		}

		return &upgrade.PhaseStatus{
			State:   lifecyclev1alpha1.UpgradeSkipped,
			Message: "Upgrade strategy Replace: the operating system comes with the node image of the release",
		}, nil
	}

	logger := log.FromContext(ctx)
	osConfig := config.OS
	logger.Info("Reconciling OS upgrade",
		"image", osConfig.Image,
		"version", osConfig.Version,
		"release", config.ReleaseNamespacedName.Name)

	// Let the nodes pull the OS image while they wait for their turn. The upgrade does not depend on the outcome.
	if err := r.ensurePrePull(ctx, config); err != nil {
		return nil, fmt.Errorf("pulling the OS image in advance: %w", err)
	}

	// prepare an ordered list of SUC Plans for the different node types of the cluster.
	plans, nodes, err := r.preparePlans(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("preparing OS upgrade plans: %w", err)
	}

	// The operating system of a worker node does not depend on that of the control plane nodes, so the
	// worker plan does not have to wait for the control plane plan when the control plane stays available.
	concurrent := hasHighlyAvailableControlPlane(nodes)

	// reconcile each plan in the prepared list.
	var pending *upgrade.PhaseStatus
	for _, p := range plans {
		result, err := r.sucReconciler.Reconcile(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("reconciling OS upgrade plan %q: %w", p.Name, err)
		}

		if result.Status.State != lifecyclev1alpha1.PlanComplete {
			// With a single control plane node the plans are reconciled in sequence: return the current status
			// of the first plan that is not in a 'Complete' status, so that no other plan is started before it.
			if !concurrent {
				return result.Status, nil
			}

			pending = combinePlanStatus(pending, result.Status)
			continue
		}

		logger.Info("OS upgrade plan completed",
			"plan", p.Name,
			"namespace", p.Namespace,
			"applied_on", getNodeNamesFromList(result.Nodes),
		)
	}

	if pending != nil {
		return pending, nil
	}

	return &upgrade.PhaseStatus{
		State:   lifecyclev1alpha1.UpgradeSucceeded,
		Message: "All nodes upgraded successfully",
	}, nil
}

// ensurePrePull makes sure that the SUC Plan which pulls the OS image on the nodes in advance exists.
//
// The Plan is created together with the first upgrade Plan and not again once the upgrade Plans exist, so that
// a Plan that someone removed stays removed. Its status is not looked at: a node on which the pull in advance
// did not work pulls the image in the prepare step of its upgrade job, as it would without this Plan.
func (r *OSReconciler) ensurePrePull(ctx context.Context, config *upgrade.Config) error {
	osConfig := config.OS
	releaseName := config.ReleaseNamespacedName.Name

	cpPlan, err := plan.OSControlPlane(releaseName, osConfig.Image, osConfig.Version, config.ReleaseVersion, osConfig.DrainOpts.ControlPlane)
	if err != nil {
		return fmt.Errorf("generating OS control-plane plan: %w", err)
	}

	err = r.Get(ctx, client.ObjectKeyFromObject(cpPlan), &upgradecattlev1.Plan{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("retrieving plan %q: %w", cpPlan.Name, err)
	}

	prePull := plan.OSPrePull(releaseName, osConfig.Image, osConfig.Version, config.ReleaseVersion)
	if err := r.Create(ctx, prePull); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating plan %q: %w", prePull.Name, err)
	}

	log.FromContext(ctx).Info("Pulling the OS image on all nodes in advance", "plan", prePull.Name, "image", osConfig.Image)

	return nil
}

// hasHighlyAvailableControlPlane reports whether the cluster has more than one control plane node.
//
// The control plane plan upgrades one node at a time, so the Kubernetes API of such a cluster stays
// available while a control plane node reboots. With a single control plane node the API is gone for the
// time of its reboot, and the drain of a worker node that needs the API at that moment would fail.
func hasHighlyAvailableControlPlane(nodes []corev1.Node) bool {
	var controlPlanes int
	for _, node := range nodes {
		if _, isControlPlane := node.Labels[plan.ControlPlaneLabel]; isControlPlane {
			controlPlanes++
		}
	}
	return controlPlanes > 1
}

// combinePlanStatus merges the status of a plan that has not completed into the status
// of the plans that have been looked at before it. A failed plan makes the combined status failed.
func combinePlanStatus(combined, status *upgrade.PhaseStatus) *upgrade.PhaseStatus {
	if combined == nil {
		return &upgrade.PhaseStatus{State: status.State, Message: status.Message}
	}

	if status.State == lifecyclev1alpha1.UpgradeFailed {
		combined.State = lifecyclev1alpha1.UpgradeFailed
	}
	combined.Message = fmt.Sprintf("%s; %s", combined.Message, status.Message)

	return combined
}

// preparePlans determines which types of SUC Plans does the cluster need and returns a list of ordered SUC plans ready for reconciliation,
// together with the nodes of the cluster. Control plane plans are always ordered before worker plans. With a single control plane node all
// control-plane nodes are upgraded before any worker upgrade operation starts, otherwise both node types are upgraded at the same time.
func (r *OSReconciler) preparePlans(ctx context.Context, config *upgrade.Config) ([]*upgradecattlev1.Plan, []corev1.Node, error) {
	osConfig := config.OS
	cpPlan, err := plan.OSControlPlane(config.ReleaseNamespacedName.Name, osConfig.Image, osConfig.Version, config.ReleaseVersion, osConfig.DrainOpts.ControlPlane)
	if err != nil {
		return nil, nil, fmt.Errorf("generating OS control-plane plan: %w", err)
	}

	planList := []*upgradecattlev1.Plan{cpPlan}

	allNodes := &corev1.NodeList{}
	if err := r.List(ctx, allNodes); err != nil {
		return nil, nil, fmt.Errorf("listing cluster nodes: %w", err)
	}

	wkPlan, err := plan.OSWorker(config.ReleaseNamespacedName.Name, osConfig.Image, osConfig.Version, config.ReleaseVersion, osConfig.DrainOpts.Worker, osConfig.WorkerConcurrency)
	if err != nil {
		return nil, nil, fmt.Errorf("generating OS worker plan: %w", err)
	}

	// A worker plan that has been created stays part of the phase when the cluster has no worker nodes
	// at the moment, e.g. because the only worker failed its upgrade and is being replaced. The phase
	// must not report success while that plan has not completed.
	include, err := workerPlanRequired(ctx, r.Client, wkPlan, allNodes.Items)
	if err != nil {
		return nil, nil, err
	}

	if include {
		planList = append(planList, wkPlan)
	}

	return planList, allNodes.Items, nil
}

// deleteReleasePlans deletes the SUC Plans of the given Release, of any version, whose name is matched.
func deleteReleasePlans(ctx context.Context, c client.Client, releaseName string, match func(planName string) bool) error {
	planList := &upgradecattlev1.PlanList{}
	if err := c.List(ctx, planList,
		client.InNamespace(plan.Namespace),
		client.MatchingLabels{lifecyclev1alpha1.ReleaseNameLabel: releaseName},
	); err != nil {
		return fmt.Errorf("listing plans: %w", err)
	}

	var deleteErrs []error
	for _, p := range planList.Items {
		if !match(p.Name) {
			continue
		}

		log.FromContext(ctx).Info("Deleting plan, the nodes are replaced instead of upgraded in place", "name", p.Name)
		if err := c.Delete(ctx, &p); client.IgnoreNotFound(err) != nil {
			deleteErrs = append(deleteErrs, fmt.Errorf("deleting plan %s: %w", p.Name, err))
		}
	}

	return errors.Join(deleteErrs...)
}

// workerPlanRequired reports whether the given worker plan is part of an upgrade phase. That is the case
// if the cluster has worker nodes, or if the plan has already been created for worker nodes that are gone since.
func workerPlanRequired(ctx context.Context, c client.Client, wkPlan *upgradecattlev1.Plan, nodes []corev1.Node) (bool, error) {
	if !isControlPlaneOnlyCluster(nodes) {
		return true, nil
	}

	err := c.Get(ctx, client.ObjectKeyFromObject(wkPlan), &upgradecattlev1.Plan{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("retrieving worker plan %q: %w", wkPlan.Name, err)
	}

	return true, nil
}

// isControlPlaneOnlyCluster returns true if all nodes in the cluster are control plane nodes.
func isControlPlaneOnlyCluster(nodes []corev1.Node) bool {
	for _, node := range nodes {
		if _, isControlPlane := node.Labels[plan.ControlPlaneLabel]; !isControlPlane {
			return false
		}
	}
	return true
}

func getNodeNamesFromList(nodes []corev1.Node) []string {
	nodeNames := make([]string, len(nodes))
	for i := range nodes {
		nodeNames[i] = nodes[i].Name
	}

	return nodeNames
}
